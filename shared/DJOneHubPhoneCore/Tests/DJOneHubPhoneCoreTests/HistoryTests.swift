import XCTest
@testable import DJOneHubPhoneCore

final class HistoryTests: XCTestCase {
  func testDuplicateAndLateEventsDoNotChangeFinishedCall() throws {
    var h = CallHistory()
    let id = UUID(), t = Date(timeIntervalSince1970: 100)
    h.begin(id: id, module: "m", display: "123", outgoing: false, now: t)
    h.begin(id: id, module: "other", display: "other", outgoing: true)
    h.connected(id: id, now: t.addingTimeInterval(5))
    h.connected(id: id, now: t.addingTimeInterval(10))
    h.finish(id: id, result: "ended", now: t.addingTimeInterval(25))
    let entry = h.entries[0]
    h.finish(id: id, result: "late")
    h.connected(id: id)
    XCTAssertEqual(h.entries, [entry])
    XCTAssertEqual(entry.duration, 20)
    XCTAssertEqual(try CallHistory.restore(JSONEncoder().encode(h)).entries, [entry])
  }
  func testCrashRecoveryAndClearPreserveCurrentCall() throws {
    var h = CallHistory()
    let id = UUID()
    h.begin(id: id, module: "m", display: "123", outgoing: true)
    h.clearCompleted()
    XCTAssertEqual(h.entries.count, 1)
    var restored = try CallHistory.restore(JSONEncoder().encode(h))
    XCTAssertNotNil(restored.entries[0].ended)
    XCTAssertEqual(restored.entries[0].duration, 0)
    restored.clearCompleted()
    XCTAssertTrue(restored.entries.isEmpty)
    XCTAssertThrowsError(try CallHistory.restore(Data(repeating: 0, count: 262145)))
  }
  func testBoundAndUnknownEvent() {
    var h = CallHistory()
    for _ in 0..<110 { h.begin(id: UUID(), module: "m", display: "123", outgoing: false) }
    h.finish(id: UUID(), result: "unknown")
    XCTAssertEqual(h.entries.count, 100)
    XCTAssertTrue(h.entries.allSatisfy { $0.ended == nil })
  }

  func testDeletingOneCompletedCallPersistsAndKeepsOtherCallsToSameNumber() throws {
    var history = CallHistory()
    let deleted = UUID(), kept = UUID(), active = UUID()
    let start = Date(timeIntervalSince1970: 100)
    for id in [deleted, kept, active] {
      history.begin(id: id, module: "m", display: "10086", outgoing: true, now: start)
    }
    history.finish(id: deleted, result: "已挂断", now: start.addingTimeInterval(10))
    history.finish(id: kept, result: "已挂断", now: start.addingTimeInterval(20))
    XCTAssertTrue(history.deleteCompleted(deleted))
    XCTAssertEqual(history.entries.map(\.id), [active, kept])
    history.finish(id: deleted, result: "late")
    history.connected(id: deleted)
    let restored = try CallHistory.restore(JSONEncoder().encode(history))
    XCTAssertEqual(restored.entries.map(\.id), [active, kept])
    XCTAssertEqual(restored.entries.first(where: { $0.id == kept })?.ended, start.addingTimeInterval(20))
  }

  func testDeletingActiveOrMissingCallLeavesHistoryUntouched() {
    var history = CallHistory()
    let active = UUID()
    history.begin(id: active, module: "m", display: "10086", outgoing: false)
    history.connected(id: active)
    let previous = history.entries
    XCTAssertFalse(history.deleteCompleted(active))
    XCTAssertFalse(history.deleteCompleted(UUID()))
    XCTAssertEqual(history.entries, previous)
  }
}

extension HistoryTests {
  func testPushHintOnlyUpdatedByMatchingInviteAndNeverAfterEnd() {
    var h = CallHistory()
    let id = UUID()
    h.begin(id: id, module: "m", display: "DJOneHub", outgoing: false)
    h.identify(id: id, module: "wrong", display: "wrong")
    XCTAssertEqual(h.entries[0].display, "DJOneHub")
    h.identify(id: id, module: "m", display: "123")
    h.finish(id: id, result: "done")
    h.identify(id: id, module: "m", display: "late")
    XCTAssertEqual(h.entries[0].display, "123")
  }
}
