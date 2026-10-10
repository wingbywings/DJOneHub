import Foundation

// Only a hint to report CallKit and establish authenticated signaling. It never
// authorizes an answer, media, a Relay URL, or a change of installation identity.
public struct PushInvite: Sendable {
  public let signal: Signal
  public let binding: Binding
  public let deadline: Date
  public static func decode(_ data: Data) throws -> Self {
    guard data.count <= 5120,
      let root = try JSONSerialization.jsonObject(with: data) as? [String: Any],
      Set(root.keys) == ["aps", "invite"], root["aps"] is [String: Any],
      let object = root["invite"] as? [String: Any]
    else { throw PhoneError.invalidData }
    let signal = try Signal.decode(JSONSerialization.data(withJSONObject: object))
    guard signal.type == "invite", let binding = signal.binding,
      signal.payload["direction"]?.string != "outgoing",
      let deadline = signal.payload["deadline"]?.string.flatMap(Wire.date)
    else { throw PhoneError.invalidData }
    return Self(signal: signal, binding: binding, deadline: deadline)
  }
  public func validate(principal: Principal, now: Date = Date()) throws {
    try signal.validateIncoming(principal: principal, now: now)
  }
  public func matches(_ other: Signal) -> Bool {
    other.type == "invite" && other.binding == binding
      && other.payload == signal.payload
  }
}
