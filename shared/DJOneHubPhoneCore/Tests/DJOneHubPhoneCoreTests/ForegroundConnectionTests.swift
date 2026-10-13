import XCTest
@testable import DJOneHubPhoneCore

final class ForegroundConnectionTests: XCTestCase {
  func testForegroundConnectionRequiresReadyPairingAndPermission() {
    var intent = ForegroundConnectionIntent()
    XCTAssertFalse(intent.eligible(ready: true, microphoneGranted: true, busy: false))
    intent.activate()
    XCTAssertFalse(intent.eligible(ready: false, microphoneGranted: true, busy: false))
    XCTAssertFalse(intent.eligible(ready: true, microphoneGranted: false, busy: false))
    // Completing pairing or granting permission can connect in the same visit.
    XCTAssertTrue(intent.claim(generation: intent.generation, ready: true, microphoneGranted: true, busy: false))
  }
  func testDuplicateActiveEventsDoNotReconnectOrRepeatFailedAttempt() {
    var intent = ForegroundConnectionIntent()
    intent.activate()
    let generation = intent.generation
    XCTAssertTrue(intent.claim(generation: generation, ready: true, microphoneGranted: true, busy: false))
    intent.activate()
    XCTAssertEqual(intent.generation, generation)
    XCTAssertFalse(intent.claim(generation: generation, ready: true, microphoneGranted: true, busy: false))
    // A new foreground visit permits one new attempt after a failed connection.
    intent.deactivate(); intent.activate()
    XCTAssertTrue(intent.claim(generation: intent.generation, ready: true, microphoneGranted: true, busy: false))
  }
  func testBackgroundInvalidatesDelayedWorkAndReturnGetsNewIntent() {
    var intent = ForegroundConnectionIntent()
    intent.activate()
    let old = intent.generation
    intent.deactivate()
    XCTAssertFalse(intent.claim(generation: old, ready: true, microphoneGranted: true, busy: false))
    intent.activate()
    XCTAssertFalse(intent.claim(generation: old, ready: true, microphoneGranted: true, busy: false))
    XCTAssertTrue(intent.claim(generation: intent.generation, ready: true, microphoneGranted: true, busy: false))
  }
  func testExistingCallSocketOrCredentialWorkWaitsWithoutConsumingAttempt() {
    var intent = ForegroundConnectionIntent()
    intent.activate()
    let generation = intent.generation
    XCTAssertFalse(intent.claim(generation: generation, ready: true, microphoneGranted: true, busy: true))
    XCTAssertTrue(intent.claim(generation: generation, ready: true, microphoneGranted: true, busy: false))
  }
  func testExplicitPauseSurvivesForegroundChangesUntilResume() {
    var intent = ForegroundConnectionIntent()
    intent.activate()
    let old = intent.generation
    intent.pause()
    XCTAssertTrue(intent.paused)
    XCTAssertFalse(intent.claim(generation: old, ready: true, microphoneGranted: true, busy: false))
    intent.deactivate(); intent.activate()
    XCTAssertFalse(intent.eligible(ready: true, microphoneGranted: true, busy: false))
    intent.resume()
    XCTAssertFalse(intent.paused)
    XCTAssertTrue(intent.claim(generation: intent.generation, ready: true, microphoneGranted: true, busy: false))
  }
  func testManualRetryPreventsDuplicateAutomaticConnection() {
    var intent = ForegroundConnectionIntent()
    intent.activate()
    intent.manualAttempt()
    XCTAssertFalse(intent.eligible(ready: true, microphoneGranted: true, busy: false))
    intent.resume()
    intent.manualAttempt()
    XCTAssertFalse(intent.claim(generation: intent.generation, ready: true, microphoneGranted: true, busy: false))
  }
}
