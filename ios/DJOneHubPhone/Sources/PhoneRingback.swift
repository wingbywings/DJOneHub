import AVFAudio
import Foundation

@MainActor protocol RingbackPlaying: AnyObject {
  @discardableResult func play() -> Bool
  func stop()
}

extension AVAudioPlayer: RingbackPlaying {}

// Local feedback for outgoing calls while the Hub's media gate is closed.
// This player never activates the audio session or enables the microphone.
@MainActor final class PhoneRingback {
  private let makePlayer: () throws -> any RingbackPlaying
  private var player: (any RingbackPlaying)?
  var onFailure: ((Error) -> Void)?

  init(makePlayer: @escaping () throws -> any RingbackPlaying = {
    let player = try AVAudioPlayer(data: PhoneRingback.waveData())
    player.numberOfLoops = -1
    player.volume = 1
    return player
  }) {
    self.makePlayer = makePlayer
  }

  func update(outgoing: Bool, phase: String, audioActive: Bool) {
    guard outgoing, audioActive, ["dialing", "offered", "answering"].contains(phase) else {
      stop()
      return
    }
    guard player == nil else { return }
    do {
      let next = try makePlayer()
      guard next.play() else {
        next.stop()
        throw NSError(domain: "PhoneRingback", code: 1)
      }
      player = next
    } catch { onFailure?(error) }
  }

  func stop() {
    player?.stop()
    player = nil
  }

  // A gentle 450 Hz tone: one second on, four seconds off. Short fades avoid
  // clicks; the entire cadence loops without timers, including when locked.
  nonisolated static func waveData() -> Data {
    let rate = 48000, toneFrames = rate, frames = rate * 5, fadeFrames = rate / 100
    var data = Data()
    data.reserveCapacity(44 + frames * 2)
    func append<T: FixedWidthInteger>(_ value: T) {
      var little = value.littleEndian
      withUnsafeBytes(of: &little) { data.append(contentsOf: $0) }
    }
    data.append(contentsOf: "RIFF".utf8)
    append(UInt32(36 + frames * 2))
    data.append(contentsOf: "WAVEfmt ".utf8)
    append(UInt32(16))
    append(UInt16(1)) // linear PCM
    append(UInt16(1)) // mono
    append(UInt32(rate))
    append(UInt32(rate * 2))
    append(UInt16(2))
    append(UInt16(16))
    data.append(contentsOf: "data".utf8)
    append(UInt32(frames * 2))
    for frame in 0..<frames {
      var sample: Int16 = 0
      if frame < toneFrames {
        let fade = min(1, Double(min(frame, toneFrames - 1 - frame)) / Double(fadeFrames))
        sample = Int16((8191 * fade * sin(2 * .pi * 450 * Double(frame) / Double(rate))).rounded())
      }
      append(sample)
    }
    return data
  }
}
