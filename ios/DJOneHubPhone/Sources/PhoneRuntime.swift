import AVFAudio
import CallKit
import DJOneHubPhoneCore
import OSLog
import Network
import PushKit
import SwiftUI

@MainActor
final class PhoneRuntime: NSObject, ObservableObject, @preconcurrency CXProviderDelegate,
  @preconcurrency PKPushRegistryDelegate
{
  static let shared = PhoneRuntime()
  @Published private(set) var pushEnabled = UserDefaults.standard.bool(forKey: "voipPushEnabled")
  @Published private(set) var pushStatus = "后台来电未启用"
  private var pushRegistry: PKPushRegistry?
  private var pushToken: String?
  private var pushTask: Task<Void, Never>?
  private var wake: PushInvite?
  private var wakeReported: Bool?
  private var wakeRejected = false
  private var wakeTimeout: Task<Void, Never>?
  @Published var status = "未连接"
  @Published var paired = false
  @Published private(set) var identityReady = false
  @Published private(set) var foregroundPaused = false
  private var foregroundIntent = ForegroundConnectionIntent()
  private var foregroundConnectTask: Task<Void, Never>?
  private var connecting = false
  var microphoneGranted: Bool { AVAudioApplication.shared.recordPermission == .granted }
  var microphoneDenied: Bool { AVAudioApplication.shared.recordPermission == .denied }
  var canChangeForegroundConnection: Bool {
    identityReady && !working && callUUID == nil && (online || (pushTask == nil && !identity.operationInProgress))
  }
  private var foregroundConnectionBusy: Bool {
    working || identity.operationInProgress || pushTask != nil || socket != nil || callUUID != nil || wake != nil || reconnectTask != nil
  }
  func foregrounded() {
    foregroundIntent.activate()
    requestForegroundConnection()
  }
  private func requestForegroundConnection() {
    guard foregroundConnectTask == nil,
      foregroundIntent.eligible(ready: identityReady, microphoneGranted: microphoneGranted, busy: foregroundConnectionBusy)
    else { return }
    let generation = foregroundIntent.generation
    foregroundConnectTask = Task { [weak self] in
      // Let launch-time PushKit delivery reserve its call before an idle connection.
      try? await Task.sleep(for: .milliseconds(200))
      guard let self else { return }
      defer {
        self.foregroundConnectTask = nil
        if self.foregroundIntent.generation != generation { self.requestForegroundConnection() }
      }
      guard self.foregroundIntent.claim(generation: generation, ready: self.identityReady,
        microphoneGranted: self.microphoneGranted, busy: self.foregroundConnectionBusy) else { return }
      await self.connect()
    }
  }
  func pauseForegroundConnection() {
    guard !working, callUUID == nil else { return }
    foregroundIntent.pause()
    foregroundPaused = true
    disconnect("前台连接已暂停；后台来电按开关设置接收")
  }
  func retryForegroundConnection() async {
    guard canChangeForegroundConnection else { return }
    foregroundIntent.resume()
    foregroundPaused = false
    await connect()
  }
  @Published var online = false
  @Published var working = false
  @Published var installation = ""
  @Published private(set) var callDiagnostics: [String] = []
  private let logger = Logger(subsystem: "org.djonehub.phone", category: "call")
  private let vault = KeychainVault()
  private lazy var identity = PhoneIdentity(vault: vault, http: RelayHTTP())
  @Published var fingerprint = ""
  @Published private(set) var dialingSupported = false
  @Published private(set) var modules: [String] = []
  @Published private(set) var moduleNames: [String: String] = [:]
  private var moduleNamesTask: Task<Void, Never>?
  private static let moduleNamesCacheKey = "moduleNamesCache"
  func moduleName(for id: String) -> String {
    ModuleLineDisplay.name(for: id, names: moduleNames, modules: modules)
  }
  private func restoreModuleNames(_ record: StoredIdentity?) {
    guard let record, let principal = record.tokens?.principal,
      let data = UserDefaults.standard.data(forKey: Self.moduleNamesCacheKey),
      let names = try? ModuleNameCache.restore(data, relay: record.relay, principal: principal)
    else { moduleNames = [:]; return }
    moduleNames = names
  }
  private func syncModuleNames(connection id: UUID, credentials: StoredIdentity) {
    moduleNamesTask?.cancel()
    moduleNamesTask = Task { [weak self] in
      while !Task.isCancelled {
        guard let self, self.connectionID == id else { return }
        do {
          let names = try await self.identity.moduleNames()
          guard !Task.isCancelled, self.connectionID == id, let principal = credentials.tokens?.principal else { return }
          self.moduleNames = names
          self.refreshIncomingCallerName()
          let cache = ModuleNameCache(relay: credentials.relay, principal: principal, names: names)
          if let data = try? JSONEncoder().encode(cache) {
            UserDefaults.standard.set(data, forKey: Self.moduleNamesCacheKey)
          }
        } catch { /* Old or temporarily unavailable Relays keep the last known names. */ }
        do { try await Task.sleep(for: .seconds(15)) } catch { return }
      }
    }
  }
  @Published var selectedModule = ""
  @Published var dialNumber = ""
  @Published private(set) var outgoing = false
  private var pendingDial: (uuid: UUID, module: String, number: String)?
  private var signalingEpoch = ""
  private var activatedSession: AVAudioSession?
  @Published private(set) var callUUID: UUID?
  private var incomingReportedUUID: UUID?
  @Published private(set) var callDisplay = ""
  @Published private(set) var callModule = ""
  @Published private(set) var callPhase = ""
  @Published private(set) var muted = false
  @Published private(set) var speaker = false
  @Published private(set) var callControlPending = false
  @Published private(set) var connectedAt: Date?
  @Published private(set) var lastCallResult = ""
  @Published private(set) var history = CallHistory()
  @Published private(set) var historyError = ""
  private var historyWritable = true
  private var historyURL: URL {
    FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0]
      .appendingPathComponent("call-history.json")
  }
  private func saveHistory() {
    guard historyWritable else { return }
    do {
      try FileManager.default.createDirectory(at: historyURL.deletingLastPathComponent(), withIntermediateDirectories: true)
      try JSONEncoder().encode(history).write(to: historyURL, options: [.atomic, .completeFileProtectionUntilFirstUserAuthentication])
      var url = historyURL
      var values = URLResourceValues()
      values.isExcludedFromBackup = true
      try url.setResourceValues(values)
      historyError = ""
    } catch { historyError = "本机通话记录保存失败" }
  }
  private func finishHistory(_ result: String) {
    guard let id = callUUID else { return }
    history.finish(id: id, result: result)
    saveHistory()
  }
  func clearHistory() { history.clearCompleted(); historyWritable = true; saveHistory() }
  private let controller = CXCallController()
  private let provider: CXProvider
  private var socket: SignalSocket?
  private var coordinator: PhoneCall?
  private var media: PhoneMedia?
  private var earlyICE: [[String: Scalar]] = []
  private var answerAction: CXAnswerCallAction?
  private var activeAudioBinding: DJOneHubPhoneCore.Binding?
  private var reader: Task<Void, Never>?
  private var watchdog: Task<Void, Never>?
  private var expiry = Date.distantPast
  private var turnExpiry: Date?
  private var renewRequest: String?
  private var renewalStarted: Date?
  private var pingStarted: Date?
  private var lastPing = Date.distantPast
  private var connectionID = UUID()
  private var relayRecovery = false
  private var callRecovery = false
  private var signalRecovery = SignalRecoveryWindow()
  private var reconnectTask: Task<Void, Never>?
  private var recoveryTimeout: Task<Void, Never>?
  private var recoveryCommand: Signal?
  private var latestAudioState: Signal?
  private var latestMuteState: Signal?
  private var dtmfAction: (id: String, action: CXPlayDTMFCallAction)?
  @Published private(set) var callControlsSupported = false
  @Published private(set) var dtmfPending = false
  private var networkMonitor: NWPathMonitor?
  private var networkPath: String?
  private var lastControlSync = Date.distantPast
  override init() {
    let c = CXProviderConfiguration()
    c.supportsVideo = false
    c.maximumCallGroups = 1
    c.maximumCallsPerCallGroup = 1
    c.supportedHandleTypes = [.generic, .phoneNumber]
    c.includesCallsInRecents = false
    provider = CXProvider(configuration: c)
    super.init()
    provider.setDelegate(self, queue: .main)
    reloadStatus()
    if FileManager.default.fileExists(atPath: historyURL.path) {
      do {
        let file = try FileHandle(forReadingFrom: historyURL)
        defer { try? file.close() }
        history = try CallHistory.restore(try file.read(upToCount: 262145) ?? Data())
        saveHistory()
      } catch {
        historyWritable = false
        historyError = "本机通话记录无法读取，已停止覆盖旧文件；清除记录后可重新保存"
      }
    }
  }
  func startPushIfEnabled() {
    guard pushEnabled, pushRegistry == nil else { return }
    let registry = PKPushRegistry(queue: .main)
    registry.delegate = self
    pushRegistry = registry
    registry.desiredPushTypes = [.voIP]
    pushStatus = "正在注册后台来电"
  }
  func enablePush() async {
    guard paired, !working, callUUID == nil else { return }
    guard await AVAudioApplication.requestRecordPermission() else {
      pushStatus = "请先授予麦克风权限"
      return
    }
    pushEnabled = true
    UserDefaults.standard.set(true, forKey: "voipPushEnabled")
    startPushIfEnabled()
    syncPushRegistration()
    requestForegroundConnection()
  }
  func disablePush() async {
    guard !working, callUUID == nil else { return }
    working = true
    pushTask?.cancel()
    if let task = pushTask { await task.value }
    pushTask = nil
    defer { working = false; requestForegroundConnection() }
    do {
      try await identity.registerPush(environment: pushEnvironment, token: "")
      pushEnabled = false
      UserDefaults.standard.set(false, forKey: "voipPushEnabled")
      pushRegistry?.desiredPushTypes = []
      pushRegistry = nil
      pushToken = nil
      pushStatus = "后台来电已关闭"
    } catch { pushStatus = "无法关闭服务端推送，请重试或在 Hub 撤销安装身份" }
  }
  private var pushEnvironment: String {
    Bundle.main.object(forInfoDictionaryKey: "DJOneHubAPNSEnvironment") as? String ?? ""
  }
  private func syncPushRegistration() {
    guard pushEnabled, paired, !working, let token = pushToken, pushTask == nil else { return }
    pushTask = Task { [weak self] in
      guard let self else { return }
      defer {
        self.pushTask = nil
        self.requestForegroundConnection()
        if self.pushToken != token { self.syncPushRegistration() }
      }
      for attempt in 0..<3 {
        guard !Task.isCancelled, self.pushToken == token, self.pushEnabled else { return }
        do {
          try await self.identity.registerPush(environment: self.pushEnvironment, token: token)
          guard !Task.isCancelled, self.pushToken == token else { return }
          self.pushStatus = token.isEmpty ? "推送凭据已失效，请重新启用" : "后台来电已注册（需要网络与系统推送服务）"
          return
        } catch {
          self.pushStatus = "后台来电注册失败，请检查签名和 Relay APNs 配置"
          if attempt < 2 { try? await Task.sleep(for: .seconds(attempt + 1)) }
        }
      }
    }
  }
  func pushRegistry(
    _ registry: PKPushRegistry, didUpdate pushCredentials: PKPushCredentials,
    for type: PKPushType
  ) {
    guard type == .voIP else { return }
    pushToken = pushCredentials.token.map { String(format: "%02x", $0) }.joined()
    syncPushRegistration()
  }
  func pushRegistry(_ registry: PKPushRegistry, didInvalidatePushTokenFor type: PKPushType) {
    guard type == .voIP else { return }
    pushToken = ""
    syncPushRegistration()
  }
  private func clearWake() {
    wakeTimeout?.cancel()
    wakeTimeout = nil
    wake = nil
    wakeReported = nil
    wakeRejected = false
  }
  private func refreshIncomingCallerName() {
    guard !outgoing, let uuid = callUUID, uuid == incomingReportedUUID,
      callPhase != "ending", !callModule.isEmpty else { return }
    let update = CXCallUpdate()
    update.localizedCallerName = IncomingCaller(display: callDisplay)
      .localizedName(moduleName: moduleName(for: callModule))
    // Only change presentation; preserve the handle and negotiated controls.
    provider.reportCall(with: uuid, updated: update)
  }
  private func completeWakeHandoff() {
    guard let wake, coordinator?.binding == wake.binding, let reported = wakeReported else {
      return
    }
    let binding = wake.binding
    let rejected = wakeRejected
    if reported && !rejected { refreshIncomingCallerName() }
    clearWake()
    if rejected {
      coordinator?.requestEnd(binding)
      return
    }
    coordinator?.reported(binding, success: reported)
    if reported, answerAction != nil { coordinator?.requestAnswer(binding) }
  }
  func pushRegistry(
    _ registry: PKPushRegistry, didReceiveIncomingPushWith payload: PKPushPayload,
    for type: PKPushType, completion: @escaping () -> Void
  ) {
    guard type == .voIP else {
      completion()
      return
    }
    let hint = (try? JSONSerialization.data(withJSONObject: payload.dictionaryPayload))
      .flatMap { try? PushInvite.decode($0) }
    let caller = IncomingCaller(display: hint?.signal.payload["display"]?.string)
    let uuid = hint.flatMap { UUID(uuidString: $0.binding.callUUID) } ?? UUID()
    let duplicate =
      hint.map { coordinator?.binding == $0.binding || wake?.binding == $0.binding } ?? false
    let canStart = !duplicate && wake == nil && coordinator?.binding == nil && (!working || connecting)
    if canStart, let hint {
      wake = hint
      wakeReported = nil
      callUUID = uuid
      callDisplay = caller.display
      callModule = hint.binding.moduleID
      callPhase = "incoming"
    }
    let update = incomingCallUpdate(display: caller.display,
      moduleName: hint.map { moduleName(for: $0.binding.moduleID) })
    // Report synchronously in the PushKit callback, before Keychain, HTTP, WSS,
    // or stale-call checks. Even malformed/expired hints must be reported then ended.
    provider.reportNewIncomingCall(with: uuid, update: update) { [weak self] error in
      completion()
      Task { @MainActor in
        guard let self else { return }
        if duplicate { return }
        guard canStart, let hint, self.wake?.binding == hint.binding else {
          if error == nil { self.provider.reportCall(with: uuid, endedAt: Date(), reason: .failed) }
          return
        }
        self.wakeReported = error == nil
        if error != nil && !self.wakeRejected {
          self.disconnect("系统未允许后台来电")
          return
        }
        do {
          guard self.pushEnabled, let principal = try self.identity.status()?.tokens?.principal
          else {
            throw PhoneError.denied
          }
          try hint.validate(principal: principal)
          if error == nil {
            self.incomingReportedUUID = uuid
            self.refreshIncomingCallerName()
          }
          self.history.begin(id: uuid, module: hint.binding.moduleID, display: caller.display, outgoing: false)
          self.saveHistory()
        } catch {
          self.disconnect("后台来电已失效")
          return
        }
        self.wakeTimeout = Task { [weak self] in
          let seconds = max(0, min(12, hint.deadline.timeIntervalSinceNow))
          try? await Task.sleep(for: .seconds(seconds))
          guard !Task.isCancelled, self?.wake?.binding == hint.binding else { return }
          self?.disconnect("后台来电恢复超时")
        }
        if self.coordinator?.binding == hint.binding {
          self.completeWakeHandoff()
        } else if self.socket == nil {
          await self.connect(fromPush: true)
        }
      }
    }
  }
  func reloadStatus() {
    do {
      let record = try identity.status()
      paired = record != nil
      identityReady = record?.phase == "ready" && record?.tokens != nil
      installation = record?.tokens?.principal.id ?? ""
      modules = record?.tokens?.principal.modules ?? []
      restoreModuleNames(record)
      if !modules.contains(selectedModule) { selectedModule = modules.first ?? "" }
      fingerprint = try identity.fingerprint() ?? ""
      if let r = record, r.phase != "ready" { status = "配对或凭据更新未完成，请先在 Hub 撤销此安装身份，再清理本机凭据重新配对。" }
    } catch { identityReady = false; status = "无法读取本机凭据" }
  }
  func forgetLocalIdentity() {
    guard !working, socket == nil else { return }
    do {
      pushTask?.cancel()
      pushTask = nil
      pushRegistry?.desiredPushTypes = []
      pushRegistry = nil
      pushEnabled = false
      pushToken = nil
      UserDefaults.standard.set(false, forKey: "voipPushEnabled")
      try vault.remove()
      UserDefaults.standard.removeObject(forKey: Self.moduleNamesCacheKey)
      reloadStatus()
      status = "本机凭据已清理，可重新导入配对文件"
    } catch { status = "无法清理本机凭据" }
  }
  func importPairing(_ url: URL) async {
    guard !working, !paired, !online, callUUID == nil else { return }
    let allowed = url.startAccessingSecurityScopedResource()
    defer { if allowed { url.stopAccessingSecurityScopedResource() } }
    do {
      let file = try FileHandle(forReadingFrom: url)
      defer { try? file.close() }
      await pair(data: try file.read(upToCount: 8193) ?? Data())
    } catch { status = "无法读取配对文件" }
  }
  func pair(data: Data) async {
    guard !working, !paired, !online, callUUID == nil else { return }
    working = true
    defer { working = false; reloadStatus(); syncPushRegistration(); requestForegroundConnection() }
    do {
      try await identity.pair(PairingFile.parse(data))
      status = "配对完成"
    } catch { status = "配对失败；请检查配对有效期与本机凭据状态" }
  }
  func connect(fromPush: Bool = false) async {
    guard !working, socket == nil else { return }
    working = true
    connecting = true
    if !fromPush { foregroundIntent.manualAttempt() }
    let attempt = connectionID
    let wakeBinding = wake?.binding
    defer {
      connecting = false
      working = false
      syncPushRegistration()
      requestForegroundConnection()
    }
    let permitted =
      fromPush
      ? AVAudioApplication.shared.recordPermission == .granted
      : await AVAudioApplication.requestRecordPermission()
    guard permitted else {
      if fromPush || wake != nil { disconnect("后台接听需要先在前台授予麦克风权限") }
      status = "需要麦克风权限才能接听"
      return
    }
    do {
      let credentials = try await identity.credentials()
      guard connectionID == attempt, !fromPush || wake?.binding == wakeBinding else { return }
      guard let tokens = credentials.tokens else { throw PhoneError.invalidData }
      if let wake { try wake.validate(principal: tokens.principal) }
      let c = try SignalSocket(identity: credentials)
      coordinator = PhoneCall(principal: tokens.principal) { [weak self] effect in self?.handle(effect) }
      installSocket(c, credentials: credentials, recovering: false)

    } catch {
      reloadStatus()
      if fromPush || wake != nil { disconnect("后台来电连接失败") }
      else if identityReady { status = "连接失败；可点击重新连接，或检查 Relay 和配对状态" }
    }
  }
  private func installSocket(_ c: SignalSocket, credentials: StoredIdentity,
    recovering: Bool, readyWelcome: Signal? = nil) {
    guard let tokens = credentials.tokens else { c.close(); disconnect("凭据无效"); return }
    reader?.cancel()
    watchdog?.cancel()
    moduleNamesTask?.cancel()
    moduleNamesTask = nil
    renewRequest = nil
    renewalStarted = nil
    pingStarted = nil
    lastPing = .distantPast
    connectionID = UUID()
    let id = connectionID
    socket = c
    status = recovering ? "正在恢复通话连接" : "正在连接"
    c.onFailure = { [weak self] in
      guard let self, self.connectionID == id else { return }
      self.recoverSignal("信令发送失败")
    }
    expiry = Date().addingTimeInterval(5)  // welcome deadline
    watchdog = Task { [weak self] in
      while !Task.isCancelled {
        try? await Task.sleep(for: .seconds(1))
        guard let self, self.connectionID == id, !Task.isCancelled else { return }
        self.checkHealth(id)
      }
    }
    reader = Task { [weak self] in
      guard let self else { return }
      do {
        let welcome: Signal
        if let readyWelcome { welcome = readyWelcome } else { welcome = try await c.receive() }
        guard self.connectionID == id else { return }
        let exp = try welcome.welcomeExpiry(principal: tokens.principal)
        guard (welcome.payload["resumed"]?.bool == true) == recovering else { throw PhoneError.invalidData }
        if recovering { guard welcome.hub_epoch == self.signalingEpoch else { throw PhoneError.invalidData } }
        self.relayRecovery = welcome.payload["recovery"]?.bool == true
        self.expiry = exp
        self.signalingEpoch = welcome.hub_epoch!
        self.online = true
        self.syncModuleNames(connection: id, credentials: credentials)
        self.status = recovering ? "正在核对通话状态" : "在线，等待来电"
        Task { [weak self] in
          guard let origin = try? Wire.origin(credentials.relay) else { return }
          let supported = await RelayHTTP().supportsDialing(origin: origin)
          guard let self, self.connectionID == id else { return }
          self.dialingSupported = supported
        }
        while !Task.isCancelled {
          let message = try await c.receive()
          guard self.connectionID == id else { return }
          if message.type == "reauthenticated" {
            guard let requestID = self.renewRequest else { throw PhoneError.invalidData }
            let exp = try message.renewalExpiry(requestID: requestID)
            self.expiry = exp
            self.renewRequest = nil
            self.renewalStarted = nil
          } else {
            if message.type != "ice" && message.type != "invite" {
              let known = ["offer", "restart_offer", "state", "result", "error", "peer_closed", "peer_reconnecting", "peer_resumed", "dial_accepted"]
                .contains(
                  message.type)
              self.recordDiagnostic(known ? "received_" + message.type : "received_unknown")
            }
            if message.type == "invite", let wake = self.wake, !wake.matches(message) {
              self.disconnect("推送来电已失效")
              return
            }
            try self.receiveCallSignal(message, principal: tokens.principal)
          }
        }
      } catch {
        if self.connectionID == id {
          self.recordDiagnostic("signaling_receive_" + phoneFailureCode(error))
          if let failure = error as? PhoneError, case .invalidData = failure {
            self.disconnect("信令数据无效")
          } else { self.recoverSignal("连接已断开，请重新连接") }
        }
      }
    }
  }
  private func watchCallNetwork() {
    guard callRecovery, networkMonitor == nil else { return }
    let monitor = NWPathMonitor()
    networkMonitor = monitor
    networkPath = nil
    monitor.pathUpdateHandler = { [weak self, weak monitor] path in
      guard let monitor else { return }
      let signature = "\(path.status)-\(path.usesInterfaceType(.wifi))-\(path.usesInterfaceType(.cellular))-\(path.usesInterfaceType(.wiredEthernet))"
      Task { @MainActor [weak self] in
        guard let self, self.networkMonitor === monitor, self.connectedAt != nil else { return }
        let previous = self.networkPath
        self.networkPath = signature
        if let previous, previous != signature { self.recoverSignal("通话网络已变化") }
      }
    }
    monitor.start(queue: DispatchQueue(label: "org.djonehub.phone.network"))
  }
  private func beginRecovery() throws {
    guard relayRecovery, callRecovery, connectedAt != nil, let binding = coordinator?.binding else {
      throw PhoneError.unavailable
    }
    try signalRecovery.begin(binding)
    status = "正在恢复通话连接"
    if let command = recoveryCommand { media?.sendControl(command) }
    if recoveryTimeout == nil {
      recoveryTimeout = Task { [weak self] in
        for _ in 0..<10 {
          try? await Task.sleep(for: .seconds(1))
          guard !Task.isCancelled, let self, self.signalRecovery.binding == binding else { return }
          if Date().timeIntervalSince(self.lastControlSync) >= 2 {
            self.lastControlSync = Date()
            self.media?.sendControl(Signal(type: "sync", binding: binding, payload: [:]))
          }
        }
        guard !Task.isCancelled, let self, self.signalRecovery.binding == binding else { return }
        self.disconnect("通话连接恢复超时")
      }
    }
  }
  private func recoverSignal(_ reason: String) {
    do { try beginRecovery() } catch { disconnect(reason); return }
    guard reconnectTask == nil, let binding = signalRecovery.binding else { return }
    recordDiagnostic("signal_reconnecting")
    connectionID = UUID()
    let id = connectionID
    reader?.cancel()
    watchdog?.cancel()
    socket?.close()
    socket = nil
    online = false
    reconnectTask = Task { [weak self] in
      guard let self else { return }
      defer { if self.coordinator?.binding == binding { self.reconnectTask = nil } }
      while !Task.isCancelled, self.connectionID == id, self.coordinator?.binding == binding {
        var candidate: SignalSocket?
        do {
          try await Task.sleep(for: .milliseconds(250))
          let credentials = try await self.identity.credentials()
          guard !Task.isCancelled, self.connectionID == id,
            self.coordinator?.binding == binding, let tokens = credentials.tokens else { return }
          let c = try SignalSocket(identity: credentials, resume: binding)
          candidate = c
          // A task cancellation must also cancel URLSession's outstanding receive.
          let welcome = try await withTaskCancellationHandler {
            try await c.receive()
          } onCancel: { Task { @MainActor in c.close() } }
          _ = try welcome.welcomeExpiry(principal: tokens.principal)
          guard !Task.isCancelled, self.connectionID == id, self.coordinator?.binding == binding else { c.close(); return }
          guard welcome.payload["recovery"]?.bool == true,
            welcome.payload["resumed"]?.bool == true, welcome.hub_epoch == self.signalingEpoch else {
            c.close(); self.disconnect("服务器未确认通话恢复"); return
          }
          self.installSocket(c, credentials: credentials, recovering: true, readyWelcome: welcome)
          return
        } catch {
          candidate?.close()
          if Task.isCancelled { return }
        }
      }
    }
  }
  private func receiveCallSignal(_ message: Signal, principal: Principal) throws {
    try message.validateIncoming(principal: principal)
    guard message.binding == coordinator?.binding || message.type == "invite" || message.type == "dial_accepted" else { return }
    if message.type == "offer" { callRecovery = message.payload["signal_recovery"]?.bool == true }
    if message.type == "peer_reconnecting" ||
      (message.type == "error" && message.payload["code"]?.string == "peer_reconnecting") {
      try beginRecovery()
      return
    }
    if message.type == "peer_resumed" {
      try beginRecovery()
      socket?.send(try signalRecovery.synchronize())
      recordDiagnostic("signal_resumed_syncing")
      return
    }
    if try signalRecovery.accept(message, revision: coordinator?.revision ?? 0) {
      recoveryTimeout?.cancel()
      recoveryTimeout = nil
      recordDiagnostic("signal_state_reconciled")
      if message.payload["state"]?.string == "connected" {
        status = latestAudioState?.payload["active"]?.bool == false ? "系统音频暂时中断，等待恢复" : "通话中"
        if let command = recoveryCommand { socket?.send(command) }
        if let audio = latestAudioState, audio.binding == coordinator?.binding { socket?.send(audio) }
        if let mute = latestMuteState, mute.binding == coordinator?.binding { socket?.send(mute) }
      }
      recoveryCommand = nil
    }
    if message.type == "result", message.request_id == recoveryCommand?.request_id,
      message.payload["accepted"]?.bool == true { recoveryCommand = nil }
    coordinator?.receive(message)
  }
  private func checkHealth(_ id: UUID) {
    let now = Date()
    coordinator?.tick(now: now)
    if let turnExpiry = media?.turnExpiry ?? turnExpiry, now >= turnExpiry.addingTimeInterval(-30) {
      disconnect("本通中继凭据即将到期")
      return
    }
    guard connectionID == id, let socket else { return }
    if now >= expiry || renewalStarted.map({ now.timeIntervalSince($0) > 25 }) == true
      || pingStarted.map({ now.timeIntervalSince($0) > 5 }) == true
      || socket.pendingWriteSince.map({ now.timeIntervalSince($0) > 5 }) == true
    {
      if now >= expiry || renewalStarted != nil { disconnect("连接或授权超时") }
      else { recoverSignal("连接超时") }
      return
    }
    guard online else { return }
    if now.timeIntervalSince(lastPing) >= 15, pingStarted == nil {
      pingStarted = now
      lastPing = now
      Task { [weak self] in
        do {
          try await socket.ping()
          if self?.connectionID == id { self?.pingStarted = nil }
        } catch { if self?.connectionID == id { self?.recoverSignal("连接心跳失败") } }
      }
    }
    if expiry.timeIntervalSince(now) <= 60, renewalStarted == nil {
      renewalStarted = now
      Task { [weak self] in
        guard let self else { return }
        defer { self.requestForegroundConnection() }
        do {
          let record = try await self.identity.credentials(forceRefresh: true)
          guard self.connectionID == id, let tokens = record.tokens else { return }
          let m = Signal(type: "reauth", payload: ["access_token": .string(tokens.access_token)])
          self.renewRequest = m.request_id
          socket.send(m)
        } catch { if self.connectionID == id { self.disconnect("凭据续期失败，请检查配对状态") } }
      }
    }
  }
  func backgrounded() {
    foregroundIntent.deactivate()
    if coordinator?.binding == nil && wake == nil {
      disconnect(pushEnabled ? "后台等待来电推送" : "已停止前台连接；打开 App 后自动连接")
    }
  }
  func disconnect(_ message: String = "已断开") {
    if coordinator?.binding != nil { recordDiagnostic("signaling_disconnected") }
    if let wake, coordinator?.binding != wake.binding {
      provider.reportCall(
        with: UUID(uuidString: wake.binding.callUUID)!, endedAt: Date(), reason: .failed)
    }
    clearWake()
    reconnectTask?.cancel()
    reconnectTask = nil
    recoveryTimeout?.cancel()
    recoveryTimeout = nil
    signalRecovery = SignalRecoveryWindow()
    recoveryCommand = nil
    latestAudioState = nil
    latestMuteState = nil
    callControlsSupported = false
    dtmfAction?.action.fail()
    dtmfAction = nil
    dtmfPending = false
    networkMonitor?.cancel()
    networkMonitor = nil
    networkPath = nil
    relayRecovery = false
    callRecovery = false
    connectionID = UUID()
    online = false
    dialingSupported = false
    finishHistory(message)
    let old = coordinator
    coordinator = nil
    old?.finish()
    if let uuid = callUUID { provider.reportCall(with: uuid, endedAt: Date(), reason: .failed) }
    pendingDial = nil
    callUUID = nil
    incomingReportedUUID = nil
    callPhase = ""
    outgoing = false
    callControlPending = false
    connectedAt = nil
    activatedSession = nil
    activeAudioBinding = nil
    answerAction?.fail()
    answerAction = nil
    media?.close()
    media = nil
    earlyICE.removeAll()
    socket?.close()
    socket = nil
    reader?.cancel()
    reader = nil
    watchdog?.cancel()
    watchdog = nil
    moduleNamesTask?.cancel()
    moduleNamesTask = nil
    renewRequest = nil
    renewalStarted = nil
    pingStarted = nil
    turnExpiry = nil
    lastPing = .distantPast
    status = message
  }
  var canDial: Bool {
    online && dialingSupported && !working && callUUID == nil && modules.contains(selectedModule)
      && PhoneCall.dialNumber(dialNumber) != nil
  }
  func dialCall() {
    guard canDial, let number = PhoneCall.dialNumber(dialNumber) else { return }
    let uuid = UUID()
    pendingDial = (uuid, selectedModule, number)
    callUUID = uuid
    history.begin(id: uuid, module: selectedModule, display: number, outgoing: true)
    saveHistory()
    callDisplay = number
    callModule = selectedModule
    callPhase = "dialing"
    outgoing = true
    muted = false
    speaker = false
    connectedAt = nil
    lastCallResult = ""
    callDiagnostics.removeAll()
    request(CXStartCallAction(call: uuid, handle: CXHandle(type: .phoneNumber, value: number)))
  }
  func answerCall() {
    guard callPhase == "incoming", let uuid = callUUID else { return }
    request(CXAnswerCallAction(call: uuid))
  }
  func endCall() {
    guard callPhase != "ending", let uuid = callUUID else { return }
    request(CXEndCallAction(call: uuid))
  }
  func sendDTMF(_ digit: String) {
    guard let uuid = callUUID, callControlsSupported, !dtmfPending,
      callPhase == "connected", signalRecovery.binding == nil else { return }
    request(CXPlayDTMFCallAction(call: uuid, digits: digit, type: .singleTone))
  }
  func toggleMuted() {
    guard coordinator?.answered == true, let uuid = callUUID else { return }
    request(CXSetMutedCallAction(call: uuid, muted: !muted))
  }
  func refreshAudioRoute() {
    guard callUUID != nil else { return }
    speaker = AVAudioSession.sharedInstance().currentRoute.outputs.contains {
      $0.portType == .builtInSpeaker
    }
  }
  func toggleSpeaker() {
    guard coordinator?.answered == true, coordinator?.ending == false else { return }
    do {
      try AVAudioSession.sharedInstance().overrideOutputAudioPort(speaker ? .none : .speaker)
      refreshAudioRoute()
    } catch { lastCallResult = "切换扬声器失败，请使用系统音频路由选择" }
  }
  private func request(_ action: CXCallAction) {
    guard !callControlPending, callUUID == action.callUUID else { return }
    callControlPending = true
    controller.request(CXTransaction(action: action)) { [weak self] error in
      Task { @MainActor in
        guard let self, self.callUUID == action.callUUID else { return }
        self.callControlPending = false
        if let error {
          self.recordDiagnostic("callkit_transaction_" + phoneFailureCode(error))
          self.lastCallResult = "系统通话操作失败，请重试"
          if action is CXStartCallAction, self.coordinator?.binding == nil {
            self.finishHistory("拨号启动失败")
            self.pendingDial = nil
            self.callUUID = nil
            self.incomingReportedUUID = nil
            self.callPhase = ""
            self.outgoing = false
          }
        }
      }
    }
  }
  private func recordDiagnostic(_ code: String) {
    logger.info("stage=\(code, privacy: .public)")
    callDiagnostics.append(code)
    if callDiagnostics.count > 24 { callDiagnostics.removeFirst(callDiagnostics.count - 24) }
  }
  private func handle(_ effect: CallEffect) {
    switch effect {
    case .reportIncoming(let b, let display):
      let caller = IncomingCaller(display: display)
      outgoing = false
      callUUID = UUID(uuidString: b.callUUID)
      if let id = callUUID {
        history.begin(id: id, module: b.moduleID, display: caller.display, outgoing: false)
        history.identify(id: id, module: b.moduleID, display: caller.display)
        saveHistory()
      }
      callDisplay = caller.display
      callModule = b.moduleID
      muted = false
      speaker = false
      connectedAt = nil
      lastCallResult = ""
      callDiagnostics.removeAll()
      recordDiagnostic("invite_received call=" + b.callUUID)
      let update = incomingCallUpdate(display: caller.display, moduleName: moduleName(for: b.moduleID))
      if wake?.binding == b {
        completeWakeHandoff()
        status = coordinator?.ending == true ? "正在挂断" : "来电"
        return
      }
      provider.reportNewIncomingCall(with: UUID(uuidString: b.callUUID)!, update: update) {
        [weak self] error in
        Task { @MainActor in
          guard let self else { return }
          if self.coordinator?.binding != b {
            if error == nil {
              self.provider.reportCall(
                with: UUID(uuidString: b.callUUID)!, endedAt: Date(), reason: .remoteEnded)
            }
            return
          }
          if let error {
            self.recordDiagnostic("report_failed_" + phoneFailureCode(error))
          } else {
            self.recordDiagnostic("callkit_reported")
            self.incomingReportedUUID = UUID(uuidString: b.callUUID)
            self.refreshIncomingCallerName()
          }
          self.coordinator?.reported(b, success: error == nil)
        }
      }
      status = "来电"
    case .acceptOffer(let b, let sdp, let recovery):
      callControlsSupported = coordinator?.callControls == true
      let controlsUpdate = CXCallUpdate()
      controlsUpdate.supportsDTMF = callControlsSupported
      provider.reportCall(with: UUID(uuidString: b.callUUID)!, updated: controlsUpdate)
      Task { [weak self] in
        guard let self else { return }
        var phase = "ice_fetch"
        do {
          self.recordDiagnostic("ice_fetch_started")
          let ice = try await self.identity.ice(module: b.moduleID)
          guard self.coordinator?.binding == b else { return }
          self.recordDiagnostic("ice_config_validated")
          phase = "media_config"
          let peer = try PhoneMedia(binding: b, ice: ice, recoveryEnabled: recovery, controlEnabled: self.callRecovery)
          peer.onControl = { [weak self] data in
            guard let self, self.coordinator?.binding == b else { return }
            do {
              let message = try Signal.decode(data)
              guard message.binding == b, ["state", "result"].contains(message.type) else { throw PhoneError.invalidData }
              // A DC heartbeat can report call state, but cannot reset WSS's
              // authorization/reconnect deadline or confirm a new connection.
              self.coordinator?.receive(message)
            } catch { self.disconnect("备用控制通道数据无效") }
          }
          peer.onDiagnostic = { [weak self] code in
            guard self?.coordinator?.binding == b else { return }
            self?.recordDiagnostic(code)
          }
          self.turnExpiry = ice.expires_at
          peer.onCandidate = { [weak self] payload in
            guard self?.coordinator?.binding == b else { return }
            self?.socket?.send(Signal(type: "ice", binding: b, payload: payload))
          }
          peer.onConnected = { [weak self] in
            guard self?.coordinator?.binding == b else { return }
            self?.recordDiagnostic("transport_connected")
            self?.coordinator?.mediaReady(b)
          }
          peer.onFailure = { [weak self] in
            if self?.coordinator?.binding == b {
              self?.recordDiagnostic("transport_failed")
              self?.disconnect("音频连接失败")
            }
          }
          self.media = peer
          if self.activeAudioBinding?.callUUID == b.callUUID, let session = self.activatedSession {
            peer.activated(session)
          }
          for candidate in self.earlyICE { peer.candidate(candidate) }
          self.earlyICE.removeAll()
          phase = "sdp_answer"
          self.recordDiagnostic("sdp_answer_started")
          let answer = try await peer.answer(sdp)
          guard self.coordinator?.binding == b else {
            peer.close()
            return
          }
          self.socket?.send(
            Signal(
              type: "answer_sdp", binding: b,
              payload: ["type": .string("answer"), "sdp": .string(answer)]))
          self.recordDiagnostic("answer_sdp_sent")
        } catch {
          if self.coordinator?.binding == b {
            self.recordDiagnostic(phase + "_" + phoneFailureCode(error))
            self.disconnect("音频或中继配置失败")
          }
        }
      }
    case .acceptRestartOffer(let b, let sdp, let id):
      guard let peer = media, peer.binding == b else { disconnect("音频恢复状态无效"); return }
      recordDiagnostic("ice_restart_started")
      Task { [weak self] in
        guard let self else { return }
        do {
          let answer = try await peer.answerRestart(sdp, id: id, refreshICE: { [weak self] in
            guard let self, self.coordinator?.binding == b else { throw PhoneError.unavailable }
            return try await self.identity.ice(module: b.moduleID)
          })
          guard self.coordinator?.binding == b, self.media === peer else { return }
          self.socket?.send(Signal(type: "restart_answer", binding: b,
            payload: ["negotiation_id": .uint(id), "sdp": .string(answer)]))
          self.recordDiagnostic("ice_restart_answer_sent")
        } catch {
          if self.coordinator?.binding == b { self.disconnect("音频网络恢复失败") }
        }
      }
    case .candidate(let b, let payload):
      if let media, media.binding == b {
        media.candidate(payload)
      } else if earlyICE.count < 256 {
        earlyICE.append(payload)
      } else {
        disconnect("音频候选过多")
      }
    case .send(let message):
      if message.type == "audio_active" { latestAudioState = message }
      if message.type == "mute" { latestMuteState = message }
      if message.type == "command", message.payload["action"]?.string == "hangup" {
        recoveryCommand = message // retain one request until acknowledged, including uncertain writes
      }
      if signalRecovery.binding == nil { socket?.send(message) }
      else if message.type == "command", message.payload["action"]?.string == "hangup" { media?.sendControl(message) }
    case .controlCompleted(let b, let id, let accepted):
      guard coordinator?.binding == b else { return }
      if let pending = dtmfAction, pending.id == id {
        if accepted { pending.action.fulfill() } else { pending.action.fail(); status = "按键发送未确认，请按需重试" }
        dtmfAction = nil
        dtmfPending = false
      } else if latestMuteState?.request_id == id, !accepted {
        status = "静音同步未确认，请重试"
      }
    case .answerCompleted(let b):
      if coordinator?.outgoing == true {
        if activeAudioBinding?.callUUID == b.callUUID, let session = activatedSession {
          media?.activated(session)
          coordinator?.activated(b)
        }
        status = "正在启动通话音频"
        break
      }
      guard answerAction?.callUUID.uuidString.lowercased() == b.callUUID else {
        disconnect("接听动作已失效")
        return
      }
      answerAction?.fulfill()
      answerAction = nil
      status = "正在启动通话音频"
    case .phase(let b, let phase):
      guard coordinator?.binding == b else { return }
      callPhase = phase
      switch phase {
      case "incoming": status = "来电"
      case "answering": status = outgoing ? "正在拨号，等待对方接听" : "正在接听"
      case "awaiting_audio": status = "正在启动通话音频"
      case "audio_interrupted": status = "系统音频暂时中断，等待恢复"
      case "connected":
        status = "通话中"
        if connectedAt == nil {
          connectedAt = Date()
          if let id = callUUID { history.connected(id: id, now: connectedAt!); saveHistory() }
          if outgoing {
            provider.reportOutgoingCall(
              with: UUID(uuidString: b.callUUID)!, connectedAt: connectedAt)
          }
        }
        watchCallNetwork()
      case "dialing": status = "正在准备拨号"
      case "ending": status = "正在挂断"
      default: break
      }
    case .audio(let b, let active): if media?.binding == b { media?.setAudio(active) }
    case .ended(let b):
      guard callUUID == UUID(uuidString: b.callUUID) else { return }
      networkMonitor?.cancel()
      networkMonitor = nil
      networkPath = nil
      reconnectTask?.cancel()
      recoveryTimeout?.cancel()
      recoveryTimeout = nil
      signalRecovery = SignalRecoveryWindow()
      recoveryCommand = nil
      latestAudioState = nil
      latestMuteState = nil
      callControlsSupported = false
      dtmfAction?.action.fail()
      dtmfAction = nil
      dtmfPending = false
      callRecovery = false
      guard callUUID == UUID(uuidString: b.callUUID) else { return }
      let reason =
        coordinator?.endReason == "cleanup_failed"
        ? "cleanup_failed"
        : coordinator?.ending == true
          ? (outgoing && connectedAt == nil
            ? "dial_cancelled" : connectedAt == nil ? "rejected" : "remote_hangup")
          : coordinator?.endReason ?? "ended"
      lastCallResult = Self.endMessage(reason, connected: connectedAt != nil)
      finishHistory(lastCallResult)
      let endReason: CXCallEndedReason =
        ["remote_hangup", "rejected"].contains(reason)
        ? .remoteEnded
        : ["ring_timeout", "answer_timeout"].contains(reason)
          ? .unanswered
          : [
            "peer_closed", "media_failed", "module_busy", "dial_not_allowed", "module_unavailable",
            "answer_failed", "cleanup_failed",
          ].contains(reason) ? .failed : .remoteEnded
      pendingDial = nil
      outgoing = false
      activatedSession = nil
      callUUID = nil
      incomingReportedUUID = nil
      callPhase = ""
      callControlPending = false
      connectedAt = nil
      muted = false
      speaker = false
      activeAudioBinding = nil
      turnExpiry = nil
      answerAction?.fail()
      answerAction = nil
      media?.close()
      media = nil
      earlyICE.removeAll()
      provider.reportCall(
        with: UUID(uuidString: b.callUUID)!, endedAt: Date(), reason: endReason)
      status = online ? "在线，等待来电" : "已断开"
      // After a background call ends, do not leave an idle WSS socket that iOS
      // may suspend while the Relay still mistakes it for an available phone.
      Task { [weak self] in
        guard let self, self.callUUID == nil, self.coordinator?.binding == nil, self.wake == nil
        else { return }
        if UIApplication.shared.applicationState == .background { self.backgrounded() }
        else if self.foregroundPaused { self.disconnect("前台连接已暂停；后台来电按开关设置接收") }
      }
    case .disconnect(let reason):
      recordDiagnostic(reason.rawValue)
      disconnect("通话已结束或连接不可用")
    }
  }
  static func endMessage(_ reason: String, connected: Bool) -> String {
    switch reason {
    case "cleanup_failed": return "模块挂断未确认，请检查模块通话状态"
    case "cellular_ended": return "对方已挂断"
    case "dial_cancelled": return "已取消拨号"
    case "rejected": return "已拒接"
    case "remote_hangup": return "已挂断"
    case "ring_timeout": return "来电未接听"
    case "answer_timeout": return "对方未接听或模块音频未就绪"
    case "module_busy": return "模块正在通话，请稍后再拨"
    case "dial_not_allowed": return "此模块未启用本机远程通话，或已配置 AI 接听"
    case "module_unavailable": return "模块不在线"
    case "answer_failed": return "模块拨号或接听失败"
    case "peer_closed": return "远端连接已断开"
    case "dial_rate_limited": return "拨号过于频繁，请稍后重试"
    default: return connected ? "通话已结束" : "本次通话已结束"
    }
  }
  func provider(_ provider: CXProvider, perform action: CXStartCallAction) {
    guard let pending = pendingDial, action.callUUID == pending.uuid, online, let coordinator else {
      action.fail()
      return
    }
    do {
      let session = AVAudioSession.sharedInstance()
      try session.setCategory(.playAndRecord, mode: .voiceChat, options: [.allowBluetoothHFP])
      try session.setPreferredSampleRate(48000)
      try session.setPreferredIOBufferDuration(0.02)
      try coordinator.beginOutgoing(
        module: pending.module, uuid: pending.uuid, epoch: signalingEpoch, number: pending.number)
      recordDiagnostic("outgoing_requested call=" + pending.uuid.uuidString.lowercased())
      provider.reportOutgoingCall(with: pending.uuid, startedConnectingAt: Date())
      action.fulfill()
    } catch {
      action.fail()
      finishHistory("拨号启动失败")
      pendingDial = nil
      callUUID = nil
      incomingReportedUUID = nil
      callPhase = ""
      outgoing = false
      lastCallResult = "拨号启动失败"
    }
  }
  func providerDidReset(_ provider: CXProvider) { disconnect("系统已重置通话") }
  func provider(_ provider: CXProvider, perform action: CXAnswerCallAction) {
    if let wake, wake.binding.callUUID == action.callUUID.uuidString.lowercased(),
      coordinator?.binding == nil, answerAction == nil
    {
      answerAction = action
      return
    }
    guard let b = coordinator?.binding, b.callUUID == action.callUUID.uuidString.lowercased(),
      answerAction == nil
    else {
      action.fail()
      return
    }
    do { try media?.configureAudio() } catch {
      action.fail()
      disconnect("音频配置失败")
      return
    }
    answerAction = action
    recordDiagnostic("answer_requested")
    coordinator?.requestAnswer(b)
  }
  func provider(_ provider: CXProvider, perform action: CXEndCallAction) {
    if let wake, wake.binding.callUUID == action.callUUID.uuidString.lowercased(),
      coordinator?.binding == nil
    {
      wakeRejected = true
      answerAction?.fail()
      answerAction = nil
      callPhase = "ending"
      status = "正在拒接后台来电"
      // Keep restoring the authenticated route, then send one reject. Push
      // data alone cannot authorize ATH, just as it cannot authorize ATA.
      action.fulfill()
      return
    }
    guard let b = coordinator?.binding, b.callUUID == action.callUUID.uuidString.lowercased() else {
      action.fail()
      return
    }
    answerAction?.fail()
    answerAction = nil
    coordinator?.requestEnd(b)
    action.fulfill()
  }
  func provider(_ provider: CXProvider, perform action: CXPlayDTMFCallAction) {
    guard action.type == .singleTone, dtmfAction == nil, signalRecovery.binding == nil,
      let b = coordinator?.binding, b.callUUID == action.callUUID.uuidString.lowercased(),
      let id = coordinator?.requestDTMF(b, digit: action.digits) else { action.fail(); return }
    dtmfAction = (id, action)
    dtmfPending = true
  }
  func provider(_ provider: CXProvider, perform action: CXSetMutedCallAction) {
    guard media?.binding.callUUID == action.callUUID.uuidString.lowercased() else {
      action.fail()
      return
    }
    media?.setMuted(action.isMuted)
    muted = action.isMuted
    if let b = coordinator?.binding { coordinator?.setMuted(b, muted: action.isMuted) }
    action.fulfill()
  }
  func provider(_ provider: CXProvider, timedOutPerforming action: CXAction) {
    if let dtmf = action as? CXPlayDTMFCallAction {
      dtmf.fail()
      if dtmfAction?.action.uuid == dtmf.uuid { dtmfAction = nil; dtmfPending = false }
      return
    }
    if let action = action as? CXCallAction,
      wake?.binding.callUUID == action.callUUID.uuidString.lowercased()
    {
      disconnect("后台接听操作超时")
      return
    }
    action.fail()
    guard let callAction = action as? CXCallAction,
      coordinator?.binding?.callUUID == callAction.callUUID.uuidString.lowercased()
    else { return }
    disconnect("系统通话动作超时")
  }
  func provider(_ provider: CXProvider, didActivate audioSession: AVAudioSession) {
    guard let b = coordinator?.binding, coordinator?.ending == false,
      coordinator?.answered == true || coordinator?.outgoing == true
    else { return }
    activeAudioBinding = b
    activatedSession = audioSession
    recordDiagnostic("audio_session_activated")
    media?.activated(audioSession)
    coordinator?.activated(b)
  }
  func provider(_ provider: CXProvider, didDeactivate audioSession: AVAudioSession) {
    guard let b = activeAudioBinding else { return }
    activeAudioBinding = nil
    activatedSession = nil
    if media?.binding.callUUID == b.callUUID { media?.deactivated(audioSession) }
    if let current = coordinator?.binding, current.callUUID == b.callUUID {
      coordinator?.deactivated(current)
    }
  }
}
