import AVFAudio
import XCTest

@testable import DJOneHubPhone

@MainActor private final class RingbackSpy: RingbackPlaying {
  var plays = 0, stops = 0
  var succeeds = true
  func play() -> Bool { plays += 1; return succeeds }
  func stop() { stops += 1 }
}

final class PhoneRingbackTests: XCTestCase {
  @MainActor func testWaitsForCallKitAndDoesNotRestartForWaitingPhaseChanges() {
    let player = RingbackSpy()
    let tone = PhoneRingback(makePlayer: { player })
    tone.update(outgoing: true, phase: "dialing", audioActive: false)
    XCTAssertEqual(player.plays, 0)
    tone.update(outgoing: true, phase: "dialing", audioActive: true)
    tone.update(outgoing: true, phase: "offered", audioActive: true)
    tone.update(outgoing: true, phase: "answering", audioActive: true)
    XCTAssertEqual(player.plays, 1)
    tone.update(outgoing: true, phase: "awaiting_audio", audioActive: true)
    XCTAssertEqual(player.stops, 1)
    tone.update(outgoing: true, phase: "connected", audioActive: true)
    XCTAssertEqual(player.plays, 1)
  }

  @MainActor func testStopsOnCancellationFailureAndAudioDeactivation() {
    for terminal in ["ending", "ended", "rejecting", "audio_interrupted", ""] {
      let player = RingbackSpy()
      let tone = PhoneRingback(makePlayer: { player })
      tone.update(outgoing: true, phase: "answering", audioActive: true)
      tone.update(outgoing: true, phase: terminal, audioActive: true)
      tone.stop()
      XCTAssertEqual(player.stops, 1, terminal)
    }
    let player = RingbackSpy()
    let tone = PhoneRingback(makePlayer: { player })
    tone.update(outgoing: true, phase: "answering", audioActive: true)
    tone.update(outgoing: true, phase: "answering", audioActive: false)
    XCTAssertEqual(player.stops, 1)
    tone.update(outgoing: true, phase: "answering", audioActive: true)
    XCTAssertEqual(player.plays, 2)
    tone.update(outgoing: false, phase: "answering", audioActive: true)
    XCTAssertEqual(player.stops, 2)
  }

  @MainActor func testIncomingCallsNeverPlayAndPlaybackFailureIsReported() {
    let player = RingbackSpy()
    let tone = PhoneRingback(makePlayer: { player })
    for phase in ["incoming", "answering", "awaiting_audio", "connected"] {
      tone.update(outgoing: false, phase: phase, audioActive: true)
    }
    XCTAssertEqual(player.plays, 0)
    player.succeeds = false
    var failures = 0
    tone.onFailure = { _ in failures += 1 }
    tone.update(outgoing: true, phase: "dialing", audioActive: true)
    XCTAssertEqual(failures, 1)
    XCTAssertEqual(player.stops, 1)
  }

  @MainActor func testGeneratedWaveIsPlayableAndContainsSilentInterval() throws {
    let wave = PhoneRingback.waveData()
    let player = try AVAudioPlayer(data: wave)
    XCTAssertEqual(player.duration, 5, accuracy: 0.001)
    XCTAssertEqual(player.numberOfChannels, 1)
    XCTAssertEqual(wave.count, 44 + 48000 * 5 * 2)
    XCTAssertTrue(wave.dropFirst(44 + 48000 * 2).allSatisfy { $0 == 0 })
    XCTAssertTrue(wave[44..<(44 + 48000 * 2)].contains { $0 != 0 })
  }
}
