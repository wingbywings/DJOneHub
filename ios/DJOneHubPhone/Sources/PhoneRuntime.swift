import AVFAudio
import CallKit
import DJOneHubPhoneCore
import OSLog
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
  @Published var selectedModule = ""
  @Published var dialNumber = ""
  @Published private(set) var outgoing = false
  private var pendingDial: (uuid: UUID, module: String, number: String)?
  private var signalingEpoch = ""
  private var activatedSession: AVAudioSession?
  @Published private(set) var callUUID: UUID?
  @Published private(set) var callDisplay = ""
  @Published private(set) var callModule = ""
  @Published private(set) var callPhase = ""
  @Published private(set) var muted = false
  @Published private(set) var speaker = false
  @Published private(set) var callControlPending = false
  @Published private(set) var connectedAt: Date?
  @Published private(set) var lastCallResult = ""
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
  }
  func disablePush() async {
    guard !working, callUUID == nil else { return }
    working = true
    pushTask?.cancel()
    if let task = pushTask { await task.value }
    pushTask = nil
    defer { working = false }
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
  private func completeWakeHandoff() {
    guard let wake, coordinator?.binding == wake.binding, let reported = wakeReported else {
      return
    }
    let binding = wake.binding
    let rejected = wakeRejected
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
    let uuid = hint.flatMap { UUID(uuidString: $0.binding.callUUID) } ?? UUID()
    let duplicate =
      hint.map { coordinator?.binding == $0.binding || wake?.binding == $0.binding } ?? false
    let canStart = !duplicate && wake == nil && coordinator?.binding == nil && !working
    if canStart, let hint {
      wake = hint
      wakeReported = nil
      callUUID = uuid
      callDisplay = "DJOneHub"
      callModule = hint.binding.moduleID
      callPhase = "incoming"
    }
    let update = CXCallUpdate()
    update.remoteHandle = CXHandle(type: .generic, value: "DJOneHub")
    update.localizedCallerName = "DJOneHub"
    update.hasVideo = false
    update.supportsHolding = false
    update.supportsGrouping = false
    update.supportsUngrouping = false
    update.supportsDTMF = false
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
      installation = record?.tokens?.principal.id ?? ""
      modules = record?.tokens?.principal.modules ?? []
      if !modules.contains(selectedModule) { selectedModule = modules.first ?? "" }
      fingerprint = try identity.fingerprint() ?? ""
      if let r = record, r.phase != "ready" { status = "配对或凭据更新未完成，请先在 Hub 撤销此安装身份，再清理本机凭据重新配对。" }
    } catch { status = "无法读取本机凭据" }
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
      reloadStatus()
      status = "本机凭据已清理，可重新导入配对文件"
    } catch { status = "无法清理本机凭据" }
  }
  func importPairing(_ url: URL) async {
    guard !working, !online else { return }
    working = true
    defer {
      working = false
      reloadStatus()
      syncPushRegistration()
    }
    let allowed = url.startAccessingSecurityScopedResource()
    defer { if allowed { url.stopAccessingSecurityScopedResource() } }
    do {
      let file = try FileHandle(forReadingFrom: url)
      defer { try? file.close() }
      let data = try file.read(upToCount: 8193) ?? Data()
      try await identity.pair(PairingFile.parse(data))
      status = "配对完成"
    } catch { status = "配对失败；请检查配对文件与本机凭据状态" }
  }
  func connect(fromPush: Bool = false) async {
    guard !working, socket == nil else { return }
    working = true
    let attempt = connectionID
    let wakeBinding = wake?.binding
    defer {
      working = false
      syncPushRegistration()
    }
    let permitted =
      fromPush
      ? AVAudioApplication.shared.recordPermission == .granted
      : await AVAudioApplication.requestRecordPermission()
    guard permitted else {
      if fromPush { disconnect("后台接听需要先在前台授予麦克风权限") }
      status = "需要麦克风权限才能接听"
      return
    }
    do {
      let credentials = try await identity.credentials()
      guard connectionID == attempt, !fromPush || wake?.binding == wakeBinding else { return }
      guard let tokens = credentials.tokens else { throw PhoneError.invalidData }
      if let wake { try wake.validate(principal: tokens.principal) }
      let c = try SignalSocket(identity: credentials)
      connectionID = UUID()
      let id = connectionID
      socket = c
      status = "正在连接"
      c.onFailure = { [weak self] in self?.disconnect("信令发送失败") }
      coordinator = PhoneCall(principal: tokens.principal) { [weak self] effect in
        self?.handle(effect)
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
          let welcome = try await c.receive()
          guard self.connectionID == id else { return }
          let exp = try welcome.welcomeExpiry(principal: tokens.principal)
          self.expiry = exp
          self.signalingEpoch = welcome.hub_epoch!
          self.online = true
          self.status = "在线，等待来电"
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
                let known = ["offer", "state", "result", "error", "peer_closed", "dial_accepted"]
                  .contains(
                    message.type)
                self.recordDiagnostic(known ? "received_" + message.type : "received_unknown")
              }
              if message.type == "invite", let wake = self.wake, !wake.matches(message) {
                self.disconnect("推送来电已失效")
                return
              }
              self.coordinator?.receive(message)
            }
          }
        } catch {
          if self.connectionID == id {
            self.recordDiagnostic("signaling_receive_" + phoneFailureCode(error))
            self.disconnect("连接已断开，请重新连接")
          }
        }
      }
    } catch {
      if fromPush { disconnect("后台来电连接失败") } else { status = "连接失败；请检查 Relay 和配对状态" }
    }
  }
  private func checkHealth(_ id: UUID) {
    let now = Date()
    coordinator?.tick(now: now)
    if let turnExpiry, now >= turnExpiry.addingTimeInterval(-30) {
      disconnect("本通中继凭据即将到期")
      return
    }
    guard connectionID == id, let socket else { return }
    if now >= expiry || renewalStarted.map({ now.timeIntervalSince($0) > 25 }) == true
      || pingStarted.map({ now.timeIntervalSince($0) > 5 }) == true
      || socket.pendingWriteSince.map({ now.timeIntervalSince($0) > 5 }) == true
    {
      disconnect("连接超时")
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
        } catch { if self?.connectionID == id { self?.disconnect("连接心跳失败") } }
      }
    }
    if expiry.timeIntervalSince(now) <= 60, renewalStarted == nil {
      renewalStarted = now
      Task { [weak self] in
        guard let self else { return }
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
    if coordinator?.binding == nil && wake == nil {
      disconnect(pushEnabled ? "后台等待来电推送" : "已停止前台等待；再次打开后请连接")
    }
  }
  func disconnect(_ message: String = "已断开") {
    if coordinator?.binding != nil { recordDiagnostic("signaling_disconnected") }
    if let wake, coordinator?.binding != wake.binding {
      provider.reportCall(
        with: UUID(uuidString: wake.binding.callUUID)!, endedAt: Date(), reason: .failed)
    }
    clearWake()
    connectionID = UUID()
    online = false
    dialingSupported = false
    let old = coordinator
    coordinator = nil
    old?.finish()
    if let uuid = callUUID { provider.reportCall(with: uuid, endedAt: Date(), reason: .failed) }
    pendingDial = nil
    callUUID = nil
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
            self.pendingDial = nil
            self.callUUID = nil
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
      outgoing = false
      callUUID = UUID(uuidString: b.callUUID)
      callDisplay = display.isEmpty ? "DJOneHub" : display
      callModule = b.moduleID
      muted = false
      speaker = false
      connectedAt = nil
      lastCallResult = ""
      callDiagnostics.removeAll()
      recordDiagnostic("invite_received call=" + b.callUUID)
      let update = CXCallUpdate()
      update.remoteHandle = CXHandle(type: .generic, value: display.isEmpty ? "DJOneHub" : display)
      update.localizedCallerName = display
      update.hasVideo = false
      update.supportsHolding = false
      update.supportsGrouping = false
      update.supportsUngrouping = false
      update.supportsDTMF = false
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
          }
          self.coordinator?.reported(b, success: error == nil)
        }
      }
      status = "来电"
    case .acceptOffer(let b, let sdp):
      Task { [weak self] in
        guard let self else { return }
        var phase = "ice_fetch"
        do {
          self.recordDiagnostic("ice_fetch_started")
          let ice = try await self.identity.ice(module: b.moduleID)
          guard self.coordinator?.binding == b else { return }
          self.recordDiagnostic("ice_config_validated")
          phase = "media_config"
          let peer = try PhoneMedia(binding: b, ice: ice)
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
    case .candidate(let b, let payload):
      if let media, media.binding == b {
        media.candidate(payload)
      } else if earlyICE.count < 256 {
        earlyICE.append(payload)
      } else {
        disconnect("音频候选过多")
      }
    case .send(let message): socket?.send(message)
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
      case "connected":
        status = "通话中"
        if connectedAt == nil {
          connectedAt = Date()
          if outgoing {
            provider.reportOutgoingCall(
              with: UUID(uuidString: b.callUUID)!, connectedAt: connectedAt)
          }
        }
      case "dialing": status = "正在准备拨号"
      case "ending": status = "正在挂断"
      default: break
      }
    case .audio(let b, let active): if media?.binding == b { media?.setAudio(active) }
    case .ended(let b):
      guard callUUID == UUID(uuidString: b.callUUID) else { return }
      let reason =
        coordinator?.endReason == "cleanup_failed"
        ? "cleanup_failed"
        : coordinator?.ending == true
          ? (outgoing && connectedAt == nil
            ? "dial_cancelled" : connectedAt == nil ? "rejected" : "remote_hangup")
          : coordinator?.endReason ?? "ended"
      lastCallResult = Self.endMessage(reason, connected: connectedAt != nil)
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
        guard let self, UIApplication.shared.applicationState == .background,
          self.coordinator?.binding == nil, self.wake == nil
        else { return }
        self.backgrounded()
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
      pendingDial = nil
      callUUID = nil
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
  func provider(_ provider: CXProvider, perform action: CXSetMutedCallAction) {
    guard media?.binding.callUUID == action.callUUID.uuidString.lowercased() else {
      action.fail()
      return
    }
    media?.setMuted(action.isMuted)
    muted = action.isMuted
    action.fulfill()
  }
  func provider(_ provider: CXProvider, timedOutPerforming action: CXAction) {
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
