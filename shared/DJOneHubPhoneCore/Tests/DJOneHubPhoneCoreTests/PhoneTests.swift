import XCTest

@testable import DJOneHubPhoneCore

@MainActor final class MemoryVault: IdentityVault {
  var data: Data?
  var failReplace = false
  func load() throws -> Data? { data }
  func create(_ data: Data) throws {
    guard self.data == nil else { throw PhoneError.alreadyPaired }
    self.data = data
  }
  func replace(_ data: Data) throws {
    if failReplace { throw PhoneError.unavailable }
    self.data = data
  }
}
@MainActor final class MockHTTP: IdentityHTTP {
  var calls: [String] = []
  var accessTokens: [String?] = []
  var response: (String, Data) throws -> Data = { _, _ in throw PhoneError.unavailable }
  func post(origin: URL, path: String, body: Data, accessToken: String?) async throws -> Data {
    calls.append(path)
    accessTokens.append(accessToken)
    return try response(path, body)
  }
}
final class PhoneTests: XCTestCase {
  let now = Date()
  func principal() -> Principal {
    Principal(
      id: "phone", tenant: "tenant", kind: "installation", hub_id: "hub", modules: ["module"],
      version: 1, access_expires_at: Date().addingTimeInterval(900))
  }
  func tokens(_ accessExpiry: Date, refresh: String = String(repeating: "r", count: 43)) -> Tokens {
    let p = Principal(
      id: "phone", tenant: "tenant", kind: "installation", hub_id: "hub", modules: ["module"],
      version: 1, access_expires_at: accessExpiry)
    return Tokens(
      access_token: String(repeating: "a", count: 43), refresh_token: refresh,
      refresh_expires_at: Date().addingTimeInterval(3600), principal: p)
  }
  func invite(_ uuid: String = UUID().uuidString.lowercased()) -> Signal {
    let b = Binding(
      hubEpoch: "epoch", moduleID: "module", callUUID: uuid, installationID: "phone", generation: 1)
    let f = ISO8601DateFormatter()
    return Signal(
      type: "invite", binding: b,
      payload: [
        "deadline": .string(f.string(from: Date().addingTimeInterval(45))),
        "display": .string("DJOneHub"),
      ])
  }
  func testWireRejectsForeignInstallationOversizeAndUnknownFields() throws {
    let message = invite()
    try message.validateIncoming(principal: principal())
    var wrong = message
    wrong.installation_id = "someone_else"
    XCTAssertThrowsError(try wrong.validateIncoming(principal: principal()))
    XCTAssertThrowsError(try Signal.decode(Data(repeating: 32, count: 131073)))
    var object = try JSONSerialization.jsonObject(with: message.encoded()) as! [String: Any]
    object["admin"] = true
    XCTAssertThrowsError(try Signal.decode(JSONSerialization.data(withJSONObject: object)))
    var ice = Signal(
      type: "ice", binding: message.binding,
      payload: ["candidate": .string("c"), "sdpMLineIndex": .uint(65536)])
    XCTAssertThrowsError(try ice.validateIncoming(principal: principal()))
    ice.payload["sdpMLineIndex"] = .uint(0)
    try ice.validateIncoming(principal: principal())
  }
  @MainActor func testFailureDiagnosticsDistinguishCallKitProtocolAndTransportWait() {
    var reason: CallFailure?
    let call = PhoneCall(principal: principal()) {
      if case .disconnect(let failure) = $0 { reason = failure }
    }
    let first = invite()
    call.receive(first)
    call.reported(first.binding!, success: false)
    XCTAssertEqual(reason, .callKitReportFailed)
    XCTAssertNil(call.binding)

    let second = invite()
    call.receive(second)
    call.reported(second.binding!, success: true)
    call.requestAnswer(second.binding!, now: now)
    call.tick(now: now.addingTimeInterval(13))
    XCTAssertEqual(reason, .transportTimeout)  // no transport_ready callback
    XCTAssertNil(call.binding)

    let third = invite()
    call.receive(third)
    var invalid = Signal(
      type: "offer", binding: third.binding,
      payload: ["type": .string("offer"), "sdp": .string("v=0")])
    invalid.payload["unexpected_secret"] = .string("must_not_be_exposed")
    call.receive(invalid)
    XCTAssertEqual(reason, .invalidMessage)
    XCTAssertNil(call.binding)
  }
  @MainActor func testAnswerWaitsForMediaAndHubBeforeCallKitAndAudio() {
    var effects: [CallEffect] = []
    let call = PhoneCall(principal: principal()) { effects.append($0) }
    let m = invite()
    let b = m.binding!
    call.receive(m)
    call.reported(b, success: true)
    call.requestAnswer(b)
    XCTAssertFalse(
      effects.contains {
        if case .send = $0 { return true }
        return false
      })
    call.receive(
      Signal(type: "offer", binding: b, payload: ["type": .string("offer"), "sdp": .string("v=0")]))
    call.mediaReady(b)
    XCTAssertEqual(
      effects.filter {
        if case .send(let m) = $0 { return m.type == "command" }
        return false
      }.count, 1)
    call.activated(b)  // early system callback cannot enable audio
    XCTAssertFalse(
      effects.contains {
        if case .audio(_, true) = $0 { return true }
        return false
      })
    call.receive(
      Signal(
        type: "state", binding: b,
        payload: ["revision": .uint(3), "state": .string("awaiting_audio")]))
    XCTAssertTrue(
      effects.contains {
        if case .answerCompleted = $0 { return true }
        return false
      })
    call.activated(b)
    call.activated(b)
    XCTAssertEqual(
      effects.filter {
        if case .send(let m) = $0 { return m.type == "audio_active" }
        return false
      }.count, 1)
    call.receive(
      Signal(
        type: "state", binding: b, payload: ["revision": .uint(4), "state": .string("connected")]))
    call.tick(now: Date().addingTimeInterval(100))
    XCTAssertEqual(call.binding, b)
    call.deactivated(b)
    XCTAssertNil(call.binding)
  }
  @MainActor func testLateCallbacksAndReplayCannotTouchNextCall() {
    var effects: [CallEffect] = []
    let call = PhoneCall(principal: principal()) { effects.append($0) }
    let old = invite()
    call.receive(old)
    call.finish()
    let next = invite()
    call.receive(next)
    let count = effects.count
    call.reported(old.binding!, success: false)
    call.mediaReady(old.binding!)
    call.activated(old.binding!)
    call.requestEnd(old.binding!)
    XCTAssertEqual(effects.count, count)
    XCTAssertEqual(call.binding, next.binding)
    call.receive(
      Signal(
        type: "state", binding: old.binding,
        payload: ["revision": .uint(100), "state": .string("ended")]))
    XCTAssertEqual(call.binding, next.binding)
    call.finish()
    call.receive(old)
    XCTAssertNil(call.binding)
  }
  @MainActor func testRejectAndAnswerTimeoutFailClosed() {
    var effects: [CallEffect] = []
    let call = PhoneCall(principal: principal()) { effects.append($0) }
    let m = invite()
    call.receive(m)
    call.requestEnd(m.binding!)
    XCTAssertTrue(
      effects.contains {
        if case .send(let m) = $0 { return m.payload["action"]?.string == "reject" }
        return false
      })
    call.tick(now: Date().addingTimeInterval(19))
    XCTAssertNil(call.binding)
    let next = invite()
    call.receive(next)
    call.requestAnswer(next.binding!)
    call.tick(now: Date().addingTimeInterval(13))
    XCTAssertNil(call.binding)
  }
  @MainActor func testICEBudgetAndCallKitReportingFailureCloseConnection() {
    let m = invite()
    var disconnected = 0
    let call = PhoneCall(principal: principal()) { if case .disconnect = $0 { disconnected += 1 } }
    call.receive(m)
    for _ in 0..<257 {
      call.receive(Signal(type: "ice", binding: m.binding, payload: ["candidate": .string("c")]))
    }
    XCTAssertNil(call.binding)
    XCTAssertGreaterThan(disconnected, 0)
    let next = invite()
    call.receive(next)
    call.reported(next.binding!, success: false)
    XCTAssertNil(call.binding)
  }
  @MainActor func testPairingPersistsBeforeNetworkAndCannotOverwrite() async throws {
    let vault = MemoryVault()
    let http = MockHTTP()
    let identity = PhoneIdentity(vault: vault, http: http)
    let grant = Grant(
      id: "grant", secret: String(repeating: "s", count: 43),
      expires_at: Date().addingTimeInterval(100), modules: ["module"])
    let file = PairingFile(version: 1, relay_url: "https://relay.example", grant: grant)
    http.response = { path, body in
      XCTAssertEqual(path, "/v1/pairings/redeem")
      XCTAssertEqual(try identity.status()?.phase, "pairing_pending")
      let json = try JSONSerialization.jsonObject(with: body) as! [String: Any]
      XCTAssertNotNil(json["signature"])
      XCTAssertNil(json["seed"])
      return try Wire.encoder().encode(self.tokens(Date().addingTimeInterval(900)))
    }
    try await identity.pair(file)
    XCTAssertEqual(try identity.status()?.phase, "ready")
    do {
      try await identity.pair(file)
      XCTFail("overwrote identity")
    } catch {}
    XCTAssertEqual(http.calls.count, 1)
  }
  @MainActor func testRefreshLostResponseRemainsBlockedAfterRestart() async throws {
    let vault = MemoryVault()
    let http = MockHTTP()
    let record = StoredIdentity(
      relay: "https://relay.example", seed: Data(repeating: 1, count: 32), phase: "ready",
      tokens: tokens(Date().addingTimeInterval(10)))
    try vault.create(Wire.encoder().encode(record))
    http.response = { path, _ in
      if path == "/v1/auth/challenge" {
        return try Wire.encoder().encode([
          "id": "challenge", "value": String(repeating: "c", count: 43),
          "expires_at": ISO8601DateFormatter().string(from: Date().addingTimeInterval(60)),
        ])
      }
      XCTAssertEqual(try PhoneIdentity(vault: vault, http: http).status()?.phase, "refresh_pending")
      throw PhoneError.unavailable
    }
    do {
      _ = try await PhoneIdentity(vault: vault, http: http).credentials()
      XCTFail()
    } catch {}
    XCTAssertEqual(http.calls.count, 2)
    do {
      _ = try await PhoneIdentity(vault: vault, http: http).credentials()
      XCTFail()
    } catch {}
    XCTAssertEqual(http.calls.count, 2)
  }
  @MainActor func testRefreshMarkerFailureSendsNoConsumingRequest() async throws {
    let vault = MemoryVault()
    let http = MockHTTP()
    try vault.create(
      Wire.encoder().encode(
        StoredIdentity(
          relay: "https://relay.example", seed: Data(repeating: 2, count: 32), phase: "ready",
          tokens: tokens(Date().addingTimeInterval(10)))))
    vault.failReplace = true
    http.response = { _, _ in
      try Wire.encoder().encode([
        "id": "challenge", "value": String(repeating: "c", count: 43),
        "expires_at": ISO8601DateFormatter().string(from: Date().addingTimeInterval(60)),
      ])
    }
    do {
      _ = try await PhoneIdentity(vault: vault, http: http).credentials()
      XCTFail()
    } catch {}
    XCTAssertEqual(http.calls, ["/v1/auth/challenge"])
    XCTAssertEqual(try PhoneIdentity(vault: vault, http: http).status()?.phase, "ready")
  }
  func testPairingRejectsInsecureOriginsAndExpiredFiles() throws {
    for url in [
      "http://relay.example", "https://user:secret@relay.example", "https://relay.example/path",
      "https://relay.example?token=secret",
    ] { XCTAssertThrowsError(try Wire.origin(url)) }
    let file = PairingFile(
      version: 1, relay_url: "https://relay.example",
      grant: Grant(
        id: "g", secret: String(repeating: "s", count: 43),
        expires_at: Date().addingTimeInterval(-1), modules: ["module"]))
    XCTAssertThrowsError(try PairingFile.parse(Wire.encoder().encode(file)))
  }
  @MainActor func testPeerClosureEndsOnlyCallAndKeepsPhoneOnlineForNextInvite() {
    var disconnects = 0
    let call = PhoneCall(principal: principal()) { if case .disconnect = $0 { disconnects += 1 } }
    let first = invite()
    call.receive(first)
    call.receive(
      Signal(
        type: "peer_closed", binding: first.binding, payload: ["code": .string("hub_disconnected")])
    )
    XCTAssertNil(call.binding)
    XCTAssertEqual(disconnects, 0)
    let next = invite()
    call.receive(next)
    XCTAssertEqual(call.binding, next.binding)
  }
  func testWelcomeAndRenewalRejectPartialCallBindings() throws {
    let expiry = ISO8601DateFormatter().string(from: Date().addingTimeInterval(900))
    var welcome = Signal(
      type: "welcome",
      payload: [
        "id": .string("phone"), "kind": .string("installation"), "hub_id": .string("hub"),
        "access_expires_at": .string(expiry),
      ])
    welcome.request_id = nil
    welcome.hub_epoch = "epoch"
    _ = try welcome.welcomeExpiry(principal: principal())
    welcome.module_id = "module"
    XCTAssertThrowsError(try welcome.welcomeExpiry(principal: principal()))
    var ack = Signal(
      type: "reauthenticated", requestID: "request", payload: ["access_expires_at": .string(expiry)]
    )
    _ = try ack.renewalExpiry(requestID: "request")
    XCTAssertThrowsError(try ack.renewalExpiry(requestID: "different"))
    ack.generation = 1
    XCTAssertThrowsError(try ack.renewalExpiry(requestID: "request"))
  }
  @MainActor func testSuccessfulRefreshSavesRotationBeforeReturning() async throws {
    let vault = MemoryVault()
    let http = MockHTTP()
    try vault.create(
      Wire.encoder().encode(
        StoredIdentity(
          relay: "https://relay.example", seed: Data(repeating: 3, count: 32), phase: "ready",
          tokens: tokens(Date().addingTimeInterval(10)))))
    let next = tokens(Date().addingTimeInterval(900), refresh: String(repeating: "n", count: 43))
    http.response = { path, _ in
      if path == "/v1/auth/challenge" {
        return try Wire.encoder().encode([
          "id": "challenge", "value": String(repeating: "c", count: 43),
          "expires_at": ISO8601DateFormatter().string(from: Date().addingTimeInterval(60)),
        ])
      }
      return try Wire.encoder().encode(next)
    }
    let identity = PhoneIdentity(vault: vault, http: http)
    let result = try await identity.credentials()
    XCTAssertEqual(result.tokens?.refresh_token, next.refresh_token)
    XCTAssertEqual(try identity.status()?.phase, "ready")
    let restarted = try await PhoneIdentity(vault: vault, http: http).credentials()
    XCTAssertEqual(restarted.tokens?.refresh_token, next.refresh_token)
    XCTAssertEqual(http.calls.count, 2)
  }

  func testTURNConfigurationBoundsAndURLSchemes() throws {
    try ICECredentials.direct.validate()
    for value in [
      "turn:relay.example:3478?transport=udp", "turns:relay.example:5349?transport=tcp",
      "turn:[::1]:3478?transport=udp",
    ] {
      XCTAssertTrue(ICECredentials.validURL(value), value)
    }
    for value in [
      "stun:relay.example:3478", "turn:relay.example:3478",
      "turns:relay.example:5349?transport=udp", "turn://user:secret@host:3478?transport=udp",
      "turn:[gg::]:3478?transport=udp",
    ] {
      XCTAssertFalse(ICECredentials.validURL(value), value)
    }
    let expiry = Date(
      timeIntervalSince1970: floor(Date().addingTimeInterval(4 * 3600).timeIntervalSince1970))
    let server = ICEServer(
      urls: ["turn:relay.example:3478?transport=udp"],
      username: "\(Int64(expiry.timeIntervalSince1970)):" + String(repeating: "a", count: 32),
      credential: Data(repeating: 1, count: 20).base64EncodedString())
    let config = ICECredentials(
      enabled: true, servers: [server], relay_only: true, expires_at: expiry)
    try config.validate()
    XCTAssertThrowsError(try config.validate(now: expiry))
    XCTAssertThrowsError(
      try ICECredentials(enabled: false, servers: [server], relay_only: true, expires_at: expiry)
        .validate())
  }
  @MainActor func testICERequestUsesAccessTokenAndChecksModuleScope() async throws {
    let vault = MemoryVault()
    let http = MockHTTP()
    try vault.create(
      Wire.encoder().encode(
        StoredIdentity(
          relay: "https://relay.example", seed: Data(repeating: 4, count: 32), phase: "ready",
          tokens: tokens(Date().addingTimeInterval(900)))))
    http.response = { path, body in
      XCTAssertEqual(path, "/v1/ice")
      XCTAssertEqual(
        try JSONDecoder().decode([String: String].self, from: body), ["module_id": "module"])
      return try Wire.encoder().encode(ICECredentials.direct)
    }
    let identity = PhoneIdentity(vault: vault, http: http)
    let config = try await identity.ice(module: "module")
    XCTAssertFalse(config.enabled)
    XCTAssertEqual(http.accessTokens.first!, String(repeating: "a", count: 43))
    do {
      _ = try await identity.ice(module: "other")
      XCTFail("exceeded module scope")
    } catch {}
    XCTAssertEqual(http.calls.count, 1)
  }

  @MainActor func testOutgoingAdoptsHubEpochWaitsForTransportAndNeverReportsIncoming() throws {
    var effects: [CallEffect] = []
    let call = PhoneCall(principal: principal()) { effects.append($0) }
    let uuid = UUID()
    try call.beginOutgoing(
      module: "module", uuid: uuid, epoch: "phone_epoch", number: "+8613800000000")
    let original = call.binding!
    var ack = Signal(type: "dial_accepted", binding: original, payload: ["accepted": .bool(true)])
    ack.hub_epoch = "hub_epoch"
    let b = ack.binding!
    call.receive(ack)
    XCTAssertEqual(call.binding, b)
    var m = invite(uuid.uuidString.lowercased())
    m.hub_epoch = "hub_epoch"
    m.payload["direction"] = .string("outgoing")
    call.receive(m)
    call.receive(
      Signal(type: "offer", binding: b, payload: ["type": .string("offer"), "sdp": .string("v=0")]))
    XCTAssertFalse(
      effects.contains {
        if case .reportIncoming = $0 { return true }
        return false
      })
    XCTAssertFalse(
      effects.contains {
        if case .send(let m) = $0 { return m.type == "command" }
        return false
      })
    call.mediaReady(b)
    call.activated(b)  // system audio can activate while preparing, still gated
    XCTAssertFalse(
      effects.contains {
        if case .audio(_, true) = $0 { return true }
        return false
      })
    call.receive(
      Signal(
        type: "state", binding: b, payload: ["revision": .uint(2), "state": .string("answering")]))
    call.tick(now: Date().addingTimeInterval(20))
    XCTAssertEqual(call.binding, b)  // incoming 12-second budget does not end outbound ringing
    call.receive(
      Signal(
        type: "state", binding: b,
        payload: ["revision": .uint(3), "state": .string("awaiting_audio")]))
    call.activated(b)
    call.receive(
      Signal(
        type: "state", binding: b, payload: ["revision": .uint(4), "state": .string("connected")]))
    call.requestEnd(b)
    call.deactivated(b)  // local EndCall audio deactivation is expected
    XCTAssertEqual(call.binding, b)
    call.receive(
      Signal(
        type: "state", binding: b,
        payload: [
          "revision": .uint(5), "state": .string("ended"), "reason": .string("remote_hangup"),
        ]))
    XCTAssertNil(call.binding)
    XCTAssertEqual(call.endReason, "remote_hangup")
  }
  @MainActor func testOutgoingCancelBeforeAckAndInvalidNumbersNeverDial() throws {
    var sent: [Signal] = []
    let call = PhoneCall(principal: principal()) { if case .send(let m) = $0 { sent.append(m) } }
    for invalid in ["+", "123;ATH", "*123#", "１２３", String(repeating: "1", count: 33)] {
      XCTAssertNil(PhoneCall.dialNumber(invalid))
    }
    XCTAssertEqual(PhoneCall.dialNumber("+86 (138) 0000-0000"), "+8613800000000")
    XCTAssertThrowsError(
      try call.beginOutgoing(module: "foreign", uuid: UUID(), epoch: "epoch", number: "12345"))
    XCTAssertTrue(sent.isEmpty)
    try call.beginOutgoing(module: "module", uuid: UUID(), epoch: "epoch", number: "12345")
    call.requestEnd(call.binding!)
    XCTAssertEqual(sent.map(\.type), ["dial", "cancel_dial"])
  }
  @MainActor func testHangupRetriesStaleRevisionOnceAndIgnoresAudioReadyWhileEnding() {
    var sent: [Signal] = []
    var disconnects = 0
    let call = PhoneCall(principal: principal()) {
      if case .send(let m) = $0 { sent.append(m) }
      if case .disconnect = $0 { disconnects += 1 }
    }
    let m = invite()
    let b = m.binding!
    call.receive(m)
    call.reported(b, success: true)
    call.receive(
      Signal(type: "offer", binding: b, payload: ["type": .string("offer"), "sdp": .string("v=0")]))
    call.mediaReady(b)
    call.requestAnswer(b)
    call.requestEnd(b)
    let end = sent.last!
    call.receive(
      Signal(
        type: "result", binding: b, requestID: end.request_id!,
        payload: ["accepted": .bool(false), "code": .string("stale_revision")]))
    call.receive(
      Signal(
        type: "state", binding: b,
        payload: ["revision": .uint(3), "state": .string("awaiting_audio")]))
    XCTAssertEqual(disconnects, 0)
    XCTAssertFalse(call.answered)
    XCTAssertEqual(sent.last?.payload["expected_revision"], .uint(3))
    XCTAssertEqual(sent.filter { $0.payload["action"]?.string == "hangup" }.count, 2)
    call.receive(
      Signal(
        type: "state", binding: b, payload: ["revision": .uint(4), "state": .string("connected")]))
    XCTAssertEqual(disconnects, 0)  // connected arrived after local EndCall
    call.deactivated(b)
    XCTAssertEqual(disconnects, 0)
    call.receive(
      Signal(type: "state", binding: b, payload: ["revision": .uint(5), "state": .string("ended")]))
    XCTAssertNil(call.binding)
  }

}
