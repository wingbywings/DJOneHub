/// One connection attempt per foreground visit. Background push calls are independent.
public struct ForegroundConnectionIntent: Sendable {
  public private(set) var active = false
  public private(set) var paused = false
  public private(set) var generation: UInt64 = 0
  private var attempted = false

  public init() {}
  public mutating func activate() {
    if !active {
      active = true
      generation &+= 1
      attempted = false
    }
  }
  public mutating func deactivate() {
    active = false
    generation &+= 1
  }
  public mutating func pause() {
    paused = true
    generation &+= 1
  }
  public mutating func resume() {
    paused = false
    generation &+= 1
    attempted = false
  }
  public func eligible(ready: Bool, microphoneGranted: Bool, busy: Bool) -> Bool {
    active && !paused && !attempted && ready && microphoneGranted && !busy
  }
  public mutating func claim(generation expected: UInt64, ready: Bool, microphoneGranted: Bool, busy: Bool) -> Bool {
    guard generation == expected, eligible(ready: ready, microphoneGranted: microphoneGranted, busy: busy) else {
      return false
    }
    attempted = true
    return true
  }
  public mutating func manualAttempt() { attempted = true }
}
