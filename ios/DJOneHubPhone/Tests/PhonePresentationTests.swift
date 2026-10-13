import DJOneHubPhoneCore
import CallKit
import XCTest

@testable import DJOneHubPhone

@MainActor final class PhonePresentationTests: XCTestCase {
  func testIncomingCallKitShowsModuleRemarkAndKeepsPhoneHandle() {
    let update = incomingCallUpdate(display: "13800138000", moduleName: "移动主卡")
    XCTAssertEqual(update.localizedCallerName, "13800138000 · 移动主卡")
    XCTAssertEqual(update.remoteHandle?.type, .phoneNumber)
    XCTAssertEqual(update.remoteHandle?.value, "13800138000")
    XCTAssertFalse(update.supportsDTMF)
  }

  func testPushCallerAndCachedModuleNameProduceSameCallKitDisplay() throws {
    var signal = Signal(type: "invite", payload: [
      "display": .string("10086"),
      "deadline": .string(ISO8601DateFormatter().string(from: Date().addingTimeInterval(45)))])
    signal.hub_epoch = "epoch"
    signal.module_id = "module-b"
    signal.call_uuid = UUID().uuidString.lowercased()
    signal.installation_id = "phone"
    signal.generation = 1
    let object = try JSONSerialization.jsonObject(with: signal.encoded())
    let hint = try PushInvite.decode(JSONSerialization.data(withJSONObject: ["aps": [:], "invite": object]))
    let line = ModuleLineDisplay.name(for: hint.binding.moduleID,
      names: ["module-a": "主卡", "module-b": "主卡"], modules: ["module-a", "module-b"])
    let update = incomingCallUpdate(display: hint.signal.payload["display"]?.string, moduleName: line)
    XCTAssertEqual(update.localizedCallerName, "10086 · 主卡（module-b）")
    XCTAssertEqual(update.remoteHandle?.value, "10086")
    let fallback = incomingCallUpdate(display: nil, moduleName: "module-b")
    XCTAssertEqual(fallback.localizedCallerName, "DJOneHub · module-b")
    XCTAssertEqual(fallback.remoteHandle?.type, .generic)
    XCTAssertEqual(fallback.remoteHandle?.value, "DJOneHub")
  }

  func testIncomingCallKitUsesCallerPhoneNumber() {
    let update = incomingCallUpdate(display: "+86 (138) 0013-8000")
    XCTAssertEqual(update.remoteHandle?.type, .phoneNumber)
    XCTAssertEqual(update.remoteHandle?.value, "+8613800138000")
    XCTAssertEqual(update.localizedCallerName, "+86 (138) 0013-8000")
    XCTAssertFalse(update.hasVideo)
    XCTAssertFalse(update.supportsDTMF)
  }

  func testPushInviteCarriesCallerIntoCallKitUpdate() throws {
    var signal = Signal(type: "invite", payload: [
      "display": .string("13800138000"),
      "deadline": .string(ISO8601DateFormatter().string(from: Date().addingTimeInterval(45)))])
    signal.hub_epoch = "epoch"
    signal.module_id = "module"
    signal.call_uuid = UUID().uuidString.lowercased()
    signal.installation_id = "phone"
    signal.generation = 1
    let object = try JSONSerialization.jsonObject(with: signal.encoded())
    let hint = try PushInvite.decode(JSONSerialization.data(withJSONObject: ["aps": [:], "invite": object]))
    let update = incomingCallUpdate(display: hint.signal.payload["display"]?.string)
    XCTAssertEqual(update.localizedCallerName, "13800138000")
    XCTAssertEqual(update.remoteHandle?.type, .phoneNumber)
    XCTAssertEqual(update.remoteHandle?.value, "13800138000")
  }

  func testIncomingCallKitPreservesModuleAndUnknownCaller() {
    let module = incomingCallUpdate(display: "dji-module-a")
    XCTAssertEqual(module.remoteHandle?.type, .generic)
    XCTAssertEqual(module.localizedCallerName, "dji-module-a")
    let unknown = incomingCallUpdate(display: nil)
    XCTAssertEqual(unknown.remoteHandle?.type, .generic)
    XCTAssertEqual(unknown.remoteHandle?.value, "DJOneHub")
    XCTAssertEqual(unknown.localizedCallerName, "DJOneHub")
  }

  private func storageURL() -> URL {
    FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString).appendingPathComponent("contacts.json")
  }

  func testContactsNormalizeMergeAndPersistFavorites() throws {
    let url = storageURL()
    defer { try? FileManager.default.removeItem(at: url.deletingLastPathComponent()) }
    let store = PhoneContactsStore(url: url)
    XCTAssertTrue(store.save(PhoneContact(name: " 妈妈 ", number: "+86 (138) 0013-8000", favorite: true)))
    XCTAssertTrue(store.save(PhoneContact(name: "妈妈", number: "+8613800138000", favorite: true)))
    XCTAssertEqual(store.saved.count, 1)
    let restored = PhoneContactsStore(url: url)
    XCTAssertEqual(restored.saved, store.saved)
    XCTAssertEqual(restored.name(for: "+86 138 0013 8000"), "妈妈")
    XCTAssertTrue(restored.saved[0].favorite)
    XCTAssertEqual(try url.resourceValues(forKeys: [.isExcludedFromBackupKey]).isExcludedFromBackup, true)
  }

  func testHistoryAggregationExcludesNonNumbersAndKeepsSavedName() {
    let url = storageURL()
    defer { try? FileManager.default.removeItem(at: url.deletingLastPathComponent()) }
    let store = PhoneContactsStore(url: url)
    var history = CallHistory()
    history.begin(id: UUID(), module: "voice", display: "138 0013 8000", outgoing: true)
    history.begin(id: UUID(), module: "voice", display: "13800138000", outgoing: false)
    history.begin(id: UUID(), module: "voice", display: "DJOneHub", outgoing: false)
    XCTAssertEqual(store.all(history: history).map(\.number), ["13800138000"])
    XCTAssertTrue(store.saved.isEmpty, "History contacts must not be copied into permanent storage")
    XCTAssertTrue(store.save(PhoneContact(name: "妈妈", number: "13800138000", favorite: true)))
    XCTAssertEqual(store.all(history: history).count, 1)
    XCTAssertEqual(store.all(history: history).first?.name, "妈妈")
  }

  func testEditingNumberRemovesOldSavedNumberAndRejectsInvalidInput() {
    let url = storageURL()
    defer { try? FileManager.default.removeItem(at: url.deletingLastPathComponent()) }
    let store = PhoneContactsStore(url: url)
    XCTAssertTrue(store.save(PhoneContact(name: "家人", number: "13800138000")))
    XCTAssertTrue(store.save(PhoneContact(name: "家人", number: "13900139000"), replacing: "13800138000"))
    XCTAssertEqual(store.saved.map(\.number), ["13900139000"])
    XCTAssertFalse(store.save(PhoneContact(name: "无效", number: "DJOneHub")))
    XCTAssertEqual(store.saved.count, 1)
  }

  func testCorruptContactFileIsNeverOverwritten() throws {
    let url = storageURL()
    defer { try? FileManager.default.removeItem(at: url.deletingLastPathComponent()) }
    try FileManager.default.createDirectory(at: url.deletingLastPathComponent(), withIntermediateDirectories: true)
    let original = Data("corrupt contacts".utf8)
    try original.write(to: url)
    let store = PhoneContactsStore(url: url)
    XCTAssertFalse(store.error.isEmpty)
    XCTAssertFalse(store.save(PhoneContact(name: "妈妈", number: "13800138000")))
    XCTAssertEqual(try Data(contentsOf: url), original)
  }

  func testMissedFilterExcludesActiveOutgoingAndDeclinedCalls() {
    let start = Date(timeIntervalSince1970: 1000)
    var history = CallHistory()
    let incoming = UUID(), declined = UUID(), outgoing = UUID(), active = UUID(), answered = UUID()
    for (id, isOutgoing) in [(incoming, false), (declined, false), (outgoing, true), (active, false), (answered, false)] {
      history.begin(id: id, module: "voice", display: "13800138000", outgoing: isOutgoing, now: start)
    }
    history.finish(id: incoming, result: "来电未接听", now: start.addingTimeInterval(30))
    history.finish(id: declined, result: "已拒接", now: start.addingTimeInterval(10))
    history.finish(id: outgoing, result: "对方未接听", now: start.addingTimeInterval(30))
    history.connected(id: answered, now: start.addingTimeInterval(5))
    history.finish(id: answered, result: "已挂断", now: start.addingTimeInterval(70))
    XCTAssertEqual(history.entries.filter(\.isMissed).map(\.id), [incoming])
    XCTAssertEqual(history.entries.first(where: { $0.id == answered })?.summary, "呼入 · 已挂断 · 1 分 5 秒")
    XCTAssertEqual(history.entries.first(where: { $0.id == active })?.summary, "呼入 · 通话进行中")
  }
}
