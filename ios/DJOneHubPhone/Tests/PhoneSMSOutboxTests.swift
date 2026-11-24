import DJOneHubPhoneCore
import XCTest
@testable import DJOneHubPhone

@MainActor private final class SMSVault: IdentityVault {
  var data: Data?
  func load() throws -> Data? { data }
  func create(_ data: Data) throws { self.data = data }
  func replace(_ data: Data) throws { self.data = data }
}

@MainActor private final class SMSHTTP: IdentityHTTP {
  var ids: [String] = []
  var dropResponse = true
  var beforeRequest: (() throws -> Void)?
  func post(origin: URL, path: String, body: Data, accessToken: String?) async throws -> Data {
    XCTAssertEqual(path, "/v1/sms/outgoing")
    XCTAssertEqual(accessToken, String(repeating: "a", count: 43))
    try beforeRequest?()
    let request = try Wire.decoder().decode(OutgoingSMS.self, from: body)
    ids.append(request.id)
    if dropResponse { throw PhoneError.unavailable }
    return try JSONSerialization.data(withJSONObject: ["id": request.id, "installation_id": "phone",
      "state": "sent", "segments": 1, "code": ""])
  }
}

@MainActor final class PhoneSMSOutboxTests: XCTestCase {
  private func identity(_ http: SMSHTTP) throws -> PhoneIdentity {
    let vault = SMSVault()
    let formatter = ISO8601DateFormatter()
    vault.data = try JSONSerialization.data(withJSONObject: ["version": 1, "relay": "https://relay.example",
      "seed": Data(repeating: 1, count: 32).base64EncodedString(), "phase": "ready", "tokens": [
        "access_token": String(repeating: "a", count: 43), "refresh_token": String(repeating: "r", count: 43),
        "refresh_expires_at": formatter.string(from: Date().addingTimeInterval(3600)),
        "principal": ["id": "phone", "hub_id": "hub", "tenant": "tenant", "kind": "installation", "modules": ["module"],
          "version": 1, "access_expires_at": formatter.string(from: Date().addingTimeInterval(900))]]])
    return PhoneIdentity(vault: vault, http: http)
  }

  func testDraftAndPendingRequestSurviveRestartWithoutNewID() async throws {
    let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
    defer { try? FileManager.default.removeItem(at: directory) }
    let url = directory.appendingPathComponent("outbox.json")
    let http = SMSHTTP(), credentials = try identity(http)
    let first = PhoneSMSOutbox(url: url)
    first.bind(credentials)
    first.edit(phone: "10086", module: "module", message: "测试短信")
    first.background()
    let restored = PhoneSMSOutbox(url: url)
    restored.bind(credentials)
    XCTAssertEqual(restored.draft.message, "测试短信")
    http.beforeRequest = {
      let saved = try JSONSerialization.jsonObject(with: Data(contentsOf: url)) as! [String: Any]
      XCTAssertEqual((saved["entries"] as? [Any])?.count, 1, "must persist before HTTP")
    }
    XCTAssertTrue(restored.enqueue())
    for _ in 0..<100 where http.ids.isEmpty { try await Task.sleep(for: .milliseconds(10)) }
    restored.background()
    XCTAssertEqual(http.ids.count, 1)
    let pendingID = try XCTUnwrap(restored.entries.first?.id)
    http.dropResponse = false
    let restarted = PhoneSMSOutbox(url: url)
    restarted.bind(credentials)
    XCTAssertEqual(restarted.entries.first?.id, pendingID)
    restarted.activate()
    for _ in 0..<100 where restarted.entries.first?.state != "sent" { try await Task.sleep(for: .milliseconds(10)) }
    restarted.background()
    XCTAssertEqual(restarted.entries.first?.state, "sent")
    XCTAssertEqual(http.ids, [pendingID, pendingID])
  }

  func testPersistenceFailurePreventsNetworkSubmission() throws {
    let parent = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
    try Data("not a directory".utf8).write(to: parent)
    defer { try? FileManager.default.removeItem(at: parent) }
    let http = SMSHTTP(), credentials = try identity(http)
    let outbox = PhoneSMSOutbox(url: parent.appendingPathComponent("outbox.json"))
    outbox.bind(credentials)
    outbox.edit(phone: "10086", module: "module", message: "测试")
    XCTAssertFalse(outbox.enqueue())
    XCTAssertTrue(outbox.entries.isEmpty)
    XCTAssertTrue(http.ids.isEmpty)
    outbox.background()
  }

  func testDeletingPendingSMSKeepsOriginalRequestAcrossRestart() async throws {
    let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
    defer { try? FileManager.default.removeItem(at: directory) }
    let url = directory.appendingPathComponent("outbox.json")
    let http = SMSHTTP(), credentials = try identity(http)
    let outbox = PhoneSMSOutbox(url: url)
    outbox.bind(credentials)
    outbox.edit(phone: "10086", module: "module", message: "删除测试")
    XCTAssertTrue(outbox.enqueue())
    let id = try XCTUnwrap(outbox.entries.first?.id)
    // Delete before the first HTTP response can arrive.
    XCTAssertTrue(outbox.delete(id))
    XCTAssertTrue(outbox.visibleEntries.isEmpty)
    for _ in 0..<100 where http.ids.isEmpty { try await Task.sleep(for: .milliseconds(10)) }
    outbox.background()
    XCTAssertEqual(http.ids, [id])

    let restarted = PhoneSMSOutbox(url: url)
    restarted.bind(credentials)
    XCTAssertTrue(restarted.visibleEntries.isEmpty)
    XCTAssertEqual(restarted.entries.first?.id, id)
    http.dropResponse = false
    restarted.activate()
    for _ in 0..<100 where !restarted.entries.isEmpty { try await Task.sleep(for: .milliseconds(10)) }
    restarted.background()
    XCTAssertTrue(restarted.entries.isEmpty, "confirmed hidden requests should be removed")
    XCTAssertTrue(restarted.visibleEntries.isEmpty, "a late result must not recreate the row")
    XCTAssertEqual(http.ids, [id, id])
    let final = PhoneSMSOutbox(url: url)
    final.bind(credentials)
    XCTAssertTrue(final.entries.isEmpty)
  }

  func testDeleteCompletedAndUnknownRecordsAndReadLegacyFormat() throws {
    let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
    defer { try? FileManager.default.removeItem(at: directory) }
    try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
    let url = directory.appendingPathComponent("outbox.json")
    let http = SMSHTTP(), credentials = try identity(http)
    for state in ["sent", "failed", "unknown"] {
      let request = try OutgoingSMS(module: "module", phone: "10086", message: "历史短信")
      let entry = PhoneSMSOutbox.Entry(request: request, state: state)
      // nil hidden is omitted by Codable, just like the original file format.
      let item = try JSONSerialization.jsonObject(with: JSONEncoder().encode(entry))
      let data = try JSONSerialization.data(withJSONObject: ["owner": "https://relay.example|hub|phone",
        "draft": ["module": "module", "phone": "", "message": ""], "entries": [item]])
      try data.write(to: url)
      let outbox = PhoneSMSOutbox(url: url)
      outbox.bind(credentials)
      XCTAssertEqual(outbox.visibleEntries.count, 1)
      XCTAssertTrue(outbox.delete(request.id))
      let restarted = PhoneSMSOutbox(url: url)
      restarted.bind(credentials)
      XCTAssertTrue(restarted.visibleEntries.isEmpty)
      XCTAssertEqual(restarted.entries.count, state == "unknown" ? 1 : 0)
    }
    XCTAssertTrue(http.ids.isEmpty)
  }

  private func notification(_ id: String) -> [AnyHashable: Any] {
    ["aps": ["alert": ["body": "收到的短信"]], "sms": ["id": id, "hub_id": "hub",
      "installation_id": "phone", "module_id": "module", "sender": "10086", "content": "",
      "timestamp": "2026-11-07T10:00:00Z"]]
  }

  func testInboxDeletionSurvivesRestartAndNotificationReimport() throws {
    let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
    defer { try? FileManager.default.removeItem(at: directory) }
    let url = directory.appendingPathComponent("inbox.json")
    let credentials = try identity(SMSHTTP())
    let firstID = String(repeating: "a", count: 64), secondID = String(repeating: "b", count: 64)
    let inbox = PhoneSMSInbox(url: url)
    inbox.bind(identity: credentials, environment: "sandbox")
    XCTAssertEqual(inbox.receive(notification(firstID)), firstID)
    XCTAssertEqual(inbox.receive(notification(secondID)), secondID)
    // Exercise migration of pre-deletion array storage.
    try JSONEncoder().encode(inbox.messages).write(to: url)
    let migrated = PhoneSMSInbox(url: url)
    migrated.bind(identity: credentials, environment: "sandbox")
    XCTAssertEqual(migrated.messages.count, 2)
    XCTAssertTrue(migrated.delete(firstID))
    XCTAssertEqual(migrated.messages.map(\.id), [secondID])
    let restarted = PhoneSMSInbox(url: url)
    restarted.bind(identity: credentials, environment: "sandbox")
    XCTAssertNil(restarted.receive(notification(firstID)))
    XCTAssertEqual(restarted.receive(notification(secondID)), secondID)
    XCTAssertEqual(restarted.messages.map(\.id), [secondID])
    restarted.clear()
    XCTAssertNil(restarted.receive(notification(secondID)))
  }

  func testFailedDeletionKeepsRowsVisible() throws {
    let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
    defer { try? FileManager.default.removeItem(at: directory) }
    let credentials = try identity(SMSHTTP())
    let inbox = PhoneSMSInbox(url: directory.appendingPathComponent("inbox.json"))
    inbox.bind(identity: credentials, environment: "sandbox")
    let id = String(repeating: "a", count: 64)
    XCTAssertEqual(inbox.receive(notification(id)), id)
    let outbox = PhoneSMSOutbox(url: directory.appendingPathComponent("outbox.json"))
    outbox.bind(credentials)
    outbox.edit(phone: "10086", module: "module", message: "测试")
    XCTAssertTrue(outbox.enqueue())
    outbox.background()
    let outgoingID = try XCTUnwrap(outbox.entries.first?.id)
    try FileManager.default.removeItem(at: directory)
    try Data("not a directory".utf8).write(to: directory)
    XCTAssertFalse(inbox.delete(id))
    XCTAssertEqual(inbox.messages.map(\.id), [id])
    XCTAssertFalse(outbox.delete(outgoingID))
    XCTAssertEqual(outbox.visibleEntries.map(\.id), [outgoingID])
  }

  func testSavedDraftsRestoreAndRepliesPreserveExistingWork() throws {
    let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
    defer { try? FileManager.default.removeItem(at: directory) }
    let url = directory.appendingPathComponent("outbox.json")
    let http = SMSHTTP(), credentials = try identity(http)
    let outbox = PhoneSMSOutbox(url: url)
    outbox.bind(credentials)
    outbox.edit(phone: "10086", module: "module", message: "原有草稿")
    XCTAssertTrue(outbox.startDraft(phone: "10010", module: "module", message: "回复内容"))
    XCTAssertEqual(outbox.savedDrafts.first?.content.message, "原有草稿")
    XCTAssertTrue(outbox.saveDraft())
    XCTAssertFalse(outbox.draft.hasContent)
    XCTAssertEqual(outbox.draftEntries.count, 2)

    let restored = PhoneSMSOutbox(url: url)
    restored.bind(credentials)
    let original = try XCTUnwrap(restored.savedDrafts.first(where: { $0.content.message == "原有草稿" }))
    XCTAssertTrue(restored.restoreDraft(original.id))
    XCTAssertEqual(restored.draft, original.content)
    XCTAssertEqual(restored.savedDrafts.count, 1)
    XCTAssertTrue(restored.enqueue())
    restored.background()
    let restarted = PhoneSMSOutbox(url: url)
    restarted.bind(credentials)
    XCTAssertFalse(restarted.draft.hasContent, "queued messages must not remain as drafts")
    XCTAssertEqual(restarted.savedDrafts.first?.content.message, "回复内容")
    XCTAssertTrue(restarted.deleteDraft(try XCTUnwrap(restarted.savedDrafts.first?.id)))
    let final = PhoneSMSOutbox(url: url)
    final.bind(credentials)
    XCTAssertTrue(final.draftEntries.isEmpty)
    XCTAssertTrue(http.ids.isEmpty, "draft actions do not submit SMS")
  }

  func testLegacyActiveDraftAppearsInDraftBoxAndFailedSaveKeepsContent() throws {
    let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
    defer { try? FileManager.default.removeItem(at: directory) }
    try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
    let url = directory.appendingPathComponent("outbox.json")
    let credentials = try identity(SMSHTTP())
    try JSONSerialization.data(withJSONObject: ["owner": "https://relay.example|hub|phone",
      "draft": ["module": "module", "phone": "10086", "message": "旧版草稿"], "entries": []]).write(to: url)
    let outbox = PhoneSMSOutbox(url: url)
    outbox.bind(credentials)
    XCTAssertEqual(outbox.draftEntries.first?.content.message, "旧版草稿")
    XCTAssertTrue(outbox.restoreDraft("current"))
    try FileManager.default.removeItem(at: directory)
    try Data("not a directory".utf8).write(to: directory)
    XCTAssertFalse(outbox.saveDraft())
    XCTAssertFalse(outbox.startDraft(phone: "10010", message: "覆盖内容"))
    XCTAssertFalse(outbox.deleteDraft("current"))
    XCTAssertEqual(outbox.draft.message, "旧版草稿")
    XCTAssertTrue(outbox.savedDrafts.isEmpty)
    outbox.background()
  }

  func testSegmentSummaryHidesSinglePartAndShowsSubmittedOverTotal() throws {
    let single = try OutgoingSMS(module: "module", phone: "10086", message: "测试")
    XCTAssertEqual(PhoneSMSOutbox.Entry(request: single, state: "sent", segments: 1).summary, "已提交运营商")
    let multipart = try OutgoingSMS(module: "module", phone: "10086", message: String(repeating: "测", count: 150))
    XCTAssertEqual(PhoneSMSOutbox.Entry(request: multipart, state: "sent", segments: 3).summary, "已提交运营商 · 3/3")
    XCTAssertEqual(PhoneSMSOutbox.Entry(request: multipart, state: "unknown", segments: 1).summary, "结果待确认；未自动重发 · 1/3")
    XCTAssertEqual(PhoneSMSOutbox.Entry(request: multipart, state: "queued").summary, "等待 Hub 发送 · 0/3")
  }

  func testSMSListSearchesBeforePagingAndClampsAfterDeletion() {
    let items = Array(1...45)
    let first = PhoneSMSPage(items, query: "", page: 1) { [String($0)] }
    let second = PhoneSMSPage(items, query: "", page: 2) { [String($0)] }
    let last = PhoneSMSPage(items, query: "", page: 3) { [String($0)] }
    XCTAssertEqual(first.items, Array(1...20))
    XCTAssertEqual(second.items, Array(21...40))
    XCTAssertEqual(last.items, Array(41...45))
    let search = PhoneSMSPage(items, query: "  45 \n", page: 3) { [String($0), "线路"] }
    XCTAssertEqual(search.items, [45])
    XCTAssertEqual(search.number, 1)
    XCTAssertEqual(PhoneSMSPage(items, query: "线路", page: 1) { _ in ["线路"] }.matches.count, 45)
    XCTAssertEqual(PhoneSMSPage(Array(items.prefix(20)), query: "", page: 3) { [String($0)] }.number, 1)
    let empty = PhoneSMSPage(items, query: "不存在", page: 2) { [String($0)] }
    XCTAssertEqual(empty.count, 1)
    XCTAssertTrue(empty.items.isEmpty)
  }
}
