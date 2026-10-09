import Foundation

public enum Scalar: Codable, Equatable, Sendable {
  case string(String)
  case uint(UInt64)
  case bool(Bool)
  case null
  public init(from decoder: Decoder) throws {
    let c = try decoder.singleValueContainer()
    if c.decodeNil() {
      self = .null
    } else if let v = try? c.decode(Bool.self) {
      self = .bool(v)
    } else if let v = try? c.decode(UInt64.self) {
      self = .uint(v)
    } else {
      self = .string(try c.decode(String.self))
    }
  }
  public func encode(to encoder: Encoder) throws {
    var c = encoder.singleValueContainer()
    switch self {
    case .string(let v): try c.encode(v)
    case .uint(let v): try c.encode(v)
    case .bool(let v): try c.encode(v)
    case .null: try c.encodeNil()
    }
  }
  public var string: String? {
    if case .string(let v) = self { return v }
    return nil
  }
  public var uint: UInt64? {
    if case .uint(let v) = self { return v }
    return nil
  }
  public var bool: Bool? {
    if case .bool(let v) = self { return v }
    return nil
  }
}
public struct Binding: Equatable, Hashable, Sendable {
  public let hubEpoch, moduleID, callUUID, installationID: String
  public let generation: UInt64
}
public struct Signal: Codable, Sendable {
  public var version = 1
  public var type: String
  public var hub_epoch: String?
  public var module_id: String?
  public var call_uuid: String?
  public var generation: UInt64?
  public var installation_id: String?
  public var request_id: String?
  public var payload: [String: Scalar]
  public init(
    type: String, binding: Binding? = nil, requestID: String = UUID().uuidString.lowercased(),
    payload: [String: Scalar]
  ) {
    self.type = type
    self.request_id = requestID
    self.payload = payload
    if let b = binding {
      hub_epoch = b.hubEpoch
      module_id = b.moduleID
      call_uuid = b.callUUID
      generation = b.generation
      installation_id = b.installationID
    }
  }
  public var binding: Binding? {
    guard let h = hub_epoch, let m = module_id, let u = call_uuid, let g = generation,
      let i = installation_id,
      Wire.id(h), Wire.id(m), Wire.id(i), g > 0, UUID(uuidString: u)?.uuidString.lowercased() == u,
      let r = request_id, Wire.id(r)
    else { return nil }
    return Binding(hubEpoch: h, moduleID: m, callUUID: u, installationID: i, generation: g)
  }
  public static func decode(_ data: Data) throws -> Self {
    guard data.count <= 131072, String(data: data, encoding: .utf8) != nil,
      let root = try JSONSerialization.jsonObject(with: data) as? [String: Any],
      Set(root.keys).isSubset(of: [
        "version", "type", "hub_epoch", "module_id", "call_uuid", "generation", "installation_id",
        "request_id", "payload",
      ])
    else { throw PhoneError.invalidData }
    let m = try Wire.decoder().decode(Self.self, from: data)
    guard m.version == 1 else { throw PhoneError.invalidData }
    return m
  }
  public func encoded() throws -> Data { try Wire.encoder().encode(self) }
  public func welcomeExpiry(principal: Principal, now: Date = Date()) throws -> Date {
    guard version == 1, type == "welcome", hub_epoch.map(Wire.id) == true,
      module_id == nil, call_uuid == nil, generation == nil, installation_id == nil,
      request_id == nil,
      Set(payload.keys) == ["id", "kind", "hub_id", "access_expires_at"],
      payload["id"]?.string == principal.id, payload["kind"]?.string == "installation",
      payload["hub_id"]?.string == principal.hub_id,
      let expiry = payload["access_expires_at"]?.string.flatMap(Wire.date), expiry > now
    else { throw PhoneError.invalidData }
    return expiry
  }
  public func renewalExpiry(requestID: String, now: Date = Date()) throws -> Date {
    guard version == 1, type == "reauthenticated", hub_epoch == nil, module_id == nil,
      call_uuid == nil,
      generation == nil, installation_id == nil, request_id == requestID, Wire.id(requestID),
      Set(payload.keys) == ["access_expires_at"],
      let expiry = payload["access_expires_at"]?.string.flatMap(Wire.date),
      expiry > now.addingTimeInterval(60)
    else { throw PhoneError.invalidData }
    return expiry
  }
  public func validateIncoming(principal: Principal, now: Date = Date()) throws {
    guard let b = binding, b.installationID == principal.id, principal.modules.contains(b.moduleID)
    else { throw PhoneError.invalidData }
    var keys: Set<String> = []
    var valid = false
    switch type {
    case "invite":
      keys = ["deadline", "display", "direction"]
      if let d = payload["deadline"]?.string.flatMap(Wire.date),
        let display = payload["display"]?.string
      {
        valid =
          d > now && d <= now.addingTimeInterval(95) && display.utf8.count <= 128
          && (payload["direction"] == nil
            || ["incoming", "outgoing"].contains(payload["direction"]?.string ?? ""))
      }
    case "offer":
      keys = ["type", "sdp"]
      valid =
        payload["type"]?.string == "offer"
        && (payload["sdp"]?.string.map { !$0.isEmpty && $0.utf8.count <= 65536 } ?? false)
    case "ice":
      keys = ["candidate", "sdpMid", "sdpMLineIndex", "usernameFragment"]
      valid = (payload["candidate"]?.string?.utf8.count ?? 4097) <= 4096
      for (key, limit) in [("sdpMid", 128), ("usernameFragment", 256)] {
        if let v = payload[key], v != .null {
          valid = valid && (v.string?.utf8.count ?? (limit + 1)) <= limit
        }
      }
      if let v = payload["sdpMLineIndex"], v != .null {
        valid = valid && (v.uint ?? 65536) <= 65535
      }
    case "state":
      keys = ["revision", "state", "reason"]
      valid =
        (payload["revision"]?.uint ?? 0) > 0
        && ["offered", "answering", "rejecting", "awaiting_audio", "connected", "ended"].contains(
          payload["state"]?.string ?? "")
        && (payload["reason"] == nil || (payload["reason"]?.string?.utf8.count ?? 129) <= 128)
    case "dial_accepted":
      keys = ["accepted", "code"]
      valid = payload["accepted"]?.bool == true && payload["code"] == nil
    case "result":
      keys = ["accepted", "code"]
      valid =
        payload["accepted"]?.bool != nil
        && (payload["code"] == nil || (payload["code"]?.string?.utf8.count ?? 129) <= 128)
    case "peer_closed", "error":
      keys = ["code"]
      valid = payload["code"]?.string.map(Wire.id) ?? false
    default: break
    }
    guard valid, Set(payload.keys).isSubset(of: keys) else { throw PhoneError.invalidData }
  }
}

public enum CallFailure: String, Sendable {
  case invalidMessage = "invalid_message"
  case invalidBinding = "invalid_binding"
  case duplicateInvite = "duplicate_invite"
  case duplicateOffer = "duplicate_offer"
  case candidateLimit = "candidate_limit"
  case unexpectedAudioReady = "unexpected_audio_ready"
  case unexpectedConnected = "unexpected_connected"
  case commandRejected = "command_rejected"
  case relayRejected = "relay_rejected"
  case unexpectedMessage = "unexpected_message"
  case callKitReportFailed = "callkit_report_failed"
  case audioDeactivated = "audio_deactivated"
  case ringTimeout = "ring_timeout"
  case transportTimeout = "transport_timeout"
  case hubAnswerTimeout = "hub_answer_timeout"
  case audioActivationTimeout = "audio_activation_timeout"
  case endTimeout = "end_timeout"
  case actionTimeout = "action_timeout"
  case unavailable
}
public enum CallEffect {
  case reportIncoming(Binding, String)
  case acceptOffer(Binding, String)
  case candidate(Binding, [String: Scalar])
  case send(Signal)
  case answerCompleted(Binding)
  case audio(Binding, Bool)
  case phase(Binding, String)
  case ended(Binding)
  case disconnect(CallFailure)
}

// All effects and native callbacks are serialized on the main actor. Binding
// checks make delayed CallKit, SDP and ICE callbacks harmless after replacement.
@MainActor public final class PhoneCall {
  public private(set) var binding: Binding?
  public private(set) var revision: UInt64 = 1
  public private(set) var answered = false
  public private(set) var ending = false
  public private(set) var outgoing = false
  public private(set) var endReason = ""
  private var dialAcknowledged = false, inviteReceived = false
  private var endRetry = false, endRetried = false
  private var commandRevision: UInt64 = 0
  private let principal: Principal
  private let effect: (CallEffect) -> Void
  private var seen = Set<String>()
  private var pendingCommand: String?
  private var reported = false, offerReceived = false, transportReady = false, wantsAnswer = false
  private var answerSent = false, audioActive = false
  private var sequence: UInt64 = 0, iceCount = 0
  private var deadline = Date.distantPast
  private var actionDeadline: Date?
  public init(principal: Principal, effect: @escaping (CallEffect) -> Void) {
    self.principal = principal
    self.effect = effect
  }
  public static func dialNumber(_ raw: String) -> String? {
    let number = raw.filter { !" -()".contains($0) }
    let digits = number.hasPrefix("+") ? String(number.dropFirst()) : number
    guard !digits.isEmpty, number.utf8.count <= 32, digits.allSatisfy({ $0 >= "0" && $0 <= "9" })
    else { return nil }
    return number
  }
  public func beginOutgoing(
    module: String, uuid: UUID, epoch: String, number: String, now: Date = Date()
  ) throws {
    guard binding == nil, principal.modules.contains(module), Wire.id(epoch),
      Self.dialNumber(number) == number, seen.count < 1024
    else { throw PhoneError.invalidData }
    let b = Binding(
      hubEpoch: epoch, moduleID: module, callUUID: uuid.uuidString.lowercased(),
      installationID: principal.id, generation: 1)
    guard !seen.contains(b.callUUID) else { throw PhoneError.invalidData }
    reset(b)
    outgoing = true
    reported = true
    wantsAnswer = true
    deadline = now.addingTimeInterval(90)
    actionDeadline = now.addingTimeInterval(12)
    effect(.phase(b, "dialing"))
    effect(.send(Signal(type: "dial", binding: b, payload: ["number": .string(number)])))
  }
  private func reset(_ b: Binding) {
    seen.insert(b.callUUID)
    binding = b
    revision = 1
    answered = false
    ending = false
    outgoing = false
    reported = false
    offerReceived = false
    transportReady = false
    wantsAnswer = false
    answerSent = false
    sequence = 0
    iceCount = 0
    pendingCommand = nil
    actionDeadline = nil
    audioActive = false
    endReason = ""
    dialAcknowledged = false
    inviteReceived = false
    endRetry = false
    endRetried = false
  }
  public func receive(_ m: Signal, now: Date = Date()) {
    do { try m.validateIncoming(principal: principal, now: now) } catch {
      fail(.invalidMessage)
      return
    }
    guard let b = m.binding else {
      fail(.invalidBinding)
      return
    }
    if m.type == "dial_accepted" {
      guard let current = binding, outgoing, current.callUUID == b.callUUID,
        current.moduleID == b.moduleID, current.installationID == b.installationID,
        b.generation == 1, !dialAcknowledged
      else { return }
      binding = b
      dialAcknowledged = true
      return
    }
    if m.type == "invite", m.payload["direction"]?.string == "outgoing" {
      guard outgoing, dialAcknowledged, binding == b, !inviteReceived else { return }
      inviteReceived = true
      deadline = Wire.date(m.payload["deadline"]!.string!)!
      tryAnswer()
      return
    }
    if m.type == "invite" {
      if binding == b { return }
      guard binding == nil, !seen.contains(b.callUUID), seen.count < 1024 else {
        fail(.duplicateInvite)
        return
      }
      reset(b)
      inviteReceived = true
      deadline = Wire.date(m.payload["deadline"]!.string!)!
      effect(.phase(b, "incoming"))
      effect(.reportIncoming(b, m.payload["display"]!.string!))
      return
    }
    guard binding == b else { return }  // late events can never affect the next UUID
    switch m.type {
    case "offer":
      guard !offerReceived else {
        fail(.duplicateOffer)
        return
      }
      offerReceived = true
      effect(.acceptOffer(b, m.payload["sdp"]!.string!))
    case "ice":
      iceCount += 1
      guard iceCount <= 256 else {
        fail(.candidateLimit)
        return
      }
      effect(.candidate(b, m.payload))
    case "state":
      let r = m.payload["revision"]!.uint!
      guard r > revision else { return }
      revision = r
      switch m.payload["state"]!.string! {
      case "ended": finish(reason: m.payload["reason"]?.string ?? "remote_ended")
      case "awaiting_audio":
        if ending { break }
        guard answerSent else {
          fail(.unexpectedAudioReady)
          return
        }
        if !answered {
          answered = true
          actionDeadline = now.addingTimeInterval(3)
          effect(.answerCompleted(b))
        }
      case "connected":
        if ending { break }
        guard answered, audioActive else {
          fail(.unexpectedConnected)
          return
        }
        actionDeadline = nil
      default: break
      }
      if binding == b, !ending { effect(.phase(b, m.payload["state"]!.string!)) }
      if binding == b, ending, endRetry, !endRetried, revision > commandRevision {
        endRetried = true
        endRetry = false
        command(answerSent ? "hangup" : "reject", b)
      }
    case "result":
      guard m.request_id == pendingCommand else { return }
      if m.payload["accepted"]?.bool != true {
        if ending, m.payload["code"]?.string == "stale_revision", !endRetried {
          endRetry = true
          if revision > commandRevision {
            endRetry = false
            endRetried = true
            command(answerSent ? "hangup" : "reject", b)
          }
        } else {
          fail(.commandRejected)
        }
      }
    case "peer_closed": finish(reason: "peer_closed")
    case "error": fail(.relayRejected)
    default: fail(.unexpectedMessage)
    }
  }
  public func reported(_ b: Binding, success: Bool) {
    guard binding == b else { return }
    guard success else {
      fail(.callKitReportFailed)
      return
    }
    reported = true
    tryAnswer()
  }
  public func mediaReady(_ b: Binding) {
    guard binding == b else { return }
    transportReady = true
    tryAnswer()
  }
  public func requestAnswer(_ b: Binding, now: Date = Date()) {
    guard binding == b, !ending, !wantsAnswer else { return }
    wantsAnswer = true
    effect(.phase(b, "answering"))
    actionDeadline = now.addingTimeInterval(12)
    tryAnswer()
  }
  private func tryAnswer() {
    guard let b = binding, inviteReceived, reported, offerReceived, transportReady, wantsAnswer,
      !answerSent,
      !ending
    else { return }
    answerSent = true
    if outgoing { actionDeadline = Date().addingTimeInterval(65) }
    command("answer", b)
  }
  private func command(_ action: String, _ b: Binding) {
    let m = Signal(
      type: "command", binding: b,
      payload: ["action": .string(action), "expected_revision": .uint(revision)])
    commandRevision = revision
    pendingCommand = m.request_id
    effect(.send(m))
  }
  public func requestEnd(_ b: Binding, now: Date = Date()) {
    guard binding == b, !ending else { return }
    ending = true
    effect(.phase(b, "ending"))
    effect(.audio(b, false))
    if outgoing && !dialAcknowledged {
      effect(.send(Signal(type: "cancel_dial", binding: b, payload: [:])))
    } else {
      command(answerSent ? "hangup" : "reject", b)
    }
    actionDeadline = now.addingTimeInterval(18)
  }
  public func activated(_ b: Binding) {
    guard binding == b, answered, !ending, !audioActive else { return }
    audioActive = true
    sequence += 1
    effect(.audio(b, true))
    effect(
      .send(
        Signal(
          type: "audio_active", binding: b,
          payload: ["sequence": .uint(sequence), "active": .bool(true)])))
  }
  public func deactivated(_ b: Binding) {
    guard binding == b, !ending else { return }
    fail(.audioDeactivated)
  }
  public func tick(now: Date = Date()) {
    guard binding != nil else { return }
    if !answerSent && now >= deadline {
      fail(.ringTimeout)
    } else if actionDeadline.map({ now >= $0 }) ?? false {
      if ending {
        fail(.endTimeout)
      } else if wantsAnswer && !transportReady {
        fail(.transportTimeout)
      } else if answerSent && !answered {
        fail(.hubAnswerTimeout)
      } else if answered && !audioActive {
        fail(.audioActivationTimeout)
      } else {
        fail(.actionTimeout)
      }
    }
  }
  public func fail(_ reason: CallFailure = .unavailable) {
    finish(reason: reason.rawValue)
    effect(.disconnect(reason))
  }
  public func finish(reason: String = "ended") {
    guard let b = binding else { return }
    endReason = reason
    binding = nil
    effect(.audio(b, false))
    effect(.ended(b))
  }
}
