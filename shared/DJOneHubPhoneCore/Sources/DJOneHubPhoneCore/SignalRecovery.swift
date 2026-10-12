import Foundation

// The same deadline covers socket retries, peer recovery and state reconciliation.
// An authoritative state reply, not merely a welcome/pong, completes recovery.
public struct SignalRecoveryWindow {
  public private(set) var binding: Binding?
  public private(set) var deadline: Date?
  public private(set) var requestID: String?
  public init() {}
  public mutating func begin(_ binding: Binding, now: Date = Date()) throws {
    if let current = self.binding {
      guard current == binding, let deadline, now < deadline else { throw PhoneError.invalidData }
      return
    }
    self.binding = binding
    deadline = now.addingTimeInterval(10)
  }
  public mutating func synchronize(now: Date = Date()) throws -> Signal {
    guard let binding, let deadline, now < deadline else { throw PhoneError.unavailable }
    let message = Signal(type: "sync", binding: binding, payload: [:])
    requestID = message.request_id
    return message
  }
  public mutating func accept(_ message: Signal, revision: UInt64, now: Date = Date()) throws -> Bool {
    guard message.type == "state", message.request_id == requestID, requestID != nil else { return false }
    guard let deadline, now < deadline, message.binding == binding,
      let received = message.payload["revision"]?.uint, received >= revision,
      let state = message.payload["state"]?.string, ["connected", "ended"].contains(state)
    else { throw PhoneError.invalidData }
    self = SignalRecoveryWindow()
    return true
  }
}
