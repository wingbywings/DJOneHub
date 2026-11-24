import DJOneHubPhoneCore
import SwiftUI
import UserNotifications

@MainActor final class PhoneSMSInbox: NSObject, ObservableObject, @preconcurrency UNUserNotificationCenterDelegate {
  static let shared = PhoneSMSInbox()
  @Published private(set) var messages: [SMSNotification] = []
  @Published private(set) var status = "配对后自动接收已开启 APP 接听线路的短信通知"
  @Published var openedNotification: String?
  private var identity: PhoneIdentity?
  private var principal: Principal?
  private var environment = ""
  private var token: String?
  private var registration: Task<Void, Never>?
  private var authorization: Task<Void, Never>?
  private var generation = UUID()
  private var writable = true
  private struct Saved: Codable {
    var owner: String
    var messages: [SMSNotification]
    var deletedIDs: [String]
  }
  private var owner = ""
  private var deletedIDs: [String] = []
  private let storageURL: URL?
  init(url: URL? = nil) { storageURL = url; super.init() }
  private var file: URL {
    storageURL ?? FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0]
      .appendingPathComponent("sms-notifications.json")
  }

  func bind(identity: PhoneIdentity, environment: String) {
    self.identity = identity
    self.environment = environment
    let record = try? identity.status()
    let next = record?.tokens?.principal
    let nextOwner = next.map { (record?.relay ?? "") + "|" + $0.hub_id + "|" + $0.id } ?? ""
    if nextOwner != owner {
      registration?.cancel(); registration = nil
      authorization?.cancel(); authorization = nil
      generation = UUID()
      messages = []; deletedIDs = []; owner = nextOwner
      writable = true
      if let next, FileManager.default.fileExists(atPath: file.path) {
        do {
          let data = try Data(contentsOf: file)
          guard data.count <= 4_000_000 else { throw PhoneError.invalidData }
          let decoder = JSONDecoder()
          if let saved = try? decoder.decode(Saved.self, from: data) {
            if saved.owner == owner {
              messages = saved.messages
              deletedIDs = Array(saved.deletedIDs.suffix(10_000))
            }
          } else {
            // Migrate the original array format on the next save.
            messages = try decoder.decode([SMSNotification].self, from: data)
          }
          messages = messages
            .filter { $0.installation_id == next.id && $0.hub_id == next.hub_id && next.modules.contains($0.module_id) }
          messages = Array(messages.prefix(500))
        } catch { writable = false; status = "短信通知记录无法读取，请清除本机记录后重试" }
      }
    }
    principal = next
  }

  func activate() {
    guard principal != nil, authorization == nil else { return }
    let current = generation
    authorization = Task { [weak self] in
      guard let self else { return }
      defer { if self.generation == current { self.authorization = nil } }
      do {
        let center = UNUserNotificationCenter.current()
        let granted = try await center.requestAuthorization(options: [.alert, .sound, .badge])
        guard !Task.isCancelled, self.generation == current else { return }
        if granted {
          UIApplication.shared.registerForRemoteNotifications()
          self.syncRegistration()
        } else { self.status = "短信通知未获系统授权，请在 iPhone 设置中允许通知" }
        for notification in await center.deliveredNotifications() {
          guard self.generation == current else { return }
          self.receive(notification.request.content.userInfo)
        }
      } catch { self.status = "无法启用短信通知，请检查系统通知设置" }
    }
  }

  func registered(_ data: Data) {
    token = data.map { String(format: "%02x", $0) }.joined()
    syncRegistration()
  }

  func registrationFailed() { status = "短信通知注册失败，请检查网络与 App 签名" }

  private func syncRegistration() {
    guard let identity, principal != nil, let token, registration == nil else { return }
    let current = generation
    registration = Task { [weak self] in
      guard let self else { return }
      defer {
        if self.generation == current {
          self.registration = nil
          if self.token != token { self.syncRegistration() }
        }
      }
      for attempt in 0..<3 {
        guard !Task.isCancelled, self.generation == current else { return }
        do {
          try await identity.registerPush(environment: self.environment, token: token, alerts: true)
          guard !Task.isCancelled, self.generation == current else { return }
          self.status = "短信通知已接入；Hub 开启 APP 接听后自动暂停该 SIM 的 Bark 提示"
          return
        } catch {
          self.status = "短信通知接入失败，请检查 Relay 是否已升级及 APNs 配置"
          if attempt < 2 { try? await Task.sleep(for: .seconds(attempt + 1)) }
        }
      }
    }
  }

  @discardableResult
  func receive(_ userInfo: [AnyHashable: Any]) -> String? {
    guard let principal, let data = try? JSONSerialization.data(withJSONObject: userInfo),
      let message = try? SMSNotification.decode(data, principal: principal) else { return nil }
    guard !deletedIDs.contains(message.id) else { return nil }
    if !messages.contains(where: { $0.id == message.id }) {
      messages.append(message)
      messages.sort { $0.timestamp > $1.timestamp }
      messages = Array(messages.prefix(500))
      save()
    }
    return message.id
  }

  @discardableResult private func save() -> Bool {
    guard writable else { return false }
    do {
      try FileManager.default.createDirectory(at: file.deletingLastPathComponent(), withIntermediateDirectories: true)
      try JSONEncoder().encode(Saved(owner: owner, messages: messages, deletedIDs: deletedIDs))
        .write(to: file, options: [.atomic, .completeFileProtectionUntilFirstUserAuthentication])
      var url = file
      var values = URLResourceValues(); values.isExcludedFromBackup = true
      try url.setResourceValues(values)
      return true
    } catch { status = "本机短信通知记录保存失败"; return false }
  }

  @discardableResult func delete(_ id: String) -> Bool {
    guard messages.contains(where: { $0.id == id }) else { return false }
    return remove([id])
  }

  @discardableResult private func remove(_ ids: [String]) -> Bool {
    let previous = messages, previousDeleted = deletedIDs
    let removing = Set(ids)
    messages.removeAll { removing.contains($0.id) }
    deletedIDs.removeAll { removing.contains($0) }
    deletedIDs = Array((deletedIDs + ids).suffix(10_000))
    guard save() else { messages = previous; deletedIDs = previousDeleted; return false }
    if let openedNotification, removing.contains(openedNotification) { self.openedNotification = nil }
    // Notification-center identifiers differ from the SMS payload IDs.
    let recipient = principal
    Task {
      let center = UNUserNotificationCenter.current()
      let notifications = await center.deliveredNotifications()
      let identifiers = notifications.compactMap { notification -> String? in
        guard let recipient,
          let data = try? JSONSerialization.data(withJSONObject: notification.request.content.userInfo),
          let message = try? SMSNotification.decode(data, principal: recipient),
          removing.contains(message.id) else { return nil }
        return notification.request.identifier
      }
      center.removeDeliveredNotifications(withIdentifiers: identifiers)
    }
    return true
  }

  func clear() {
    writable = true
    if remove(messages.map(\.id)) { UNUserNotificationCenter.current().removeAllDeliveredNotifications() }
  }

  func forget() {
    registration?.cancel(); registration = nil
    authorization?.cancel(); authorization = nil
    generation = UUID(); principal = nil; identity = nil
    token = nil; openedNotification = nil
    owner = ""; messages = []; deletedIDs = []; writable = true; save()
    UNUserNotificationCenter.current().removeAllDeliveredNotifications()
    UIApplication.shared.unregisterForRemoteNotifications()
    status = "配对后自动接收已开启 APP 接听线路的短信通知"
  }

  func userNotificationCenter(_ center: UNUserNotificationCenter, willPresent notification: UNNotification,
    withCompletionHandler completionHandler: @escaping (UNNotificationPresentationOptions) -> Void) {
    let accepted = receive(notification.request.content.userInfo) != nil
    completionHandler(accepted ? [.banner, .list, .sound] : [])
  }

  func userNotificationCenter(_ center: UNUserNotificationCenter, didReceive response: UNNotificationResponse,
    withCompletionHandler completionHandler: @escaping () -> Void) {
    openedNotification = receive(response.notification.request.content.userInfo)
    completionHandler()
  }
}

extension SMSNotification {
  var displaySender: String {
    guard let number = OutgoingSMS.normalizePhone(sender) else { return sender }
    for prefix in ["+86", "0086"] where number.hasPrefix(prefix) {
      let national = String(number.dropFirst(prefix.count))
      if !national.isEmpty { return national }
    }
    return sender
  }
}

// Filter the complete local list before taking a page, including matches beyond page one.
struct PhoneSMSPage<Item> {
  static var size: Int { 20 }
  let matches: [Item]
  let number: Int
  var count: Int { max(1, (matches.count + Self.size - 1) / Self.size) }
  var items: [Item] { Array(matches.dropFirst((number - 1) * Self.size).prefix(Self.size)) }

  init(_ items: [Item], query: String, page: Int, fields: (Item) -> [String]) {
    let term = query.trimmingCharacters(in: .whitespacesAndNewlines)
    matches = term.isEmpty ? items : items.filter { fields($0).contains { $0.localizedStandardContains(term) } }
    number = min(max(1, page), max(1, (matches.count + Self.size - 1) / Self.size))
  }
}

struct PhoneSMSView: View {
  private enum Folder: String, CaseIterable { case inbox = "收件箱", outgoing = "发件记录", drafts = "草稿箱" }
  @ObservedObject var runtime: PhoneRuntime
  @ObservedObject var inbox = PhoneSMSInbox.shared
  @ObservedObject var outbox = PhoneSMSOutbox.shared
  @State private var clearing = false
  @State private var composing = false
  @State private var folder = Folder.inbox
  @State private var query = ""
  @State private var page = 1

  private var incomingPage: PhoneSMSPage<SMSNotification> {
    PhoneSMSPage(inbox.messages, query: query, page: page) {
      [$0.sender, $0.displaySender, $0.content, $0.module_id, runtime.moduleName(for: $0.module_id)]
    }
  }
  private var outgoingPage: PhoneSMSPage<PhoneSMSOutbox.Entry> {
    PhoneSMSPage(outbox.visibleEntries, query: query, page: page) {
      [$0.request.phone, $0.request.message, $0.request.module_id,
       runtime.moduleName(for: $0.request.module_id), $0.summary]
    }
  }
  private var draftsPage: PhoneSMSPage<PhoneSMSOutbox.SavedDraft> {
    PhoneSMSPage(outbox.draftEntries, query: query, page: page) {
      [$0.content.phone, $0.content.message, $0.content.module, runtime.moduleName(for: $0.content.module)]
    }
  }
  private var matchCount: Int {
    switch folder {
    case .inbox: return incomingPage.matches.count
    case .outgoing: return outgoingPage.matches.count
    case .drafts: return draftsPage.matches.count
    }
  }
  private var pageCount: Int {
    switch folder {
    case .inbox: return incomingPage.count
    case .outgoing: return outgoingPage.count
    case .drafts: return draftsPage.count
    }
  }
  private var currentPage: Int { min(page, pageCount) }

  var body: some View {
    ScrollViewReader { proxy in
      List {
        Group {
          HStack {
            Text("短信").font(.system(size: 24, weight: .heavy)).accessibilityAddTraits(.isHeader)
            Spacer()
            Button {
              if outbox.startDraft(module: runtime.selectedModule) { composing = true }
            } label: { Label("新短信", systemImage: "square.and.pencil") }
              .disabled(!outbox.ready)
          }.id("sms-list-top")
          Picker("短信列表", selection: Binding(get: { folder }, set: { folder = $0; query = ""; page = 1 })) {
            ForEach(Folder.allCases, id: \.self) { Text($0.rawValue).tag($0) }
          }.pickerStyle(.segmented)
          HStack {
            Image(systemName: "magnifyingglass").foregroundStyle(PhoneTheme.secondary)
            TextField("搜索号码、正文或线路", text: Binding(get: { query }, set: { query = $0; page = 1 }))
              .textInputAutocapitalization(.never).autocorrectionDisabled()
              .submitLabel(.search)
            if !query.isEmpty {
              Button { query = ""; page = 1 } label: { Image(systemName: "xmark.circle.fill") }
                .accessibilityLabel("清除搜索").buttonStyle(.borderless)
            }
          }.padding(12).phoneGlass(radius: 14)
          if !outbox.error.isEmpty { Text(outbox.error).font(.caption).foregroundStyle(PhoneTheme.red) }
          if matchCount == 0 && !query.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty {
            EmptyPhoneState(icon: "magnifyingglass", title: "没有匹配的短信", message: "试试其他号码、正文或线路关键词")
          }
          switch folder {
          case .outgoing:
            PhoneSMSOutgoingList(runtime: runtime, entries: outgoingPage.items) { composing = true }
          case .drafts:
            draftRows
          case .inbox:
            inboxRows
          }
          HStack {
            Button("上一页") { page = currentPage - 1 }.disabled(currentPage <= 1)
            Spacer()
            Text("第 \(currentPage)/\(pageCount) 页 · 共 \(matchCount) 条")
              .font(.caption).foregroundStyle(PhoneTheme.secondary)
            Spacer()
            Button("下一页") { page = currentPage + 1 }.disabled(currentPage >= pageCount)
          }.buttonStyle(.borderless).padding(.vertical, 8)
        }.listRowInsets(EdgeInsets(top: 6, leading: 22, bottom: 6, trailing: 22))
          .listRowSeparator(.hidden)
          .listRowBackground(Color.clear)
      }.listStyle(.plain).scrollContentBackground(.hidden)
        .scrollDismissesKeyboard(.interactively)
        .onChange(of: page) { _, _ in proxy.scrollTo("sms-list-top", anchor: .top) }
    }.sheet(isPresented: $composing) {
      PhoneSMSComposeView(runtime: runtime, saved: { folder = .drafts; query = ""; page = 1 }) {
        folder = .outgoing; query = ""; page = 1
      }
    }.onChange(of: pageCount) { _, count in page = min(page, count) }
      .onChange(of: inbox.openedNotification, initial: true) { _, value in
        if let value {
          folder = .inbox; query = ""
          page = (inbox.messages.firstIndex(where: { $0.id == value }) ?? 0) / PhoneSMSPage<SMSNotification>.size + 1
        }
      }.confirmationDialog("清除本机短信通知记录？", isPresented: $clearing, titleVisibility: .visible) {
        Button("清除记录", role: .destructive) { inbox.clear() }
      }
  }

  @ViewBuilder private var inboxRows: some View {
    Text(inbox.status).font(.caption).foregroundStyle(PhoneTheme.secondary)
    if inbox.messages.isEmpty {
      EmptyPhoneState(icon: "message", title: "暂无短信通知", message: "Hub 开启该 SIM 的 APP 接听后，新短信将自动通知到这里")
    }
    ForEach(incomingPage.items) { message in
      VStack(alignment: .leading, spacing: 8) {
        HStack {
          Text(message.displaySender).font(.headline)
          Spacer()
          Text(runtime.moduleName(for: message.module_id)).font(.caption).foregroundStyle(PhoneTheme.violet)
        }
        Text(message.content).font(.body).textSelection(.enabled)
        if OutgoingSMS.normalizePhone(message.sender) != nil {
          Button("回复") {
            if outbox.startDraft(phone: message.sender, module: message.module_id) { composing = true }
          }.font(.caption).disabled(!outbox.ready)
        }
        Text(message.timestamp, format: .dateTime.year().month().day().hour().minute())
          .font(.caption).foregroundStyle(PhoneTheme.secondary)
      }.frame(maxWidth: .infinity, alignment: .leading).padding(14).phoneGlass(radius: 20)
        .buttonStyle(.borderless)
        .swipeActions(edge: .trailing, allowsFullSwipe: true) {
          Button("删除", role: .destructive) { inbox.delete(message.id) }
        }
    }
    Button("清除本机短信通知记录", role: .destructive) { clearing = true }
      .font(.caption).padding(.vertical, 8)
    Text("显示本机最近 500 条短信通知，左划可删除本机记录；Hub 上的短信不受影响。")
      .font(.caption2).foregroundStyle(PhoneTheme.secondary)
  }

  @ViewBuilder private var draftRows: some View {
    if outbox.draftEntries.isEmpty {
      EmptyPhoneState(icon: "doc.text", title: "暂无草稿", message: "保存的草稿会显示在这里，点击即可继续编辑")
    }
    ForEach(draftsPage.items) { entry in
      Button {
        if outbox.restoreDraft(entry.id) { composing = true }
      } label: {
        VStack(alignment: .leading, spacing: 8) {
          HStack {
            Text(entry.content.phone.isEmpty ? "未填写收件人" : entry.content.phone).font(.headline)
            Spacer()
            Text(runtime.moduleName(for: entry.content.module)).font(.caption).foregroundStyle(PhoneTheme.violet)
          }
          Text(entry.content.message.isEmpty ? "未填写正文" : entry.content.message).lineLimit(3)
          if entry.id == "current" {
            Text("继续上次编辑").font(.caption).foregroundStyle(PhoneTheme.secondary)
          } else {
            Text(entry.updatedAt, format: .dateTime.year().month().day().hour().minute())
              .font(.caption).foregroundStyle(PhoneTheme.secondary)
          }
        }.frame(maxWidth: .infinity, alignment: .leading).padding(14).phoneGlass(radius: 20)
      }.buttonStyle(.plain)
        .swipeActions(edge: .trailing, allowsFullSwipe: true) {
          Button("删除", role: .destructive) { outbox.deleteDraft(entry.id) }
        }
    }
    Text("点击草稿继续编辑，左划可删除本机草稿。")
      .font(.caption2).foregroundStyle(PhoneTheme.secondary)
  }
}
