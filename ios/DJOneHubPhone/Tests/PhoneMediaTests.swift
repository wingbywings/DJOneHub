import CallKit
import DJOneHubPhoneCore
import WebRTC
import XCTest

@testable import DJOneHubPhone

@MainActor final class PeerObserver: NSObject, RTCPeerConnectionDelegate {
  var ice: ((RTCIceCandidate) -> Void)?
  nonisolated func peerConnection(
    _ peerConnection: RTCPeerConnection, didChange stateChanged: RTCSignalingState
  ) {}
  nonisolated func peerConnection(
    _ peerConnection: RTCPeerConnection, didAdd stream: RTCMediaStream
  ) {}
  nonisolated func peerConnection(
    _ peerConnection: RTCPeerConnection, didRemove stream: RTCMediaStream
  ) {}
  nonisolated func peerConnectionShouldNegotiate(_ peerConnection: RTCPeerConnection) {}
  nonisolated func peerConnection(
    _ peerConnection: RTCPeerConnection, didChange newState: RTCIceConnectionState
  ) {}
  nonisolated func peerConnection(
    _ peerConnection: RTCPeerConnection, didChange newState: RTCIceGatheringState
  ) {}
  nonisolated func peerConnection(
    _ peerConnection: RTCPeerConnection, didRemove candidates: [RTCIceCandidate]
  ) {}
  nonisolated func peerConnection(
    _ peerConnection: RTCPeerConnection, didOpen dataChannel: RTCDataChannel
  ) { dataChannel.close() }
  nonisolated func peerConnection(
    _ peerConnection: RTCPeerConnection, didGenerate candidate: RTCIceCandidate
  ) {
    Task { @MainActor [weak self] in self?.ice?(candidate) }
  }
}
final class PhoneMediaTests: XCTestCase {
  func testPingCompletionIgnoresErrorAfterSuccessfulPong() async throws {
    try await withCheckedThrowingContinuation { (c: CheckedContinuation<Void, Error>) in
      let completion = PingCompletion(c)
      completion.finish(nil)
      completion.finish(URLError(.cancelled))
      completion.finish(nil)
    }
  }
  func testPingCompletionPreservesFirstErrorDuringConcurrentCallbacks() async {
    do {
      try await withCheckedThrowingContinuation { (c: CheckedContinuation<Void, Error>) in
        let completion = PingCompletion(c)
        completion.finish(URLError(.networkConnectionLost))
        DispatchQueue.concurrentPerform(iterations: 32) { _ in completion.finish(nil) }
      }
      XCTFail("First ping failure was lost")
    } catch {
      XCTAssertEqual((error as? URLError)?.code, .networkConnectionLost)
    }
  }
  @MainActor func testNativeOpusNegotiatesWithPionFixture() async throws {
    let base = URL(string: "http://127.0.0.1:39096")!
    let config = URLSessionConfiguration.ephemeral
    config.connectionProxyDictionary = [:]
    config.timeoutIntervalForRequest = 7
    let session = URLSession(configuration: config)
    defer { session.invalidateAndCancel() }
    let offerData: Data
    do { (offerData, _) = try await session.data(from: base.appendingPathComponent("offer")) } catch
    { throw XCTSkip("Start DJONEHUB_IOS_INTEROP=1 Go fixture to run native/Pion interoperability") }
    let offer = try XCTUnwrap(JSONSerialization.jsonObject(with: offerData) as? [String: Any])
    let sdp = try XCTUnwrap(offer["sdp"] as? String)
    var invite = Signal(type: "invite", payload: [:])
    invite.hub_epoch = "epoch"
    invite.module_id = "module"
    invite.call_uuid = UUID().uuidString.lowercased()
    invite.installation_id = "phone"
    invite.generation = 1
    func fetchICE(renew: Bool = false) async throws -> ICECredentials {
      let url = URL(string: renew ? "ice?renew=1" : "ice", relativeTo: base.appendingPathComponent("/"))!
      let (data, _) = try await session.data(from: url)
      return try Wire.decoder().decode(ICECredentials.self, from: data)
    }
    let ice = try await fetchICE()
    let phone = try PhoneMedia(binding: XCTUnwrap(invite.binding), ice: ice, recoveryEnabled: true, controlEnabled: true)
    phone.onDiagnostic = { print("interop_media_" + $0) }
    defer { phone.close() }
    func post(_ path: String, _ data: Data) async throws {
      var request = URLRequest(url: base.appendingPathComponent(path))
      request.httpMethod = "POST"
      request.httpBody = data
      request.setValue("application/json", forHTTPHeaderField: "Content-Type")
      let (_, response) = try await session.data(for: request)
      XCTAssertEqual((response as? HTTPURLResponse)?.statusCode, 204, path)
    }
    let connected = expectation(description: "native SDK and Pion ICE/DTLS connected")
    var fulfilled = false
    phone.onConnected = {
      if !fulfilled {
        fulfilled = true
        connected.fulfill()
      }
    }
    var candidateTasks: [Task<Void, Never>] = []
    phone.onCandidate = { payload in
      candidateTasks.append(
        Task { @MainActor in
          do { try await post("candidate", Wire.encoder().encode(payload)) } catch {
            XCTFail("Candidate HTTP transport failed")
          }
        })
    }
    let polling = Task { @MainActor in
      while !Task.isCancelled {
        do {
          let (data, _) = try await session.data(from: base.appendingPathComponent("candidates"))
          let candidates = try Wire.decoder().decode([[String: Scalar]].self, from: data)
          for candidate in candidates { phone.candidate(candidate) }
          try await Task.sleep(for: .milliseconds(100))
        } catch { return }
      }
    }
    defer { polling.cancel() }
    let answer = try await phone.answer(sdp)
    print("interop_initial_answer")
    try await post(
      "answer", JSONSerialization.data(withJSONObject: ["type": "answer", "sdp": answer]))
    await fulfillment(of: [connected], timeout: 15)
    XCTAssertFalse(RTCAudioSession.sharedInstance().isAudioEnabled)
    try await post("audio", Data())
    var received = AudioReceiveSnapshot()
    for _ in 0..<50 {
      received = await phone.receiveSnapshot()
      if received.packets >= 5 { break }
      try await Task.sleep(for: .milliseconds(100))
    }
    XCTAssertGreaterThanOrEqual(received.packets, 5, "Native SDK must receive real Pion Opus RTP, not merely connect ICE")
    XCTAssertGreaterThan(received.bytes, 0)
    phone.onCandidate = nil
    for task in candidateTasks { await task.value }
    polling.cancel()
    await polling.value
    for expectedID in 1...2 {
      let (data, _) = try await session.data(from: base.appendingPathComponent("restart-offer"))
      let offer = try XCTUnwrap(JSONSerialization.jsonObject(with: data) as? [String: Any])
      let id = try XCTUnwrap(offer["negotiation_id"] as? UInt64)
      XCTAssertEqual(id, UInt64(expectedID))
      let sdp = try XCTUnwrap(offer["sdp"] as? String)
      let previous = await phone.receiveSnapshot()
      let oldExpiry = phone.turnExpiry
      print("interop_restart_begin_\(id)")
      var fetches = 0
      let refresh = { @MainActor in
        fetches += 1
        return try await fetchICE(renew: true)
      }
      let answer = try await phone.answerRestart(sdp, id: id, refreshICE: refresh)
      print("interop_restart_answer_\(id)")
      try await post("restart-answer", JSONSerialization.data(withJSONObject: ["negotiation_id": id, "sdp": answer]))
      var after = previous
      for _ in 0..<50 {
        after = await phone.receiveSnapshot()
        if after.packets >= previous.packets + 5 { break }
        try await Task.sleep(for: .milliseconds(100))
      }
      XCTAssertGreaterThanOrEqual(after.packets, previous.packets + 5)
      XCTAssertFalse(RTCAudioSession.sharedInstance().isAudioEnabled)
      if let oldExpiry {
        for _ in 0..<30 {
          if phone.turnExpiry != oldExpiry { break }
          try await Task.sleep(for: .milliseconds(100))
        }
        XCTAssertGreaterThan(try XCTUnwrap(phone.turnExpiry), oldExpiry)
      }
      let replay = try await phone.answerRestart(sdp, id: id, refreshICE: refresh)
      XCTAssertEqual(replay, answer, "Exact retry must return the same answer without restarting ICE")
      XCTAssertEqual(fetches, ice.enabled ? 1 : 0, "Retry must not rotate credentials or allocation again")
      do {
        _ = try await phone.answerRestart(sdp + "a=conflict:1\r\n", id: id)
        XCTFail("Conflicting ICE restart was accepted")
      } catch {}
    }
    for _ in 0..<50 {
      if phone.controlReady { break }
      try await Task.sleep(for: .milliseconds(100))
    }
    XCTAssertTrue(phone.controlReady)
    let stateReceived = expectation(description: "SCTP state received")
    let terminalReceived = expectation(description: "control survives audio shutdown")
    phone.onControl = { data in
      if String(data: data, encoding: .utf8)?.contains("fixture_ended") == true { terminalReceived.fulfill() }
      else { stateReceived.fulfill() }
    }
    try await post("control", Data())
    await fulfillment(of: [stateReceived], timeout: 3)
    XCTAssertTrue(phone.sendControl(Signal(type: "sync", binding: invite.binding!, payload: [:])))
    try await post("control-received", Data())
    try await post("quiesce", Data())
    await fulfillment(of: [terminalReceived], timeout: 3)
    try await post("complete", Data())
    XCTAssertFalse(RTCAudioSession.sharedInstance().isAudioEnabled)
  }
  func testCandidateDiagnosticDoesNotExposeAddressesOrUntrustedFields() {
    XCTAssertEqual(
      iceCandidateKind("candidate:secret 1 udp 1 192.0.2.1 1000 typ relay raddr 10.0.0.1"), "relay")
    XCTAssertEqual(
      iceCandidateKind("candidate:secret 1 udp 1 private 1000 typ secret_token"), "unknown")
    XCTAssertEqual(iceCandidateKind("secret_token"), "unknown")
  }
  func testFailureCodeExcludesErrorUserInfo() {
    let error = NSError(
      domain: NSURLErrorDomain, code: NSURLErrorTimedOut,
      userInfo: [
        NSLocalizedDescriptionKey: "secret_token",
        NSURLErrorFailingURLStringErrorKey: "https://example.com/?token=secret_token",
      ])
    XCTAssertEqual(phoneFailureCode(error), "url_-1001")
    XCTAssertEqual(phoneFailureCode(RelayHTTPFailure(statusCode: 403)), "http_403")
    XCTAssertEqual(phoneFailureCode(PhoneError.invalidData), "invalid_data")
  }
  func testCallKitDomainsDistinguishMissingVoIPModeFromDoNotDisturb() {
    let info = [NSLocalizedDescriptionKey: "secret_token"]
    XCTAssertEqual(
      phoneFailureCode(NSError(domain: CXErrorDomain, code: 3, userInfo: info)),
      "callkit_general_3")
    XCTAssertEqual(
      phoneFailureCode(NSError(domain: CXErrorDomainIncomingCall, code: 3, userInfo: info)),
      "callkit_incoming_3")
  }
  @MainActor func testNativeOpusNegotiatesWithoutOpeningAudioBeforeCallKit() async throws {
    let id = UUID().uuidString.lowercased()
    let invite = Signal(type: "invite", payload: [:])
    var m = invite
    m.hub_epoch = "epoch"
    m.module_id = "module"
    m.call_uuid = id
    m.installation_id = "phone"
    m.generation = 1
    let phone = try PhoneMedia(binding: XCTUnwrap(m.binding))
    defer { phone.close() }
    let observer = PeerObserver()
    let factory = RTCPeerConnectionFactory()
    let config = RTCConfiguration()
    config.sdpSemantics = .unifiedPlan
    let constraints = RTCMediaConstraints(mandatoryConstraints: nil, optionalConstraints: nil)
    let hub = try XCTUnwrap(
      factory.peerConnection(with: config, constraints: constraints, delegate: observer))
    defer { hub.close() }
    let track = factory.audioTrack(with: factory.audioSource(with: constraints), trackId: "hub")
    track.isEnabled = false
    hub.add(track, streamIds: [id])
    observer.ice = { candidate in
      phone.candidate([
        "candidate": .string(candidate.sdp),
        "sdpMLineIndex": .uint(UInt64(candidate.sdpMLineIndex)),
        "sdpMid": candidate.sdpMid.map(Scalar.string) ?? .null,
      ])
    }
    var remoteSet = false
    var pending: [RTCIceCandidate] = []
    phone.onCandidate = { payload in
      let c = RTCIceCandidate(
        sdp: payload["candidate"]!.string!, sdpMLineIndex: Int32(payload["sdpMLineIndex"]!.uint!),
        sdpMid: payload["sdpMid"]?.string)
      if remoteSet { hub.add(c) { _ in } } else { pending.append(c) }
    }
    let connected = expectation(description: "ICE and DTLS connected")
    var fulfilled = false
    phone.onConnected = {
      if !fulfilled {
        fulfilled = true
        connected.fulfill()
      }
    }
    let offer: RTCSessionDescription = try await withCheckedThrowingContinuation { c in
      hub.offer(for: constraints) { sdp, error in
        if let sdp {
          c.resume(returning: sdp)
        } else {
          c.resume(throwing: error ?? PhoneError.unavailable)
        }
      }
    }
    try await withCheckedThrowingContinuation { (c: CheckedContinuation<Void, Error>) in
      hub.setLocalDescription(offer) { error in
        if let error { c.resume(throwing: error) } else { c.resume() }
      }
    }
    let answer = try await phone.answer(offer.sdp)
    XCTAssertTrue(answer.lowercased().contains("opus/48000"))
    try await withCheckedThrowingContinuation { (c: CheckedContinuation<Void, Error>) in
      hub.setRemoteDescription(RTCSessionDescription(type: .answer, sdp: answer)) { error in
        if let error { c.resume(throwing: error) } else { c.resume() }
      }
    }
    remoteSet = true
    for c in pending { hub.add(c) { _ in } }
    await fulfillment(of: [connected], timeout: 15)
    XCTAssertTrue(RTCAudioSession.sharedInstance().useManualAudio)
    XCTAssertFalse(RTCAudioSession.sharedInstance().isAudioEnabled)
    phone.setAudio(true)  // even accidental enable must require system activation
    XCTAssertFalse(RTCAudioSession.sharedInstance().isAudioEnabled)
  }
}
