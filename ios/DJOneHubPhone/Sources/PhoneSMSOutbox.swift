import DJOneHubPhoneCore
import SwiftUI

@MainActor final class PhoneSMSOutbox: ObservableObject {
  static let shared = PhoneSMSOutbox()
  struct Draft: Codable, Equatable {
    var module = ""; var phone = ""; var message = ""
    var hasContent: Bool { !phone.isEmpty || !message.isEmpty }
  }
  struct SavedDraft: Codable, Identifiable {
    var id = UUID().uuidString
    var content: Draft
    var updatedAt = Date()
  }
  struct Entry: Codable, Identifiable {
    let request: OutgoingSMS
    var state = "pending"
    var segments = 0
    var code = ""
    // Optional for compatibility with records written before swipe deletion.
    var hidden: Bool?
    var id: String { request.id }
    var pending: Bool { ["pending", "queued", "sending"].contains(state) }
    var segmentProgress: String {
      let total = state == "sent" ? segments : max(OutgoingSMS.segmentCount(request.message), segments)
      return total > 1 ? " · \(segments)/\(total)" : ""
    }
    var summary: String {
      let status: String
      switch state {
      case "pending": status = "正在提交，等待服务确认"
      case "queued": status = "等待 Hub 发送"
      case "sending": status = "SIM 正在发送"
      case "sent": status = "已提交运营商"
      case "unknown": status = "结果待确认；未自动重发"
      default:
        switch code {
        case "request_expired": status = "发送失败：Hub 未及时处理，请确认在线后重新编辑"
        case "sms_not_allowed", "unauthorized": status = "发送失败：请在 Hub 启用该线路并选择此手机"
        case "module_busy": status = "发送失败：线路正在通话或执行其他操作"
        case "module_unavailable": status = "发送失败：SIM 模块离线"
        case "message_too_long": status = "发送失败：短信超过 10 段"
        case "invalid_number": status = "发送失败：收件号码无效"
        default: status = "发送失败：请检查线路及短信内容"
        }
      }
      return status + segmentProgress
    }
  }
  private struct Saved: Codable {
    var owner: String; var draft: Draft; var entries: [Entry]
    var drafts: [SavedDraft]?
  }
  @Published var draft = Draft() { didSet { scheduleSave() } }
  @Published private(set) var savedDrafts: [SavedDraft] = []
  var draftEntries: [SavedDraft] {
    (draft.hasContent ? [SavedDraft(id: "current", content: draft)] : []) + savedDrafts
  }
  @Published private(set) var entries: [Entry] = []
  var visibleEntries: [Entry] { entries.filter { $0.hidden != true } }
  @Published private(set) var error = ""
  @Published private(set) var ready = false
  private var identity: PhoneIdentity?
  private var owner = ""
  private var modules: [String] = []
  private var writable = true
  private var restoring = false
  private var active = false
  private var task: Task<Void, Never>?
  private var saveTask: Task<Void, Never>?
  private var generation = UUID()
  private let storageURL: URL?
  init(url: URL? = nil) { storageURL = url }
  private var file: URL {
    storageURL ?? FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0]
      .appendingPathComponent("sms-outbox.json")
  }

  func bind(_ identity: PhoneIdentity) {
    self.identity = identity
    let record = try? identity.status()
    let p = record?.tokens?.principal
    let next = p.map { (record?.relay ?? "") + "|" + $0.hub_id + "|" + $0.id } ?? ""
    if owner != next {
      task?.cancel(); task = nil; saveTask?.cancel(); saveTask = nil
      generation = UUID(); owner = next; restoring = true
      entries = []; draft = Draft(); savedDrafts = []; writable = true; error = ""
      if !next.isEmpty && FileManager.default.fileExists(atPath: file.path) {
        do {
          let data = try Data(contentsOf: file)
          guard data.count <= 4_000_000 else { throw PhoneError.invalidData }
          let saved = try JSONDecoder().decode(Saved.self, from: data)
          if saved.owner == next {
            guard saved.entries.count <= 200, Set(saved.entries.map(\.id)).count == saved.entries.count,
              saved.draft.message.utf8.count <= 16384, saved.draft.phone.count <= 128 else { throw PhoneError.invalidData }
            for entry in saved.entries { try entry.request.validate() }
            let drafts = saved.drafts ?? []
            guard drafts.count <= 100, Set(drafts.map(\.id)).count == drafts.count,
              drafts.allSatisfy({ $0.id != "current" && $0.content.message.utf8.count <= 16384 && $0.content.phone.count <= 128 })
              else { throw PhoneError.invalidData }
            entries = saved.entries; draft = saved.draft; savedDrafts = drafts
          }
        } catch { writable = false; self.error = "发件记录无法读取，已停止发送以保护原记录" }
      }
      restoring = false
    }
    modules = p?.modules ?? []
    ready = record?.phase == "ready" && p != nil && writable
  }

  func activate() { active = true; poll() }
  func background() { active = false; saveTask?.cancel(); _ = save() }

  func edit(phone: String? = nil, module: String? = nil, message: String? = nil) {
    if let phone { draft.phone = phone }
    if let module, modules.contains(module) { draft.module = module }
    if let message { draft.message = message }
    if !modules.contains(draft.module) { draft.module = modules.first ?? "" }
  }

  // Save the current editor before starting a reply or another message.
  @discardableResult func startDraft(phone: String = "", module: String? = nil, message: String = "") -> Bool {
    guard saveDraft() else { return false }
    edit(phone: phone, module: module, message: message)
    return true
  }

  @discardableResult func saveDraft() -> Bool {
    let previous = draft, previousDrafts = savedDrafts
    if draft.hasContent {
      guard savedDrafts.count < 100 else { error = "草稿箱已满，请删除部分草稿后重试"; return false }
      savedDrafts.insert(SavedDraft(content: draft), at: 0)
    }
    draft = Draft(module: draft.module)
    guard save() else { draft = previous; savedDrafts = previousDrafts; return false }
    error = ""
    return true
  }

  @discardableResult func restoreDraft(_ id: String) -> Bool {
    if id == "current" { return draft.hasContent }
    guard let entry = savedDrafts.first(where: { $0.id == id }) else { return false }
    let previous = draft, previousDrafts = savedDrafts
    savedDrafts.removeAll { $0.id == id }
    if draft.hasContent { savedDrafts.insert(SavedDraft(content: draft), at: 0) }
    draft = entry.content
    guard save() else { draft = previous; savedDrafts = previousDrafts; return false }
    error = ""
    return true
  }

  @discardableResult func deleteDraft(_ id: String) -> Bool {
    let previous = draft, previousDrafts = savedDrafts
    if id == "current" { draft = Draft(module: draft.module) }
    else { savedDrafts.removeAll { $0.id == id } }
    guard save() else { draft = previous; savedDrafts = previousDrafts; return false }
    return true
  }

  @discardableResult func enqueue() -> Bool {
    guard ready, modules.contains(draft.module), entries.filter(\.pending).count < 20 else {
      error = "请检查配对和线路，或等待现有短信发送完成"; return false
    }
    do {
      let request = try OutgoingSMS(module: draft.module, phone: draft.phone, message: draft.message)
      let previous = entries
      entries.insert(Entry(request: request), at: 0)
      // Keep all in-flight records; losing their request IDs could cause duplicates.
      while entries.count > 200, let index = entries.lastIndex(where: { !$0.pending }) { entries.remove(at: index) }
      let previousDraft = draft
      draft = Draft(module: draft.module)
      guard save() else { entries = previous; draft = previousDraft; return false }
      error = ""
      active = true; poll()
      return true
    } catch { self.error = "请填写有效号码和正文，每次最多发送 10 段短信"; return false }
  }

  func refresh(_ id: String) {
    // A refresh always reuses the original request, never creates a new SMS.
    guard let i = entries.firstIndex(where: { $0.id == id }), entries[i].state != "sent", entries[i].state != "failed" else { return }
    entries[i].state = "pending"; _ = save(); active = true; poll()
  }

  @discardableResult func delete(_ id: String) -> Bool {
    guard let index = entries.firstIndex(where: { $0.id == id }) else { return false }
    let previous = entries
    if entries[index].pending || entries[index].state == "unknown" {
      // Deleting a row does not cancel a submitted SMS. Keep the original ID
      // until its result is known, including after an app restart.
      entries[index].hidden = true
    } else {
      entries.remove(at: index)
    }
    guard save() else { entries = previous; return false }
    return true
  }

  private func poll() {
    guard active, ready, task == nil, let identity, entries.contains(where: \.pending) else { return }
    let current = generation
    task = Task { [weak self] in
      guard let self else { return }
      defer { if self.generation == current { self.task = nil } }
      while self.active && self.ready && !Task.isCancelled && self.generation == current {
        let ids = self.entries.filter(\.pending).map(\.id)
        if ids.isEmpty { return }
        for id in ids {
          guard self.active, !Task.isCancelled, self.generation == current,
            let index = self.entries.firstIndex(where: { $0.id == id }) else { return }
          let request = self.entries[index].request
          do {
            let result = try await identity.sendSMS(request)
            guard !Task.isCancelled, self.generation == current,
              let i = self.entries.firstIndex(where: { $0.id == id }) else { return }
            self.entries[i].state = result.state; self.entries[i].segments = result.segments; self.entries[i].code = result.code
            if self.entries[i].hidden == true && ["sent", "failed"].contains(result.state) {
              self.entries.remove(at: i)
            }
            self.error = ""; _ = self.save()
          } catch {
            guard self.generation == current else { return }
            if let failure = error as? RelayHTTPFailure, failure.statusCode == 404 {
              self.error = "Relay 尚未支持短信发送，请升级 Relay 和 Hub；原请求已保留"
            } else { self.error = "暂时无法确认发送结果；恢复网络后自动核对原请求，请勿重复新建" }
          }
          do { try await Task.sleep(for: .seconds(3)) } catch { return }
        }
      }
    }
  }

  private func scheduleSave() {
    guard !restoring else { return }
    saveTask?.cancel()
    saveTask = Task { [weak self] in
      do { try await Task.sleep(for: .milliseconds(400)) } catch { return }
      _ = self?.save()
    }
  }
  @discardableResult private func save() -> Bool {
    guard writable, !owner.isEmpty else { return false }
    do {
      try FileManager.default.createDirectory(at: file.deletingLastPathComponent(), withIntermediateDirectories: true)
      try JSONEncoder().encode(Saved(owner: owner, draft: draft, entries: entries, drafts: savedDrafts)).write(to: file,
        options: [.atomic, .completeFileProtectionUntilFirstUserAuthentication])
      var url = file; var values = URLResourceValues(); values.isExcludedFromBackup = true; try url.setResourceValues(values)
      return true
    } catch { self.error = "发件记录保存失败，请检查设备存储后重试"; return false }
  }
  func forget() {
    task?.cancel(); task = nil; saveTask?.cancel(); saveTask = nil
    generation = UUID(); restoring = true
    entries = []; draft = Draft(); savedDrafts = []; owner = ""; ready = false; identity = nil; active = false
    restoring = false
    try? FileManager.default.removeItem(at: file)
  }
}

struct PhoneSMSComposeView: View {
  @ObservedObject var runtime: PhoneRuntime
  @ObservedObject var outbox = PhoneSMSOutbox.shared
  @Environment(\.dismiss) private var dismiss
  var saved: () -> Void
  var submitted: () -> Void
  private var segments: Int { OutgoingSMS.segmentCount(outbox.draft.message) }
  private var valid: Bool {
    outbox.ready && runtime.modules.contains(outbox.draft.module) &&
      OutgoingSMS.normalizePhone(outbox.draft.phone) != nil && (1...10).contains(segments) &&
      !outbox.draft.message.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty
  }
  var body: some View {
    NavigationStack {
      Form {
        Section("发送线路") {
          Picker("SIM", selection: $outbox.draft.module) {
            ForEach(runtime.modules, id: \.self) { id in Text(runtime.moduleName(for: id)).tag(id) }
          }
          Text("需在 Hub 开启该线路的 APP 接听，并将目标设为此手机。").font(.caption).foregroundStyle(.secondary)
        }
        Section("收件人") {
          TextField("手机号或运营商服务号码", text: $outbox.draft.phone).keyboardType(.phonePad)
            .textContentType(.telephoneNumber).onChange(of: outbox.draft.phone) { _, value in
              if value.count > 128 { outbox.draft.phone = String(value.prefix(128)) }
            }
          if let number = OutgoingSMS.normalizePhone(outbox.draft.phone) {
            Text("将发送至：\(number)").font(.caption).foregroundStyle(.secondary)
          }
          Text("中国大陆 11 位手机号自动添加 +86；服务号码保留原样。其他国际号码请填写国家区号。")
            .font(.caption).foregroundStyle(.secondary)
        }
        Section("短信正文") {
          TextEditor(text: $outbox.draft.message).frame(minHeight: 180).accessibilityLabel("短信正文")
            .onChange(of: outbox.draft.message) { _, value in
              if value.count > 1600 { outbox.draft.message = String(value.prefix(1600)) }
            }
          Text("\(outbox.draft.message.utf16.count) 字符 · 预计 \(segments) 段 / 最多 10 段")
            .font(.caption).foregroundStyle(segments > 10 ? .red : .secondary)
          Text("短信通过所选 SIM 发送，费用按运营商套餐计算；发送成功表示运营商已接受，不代表对方已读。")
            .font(.caption).foregroundStyle(.secondary)
        }
        if !outbox.error.isEmpty { Text(outbox.error).foregroundStyle(.red).font(.caption) }
      }.navigationTitle("新短信").navigationBarTitleDisplayMode(.inline)
        .toolbar {
          ToolbarItem(placement: .cancellationAction) {
            Button("保存草稿") { if outbox.saveDraft() { saved(); dismiss() } }
          }
          ToolbarItem(placement: .confirmationAction) {
            Button("发送") { if outbox.enqueue() { submitted(); dismiss() } }.disabled(!valid)
          }
        }
    }.tint(PhoneTheme.violet)
  }
}

struct PhoneSMSOutgoingList: View {
  @ObservedObject var runtime: PhoneRuntime
  @ObservedObject var outbox = PhoneSMSOutbox.shared
  var entries: [PhoneSMSOutbox.Entry]
  var edit: () -> Void
  var body: some View {
    if outbox.visibleEntries.isEmpty {
      EmptyPhoneState(icon: "paperplane", title: "暂无发件记录", message: "点击新短信，使用 Hub 上的 SIM 发送")
    }
    ForEach(entries) { entry in
        VStack(alignment: .leading, spacing: 8) {
          HStack {
            Text("发给 \(entry.request.phone)").font(.headline)
            Spacer()
            Text(runtime.moduleName(for: entry.request.module_id)).font(.caption).foregroundStyle(PhoneTheme.violet)
          }
          Text(entry.request.message).textSelection(.enabled)
          Text(entry.summary).font(.caption).foregroundStyle(entry.state == "sent" ? PhoneTheme.green : PhoneTheme.secondary)
          Text(entry.request.created_at, format: .dateTime.year().month().day().hour().minute()).font(.caption2).foregroundStyle(PhoneTheme.secondary)
          if entry.state == "failed" {
            Button("编辑后重新发送") {
              if outbox.startDraft(phone: entry.request.phone, module: entry.request.module_id, message: entry.request.message) { edit() }
            }.font(.caption)
          } else if entry.state == "unknown" || entry.pending {
            Button("核对发送结果") { outbox.refresh(entry.id) }.font(.caption)
          }
        }.frame(maxWidth: .infinity, alignment: .leading).padding(14).phoneGlass(radius: 20)
          .buttonStyle(.borderless)
          .swipeActions(edge: .trailing, allowsFullSwipe: true) {
            Button("删除", role: .destructive) { outbox.delete(entry.id) }
              .labelStyle(.titleAndIcon)
          }
      }
    Text("左划可删除本机记录；删除不会撤回或取消已经提交的短信。")
      .font(.caption2).foregroundStyle(PhoneTheme.secondary)
  }
}
