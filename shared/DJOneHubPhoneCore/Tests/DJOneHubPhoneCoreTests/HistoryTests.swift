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
