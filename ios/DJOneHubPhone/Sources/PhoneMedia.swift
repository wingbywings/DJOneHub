import AVFAudio
import DJOneHubPhoneCore
import Foundation
import WebRTC

func iceCandidateKind(_ sdp: String) -> String {
  let fields = sdp.split(whereSeparator: { $0.isWhitespace })
  guard let index = fields.firstIndex(of: "typ"), index + 1 < fields.count else { return "unknown" }
  let kind = String(fields[index + 1])
  return ["host", "srflx", "prflx", "relay"].contains(kind) ? kind : "unknown"
}

struct AudioReceiveSnapshot {
  var packets: UInt64 = 0, bytes: UInt64 = 0, samples: UInt64 = 0, concealed: UInt64 = 0
  var energy: Double = 0
}

@MainActor final class PhoneMedia: NSObject, RTCPeerConnectionDelegate, RTCDataChannelDelegate {
  let binding: Binding
  var onCandidate: (([String: Scalar]) -> Void)?
  var onConnected: (() -> Void)?
  var onFailure: (() -> Void)?
  var onDiagnostic: ((String) -> Void)?
  var onControl: ((Data) -> Void)?
  private let controlEnabled: Bool
  private var control: RTCDataChannel?
  private var controlWindow = Date.distantPast
  private var controlCount = 0
  private var localCandidateCount = 0, remoteCandidateCount = 0
  private var localKinds = Set<String>(), remoteKinds = Set<String>()
  private static let factory: RTCPeerConnectionFactory = {
    RTCInitializeSSL()
    return RTCPeerConnectionFactory()
  }()
  private var peer: RTCPeerConnection!
  private var track: RTCAudioTrack!
  private var pending: [RTCIceCandidate] = []
  private var diagnosticsTask: Task<Void, Never>?
  private var recoveryTask: Task<Void, Never>?
  private let recoveryEnabled: Bool
  private var restartID: UInt64 = 0
  private var restarting = false
  private var restartTask: Task<String, Error>?
  private var restartProposal: (id: UInt64, sdp: String)?
  private(set) var turnExpiry: Date?
  private var pendingTurnExpiry: Date?
  private var renewalConfirmation: Task<Void, Never>?
  private let relayOnly: Bool
  private var turnUsername: String?
  private var fullGatherOnly = false
  private var remoteSet = false, closed = false, muted = false, audioEnabled = false,
    systemAudioActive = false
  init(binding: Binding, ice: ICECredentials = .direct, recoveryEnabled: Bool = false, controlEnabled: Bool = false) throws {
    try ice.validate()
    self.binding = binding
    self.recoveryEnabled = recoveryEnabled
    self.controlEnabled = controlEnabled
    self.turnExpiry = ice.expires_at
    self.relayOnly = ice.relay_only
    self.turnUsername = ice.servers.first?.username
    super.init()
    let audio = RTCAudioSession.sharedInstance()
    audio.useManualAudio = true
    audio.isAudioEnabled = false
    try configureAudio()
    let config = RTCConfiguration()
    config.sdpSemantics = .unifiedPlan
    config.bundlePolicy = .maxBundle
    config.rtcpMuxPolicy = .require
    config.iceServers = ice.servers.map {
      RTCIceServer(urlStrings: $0.urls, username: $0.username, credential: $0.credential)
    }
    config.iceTransportPolicy = ice.relay_only ? .relay : .all
    let constraints = RTCMediaConstraints(mandatoryConstraints: nil, optionalConstraints: nil)
    guard
      let pc = Self.factory.peerConnection(with: config, constraints: constraints, delegate: self)
    else { throw PhoneError.unavailable }
    peer = pc
    let source = Self.factory.audioSource(with: constraints)
    track = Self.factory.audioTrack(with: source, trackId: "phone-audio")
    track.isEnabled = false
    let sender = pc.add(track, streamIds: [binding.callUUID])
    if let sender {
      let parameters = sender.parameters
      for encoding in parameters.encodings {
        encoding.maxBitrateBps = 24000
        encoding.adaptiveAudioPacketTime = false
      }
      sender.parameters = parameters
    }
  }
  func configureAudio() throws {
    let a = RTCAudioSession.sharedInstance()
    a.lockForConfiguration()
    defer { a.unlockForConfiguration() }
    try a.setCategory(
      AVAudioSession.Category.playAndRecord, mode: AVAudioSession.Mode.voiceChat,
      options: [.allowBluetoothHFP])
    try a.setPreferredSampleRate(48000)
    try a.setPreferredIOBufferDuration(0.02)
    // CallKit, not this class, activates AVAudioSession.
  }
  func answer(_ sdp: String, gather: Bool = false) async throws -> String {
    let media = sdp.components(separatedBy: "\n").filter { $0.hasPrefix("m=") }
    guard !closed, sdp.utf8.count <= 65536,
      media.first?.hasPrefix("m=audio ") == true,
      media.count == 1 || (controlEnabled && media.count == 2 && media[1].hasPrefix("m=application "))
    else { throw PhoneError.invalidData }
    try await withCheckedThrowingContinuation { (c: CheckedContinuation<Void, Error>) in
      peer.setRemoteDescription(RTCSessionDescription(type: .offer, sdp: sdp)) { error in
        if let error { c.resume(throwing: error) } else { c.resume() }
      }
    }
    guard !closed else { throw PhoneError.unavailable }
    remoteSet = true
    for candidate in pending { add(candidate) }
    pending.removeAll()
    let answer: RTCSessionDescription = try await withCheckedThrowingContinuation { c in
      peer.answer(for: RTCMediaConstraints(mandatoryConstraints: nil, optionalConstraints: nil)) {
        sdp, error in
        if let error {
          c.resume(throwing: error)
        } else if let sdp {
          c.resume(returning: sdp)
        } else {
          c.resume(throwing: PhoneError.unavailable)
        }
      }
    }
    guard !closed, answer.sdp.utf8.count <= 65536, answer.sdp.lowercased().contains("opus/48000")
    else { throw PhoneError.invalidData }
    try await withCheckedThrowingContinuation { (c: CheckedContinuation<Void, Error>) in
      peer.setLocalDescription(answer) { error in
        if let error { c.resume(throwing: error) } else { c.resume() }
      }
    }
    guard !closed else { throw PhoneError.unavailable }
    for sender in peer.senders {
      let parameters = sender.parameters
      for encoding in parameters.encodings {
        encoding.maxBitrateBps = 24000
        encoding.adaptiveAudioPacketTime = false
      }
      sender.parameters = parameters
    }
    if gather {
      let deadline = ContinuousClock.now.advanced(by: .seconds(5))
      var firstCandidateAt: ContinuousClock.Instant?
      while peer.iceGatheringState != .complete {
        guard !closed else { throw PhoneError.unavailable }
        // Gathering can wait for the remote answer to prune obsolete ports.
        // Once usable candidates exist, allow a short collection window and
        // send the snapshot instead of adding five seconds of avoidable silence.
        let candidates = peer.localDescription?.sdp.components(separatedBy: .newlines)
          .filter { $0.hasPrefix("a=candidate:") } ?? []
        let usable = candidates.contains { !relayOnly || iceCandidateKind($0) == "relay" }
        if usable && firstCandidateAt == nil { firstCandidateAt = .now }
        if usable && (firstCandidateAt.map { ContinuousClock.now >= $0.advanced(by: .milliseconds(300)) } == true || ContinuousClock.now >= deadline) {
          onDiagnostic?("ice_gathering_candidate_snapshot")
          break
        }
        guard ContinuousClock.now < deadline else { throw PhoneError.unavailable }
        try await Task.sleep(for: .milliseconds(20))
      }
      guard !closed, let complete = peer.localDescription, complete.sdp.utf8.count <= 65536 else {
        throw PhoneError.invalidData
      }
      return complete.sdp
    }
    return answer.sdp
  }
  func answerRestart(_ sdp: String, id: UInt64, refreshICE: (() async throws -> ICECredentials)? = nil) async throws -> String {
    guard recoveryEnabled, !closed else { throw PhoneError.invalidData }
    if let proposal = restartProposal, proposal.id == id {
      guard proposal.sdp == sdp, let task = restartTask else { throw PhoneError.invalidData }
      return try await task.value // exact retry resends the cached/in-flight answer
    }
    guard !restarting, restartID != UInt64.max, id == restartID + 1,
      peer.signalingState == .stable, let previous = peer.remoteDescription,
      ICERevision.isRestart(previous: previous.sdp, next: sdp)
    else { throw PhoneError.invalidData }
    restarting = true
    fullGatherOnly = true
    pending.removeAll()
    restartProposal = (id, sdp)
    let recoveryDeadline = ContinuousClock.now.advanced(by: .seconds(10))
    let task = Task { @MainActor in
      var previousPair: String?
      if let previousExpiry = self.turnExpiry {
        guard let refreshICE else { throw PhoneError.unavailable }
        let ice = try await refreshICE()
        try ice.validate()
        guard !self.closed, ice.enabled, ice.relay_only == self.relayOnly,
          let expiry = ice.expires_at, expiry >= previousExpiry,
          ice.servers.first?.username != self.turnUsername else { throw PhoneError.invalidData }
        let config = self.peer.configuration
        config.iceServers = ice.servers.map {
          RTCIceServer(urlStrings: $0.urls, username: $0.username, credential: $0.credential)
        }
        previousPair = await self.selectedCandidatePair()
        guard previousPair != nil else { throw PhoneError.unavailable }
        guard self.peer.setConfiguration(config) else { throw PhoneError.unavailable }
        self.pendingTurnExpiry = expiry
        self.turnUsername = ice.servers.first?.username
      }
      let answer = try await self.answer(sdp, gather: true)
      if let previousPair { self.confirmRenewal(replacing: previousPair, deadline: recoveryDeadline) }
      return answer
    }
    restartTask = task
    defer { restarting = false }
    let response = try await task.value
    guard !closed else { throw PhoneError.unavailable }
    restartID = id
    return response
  }
  private func selectedCandidatePair() async -> String? {
    await withCheckedContinuation { continuation in
      peer.statistics { report in
        for stat in report.statistics.values where stat.type == "transport" {
          if let id = stat.values["selectedCandidatePairId"] as? String,
            let pair = report.statistics[id], pair.values["state"] as? String == "succeeded" {
            continuation.resume(returning: id); return
          }
        }
        continuation.resume(returning: nil)
      }
    }
  }
  private func confirmRenewal(replacing previousPair: String, deadline: ContinuousClock.Instant) {
    renewalConfirmation?.cancel()
    renewalConfirmation = Task { @MainActor [weak self] in
      while ContinuousClock.now < deadline {
        guard !Task.isCancelled, let self, !self.closed, self.pendingTurnExpiry != nil else { return }
        if let pair = await self.selectedCandidatePair(), pair != previousPair,
          self.peer.connectionState == .connected {
          self.turnExpiry = self.pendingTurnExpiry
          self.pendingTurnExpiry = nil
          self.onDiagnostic?("turn_renewed")
          return
        }
        do { try await Task.sleep(for: .milliseconds(100)) } catch { return }
      }
      guard let self, !self.closed else { return }
      self.onDiagnostic?("turn_renewal_unconfirmed")
      self.onFailure?()
    }
  }
  func candidate(_ payload: [String: Scalar]) {
    guard !closed, !fullGatherOnly, let sdp = payload["candidate"]?.string, !sdp.isEmpty else { return }
    remoteCandidateCount += 1
    let kind = iceCandidateKind(sdp)
    if remoteKinds.insert(kind).inserted { onDiagnostic?("remote_ice_" + kind) }
    let candidate = RTCIceCandidate(
      sdp: sdp, sdpMLineIndex: Int32(payload["sdpMLineIndex"]?.uint ?? 0),
      sdpMid: payload["sdpMid"]?.string)
    if remoteSet {
      add(candidate)
    } else if pending.count < 256 {
      pending.append(candidate)
    } else {
      onFailure?()
    }
  }
  private func add(_ candidate: RTCIceCandidate) {
    peer.add(candidate) { [weak self] error in
      if error != nil {
        Task { @MainActor in
          guard let self, !self.closed else { return }
          self.onDiagnostic?("remote_ice_add_" + phoneFailureCode(error!))
          self.onFailure?()
        }
      }
    }
  }
  func activated(_ audio: AVAudioSession) {
    guard !closed else { return }
    systemAudioActive = true
    RTCAudioSession.sharedInstance().audioSessionDidActivate(audio)
  }
  func deactivated(_ audio: AVAudioSession) {
    systemAudioActive = false
    setAudio(false)
    RTCAudioSession.sharedInstance().audioSessionDidDeactivate(audio)
  }
  func setMuted(_ value: Bool) {
    muted = value
    track?.isEnabled = audioEnabled && !muted && !closed
  }
  func setAudio(_ enabled: Bool) {
    guard !closed else { return }
    audioEnabled = enabled && systemAudioActive
    track?.isEnabled = audioEnabled && !muted
    RTCAudioSession.sharedInstance().isAudioEnabled = audioEnabled
  }
  // Whitelist numeric audio counters; do not log the full WebRTC report,
  // which also contains network addresses and identifiers.
  func receiveSnapshot() async -> AudioReceiveSnapshot {
    guard !closed else { return AudioReceiveSnapshot() }
    return await withCheckedContinuation { continuation in
      peer.statistics { report in
        var result = AudioReceiveSnapshot()
        for stat in report.statistics.values where stat.type == "inbound-rtp" {
          let values = stat.values
          guard (values["kind"] as? String ?? values["mediaType"] as? String) == "audio" else { continue }
          result.packets += (values["packetsReceived"] as? NSNumber)?.uint64Value ?? 0
          result.bytes += (values["bytesReceived"] as? NSNumber)?.uint64Value ?? 0
          result.samples += (values["totalSamplesReceived"] as? NSNumber)?.uint64Value ?? 0
          result.concealed += (values["concealedSamples"] as? NSNumber)?.uint64Value ?? 0
          result.energy += (values["totalAudioEnergy"] as? NSNumber)?.doubleValue ?? 0
        }
        continuation.resume(returning: result)
      }
    }
  }
  private func startDiagnostics() {
    guard diagnosticsTask == nil else { return }
    diagnosticsTask = Task { @MainActor [weak self] in
      while !Task.isCancelled {
        do { try await Task.sleep(for: .seconds(5)) } catch { return }
        guard let self, !self.closed else { return }
        let snapshot = await self.receiveSnapshot()
        guard !self.closed, !Task.isCancelled else { return }
        let receivers = self.peer.receivers.compactMap { $0.track as? RTCAudioTrack }
        let audio = AVAudioSession.sharedInstance()
        let route = audio.currentRoute.outputs.first?.portType.rawValue ?? "none"
        self.onDiagnostic?("audio_rx packets=\(snapshot.packets) bytes=\(snapshot.bytes) samples=\(snapshot.samples) concealed=\(snapshot.concealed) energy=\(String(format: "%.6f", snapshot.energy))")
        self.onDiagnostic?("audio_play enabled=\(self.audioEnabled) tracks=\(receivers.count) enabled_tracks=\(receivers.filter { $0.isEnabled }.count) route=\(route) volume=\(String(format: "%.2f", audio.outputVolume))")
      }
    }
  }
  var controlReady: Bool { !closed && control?.readyState == .open }
  @discardableResult func sendControl(_ message: Signal) -> Bool {
    guard controlReady, let control, control.bufferedAmount <= 32768,
      let data = try? message.encoded(), data.count <= 8192 else { return false }
    return control.sendData(RTCDataBuffer(data: data, isBinary: false))
  }
  func close() {
    control?.delegate = nil
    control?.close()
    control = nil
    restartTask?.cancel()
    restartTask = nil
    restartProposal = nil
    guard !closed else { return }
    setAudio(false)
    closed = true
    diagnosticsTask?.cancel()
    diagnosticsTask = nil
    recoveryTask?.cancel()
    recoveryTask = nil
    renewalConfirmation?.cancel()
    renewalConfirmation = nil
    peer?.close()
    pending.removeAll()
    onCandidate = nil
    onConnected = nil
    onFailure = nil
    onDiagnostic = nil
  }
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
  ) {
    Task { @MainActor [weak self] in
      guard let self, !self.closed else { return }
      self.onDiagnostic?("ice_state_\(newState.rawValue)")
    }
  }
  nonisolated func peerConnection(
    _ peerConnection: RTCPeerConnection, didChange newState: RTCIceGatheringState
  ) {
    Task { @MainActor [weak self] in
      guard let self, !self.closed else { return }
      self.onDiagnostic?("ice_gathering_\(newState.rawValue)")
      if newState == .complete {
        self.onDiagnostic?(
          "ice_candidates_local_\(self.localCandidateCount)_remote_\(self.remoteCandidateCount)")
      }
    }
  }
  nonisolated func peerConnection(
    _ peerConnection: RTCPeerConnection, didRemove candidates: [RTCIceCandidate]
  ) {}
  nonisolated func peerConnection(
    _ peerConnection: RTCPeerConnection, didOpen dataChannel: RTCDataChannel
  ) {
    Task { @MainActor [weak self] in
      guard let self, !self.closed else { dataChannel.close(); return }
      guard self.controlEnabled, self.control == nil, dataChannel.label == "call-control",
        dataChannel.protocol == "djonehub.calls.v1", dataChannel.isOrdered,
        dataChannel.isReliable, !dataChannel.isNegotiated else {
        dataChannel.close(); self.onFailure?(); return
      }
      self.control = dataChannel
      dataChannel.delegate = self
      self.onDiagnostic?("control_channel_open")
    }
  }
  nonisolated func dataChannelDidChangeState(_ dataChannel: RTCDataChannel) {}
  nonisolated func dataChannel(_ dataChannel: RTCDataChannel, didReceiveMessageWith buffer: RTCDataBuffer) {
    guard !buffer.isBinary, !buffer.data.isEmpty, buffer.data.count <= 8192 else {
      dataChannel.close()
      Task { @MainActor [weak self] in self?.onFailure?() }
      return
    }
    let data = buffer.data
    Task { @MainActor [weak self] in
      guard let self, !self.closed, self.control === dataChannel else { return }
      let now = Date()
      if now.timeIntervalSince(self.controlWindow) >= 1 { self.controlWindow = now; self.controlCount = 0 }
      self.controlCount += 1
      guard self.controlCount <= 20 else { dataChannel.close(); self.onFailure?(); return }
      self.onControl?(data)
    }
  }
  nonisolated func peerConnection(
    _ peerConnection: RTCPeerConnection, didGenerate candidate: RTCIceCandidate
  ) {
    let payload: [String: Scalar] = [
      "candidate": .string(candidate.sdp), "sdpMid": candidate.sdpMid.map(Scalar.string) ?? .null,
      "sdpMLineIndex": .uint(UInt64(max(0, candidate.sdpMLineIndex))),
    ]
    Task { @MainActor [weak self] in
      guard let self, !self.closed else { return }
      self.localCandidateCount += 1
      let kind = iceCandidateKind(candidate.sdp)
      if self.localKinds.insert(kind).inserted { self.onDiagnostic?("local_ice_" + kind) }
      if self.fullGatherOnly { return }
      self.onCandidate?(payload)
    }
  }
  nonisolated func peerConnection(
    _ peerConnection: RTCPeerConnection, didChange newState: RTCPeerConnectionState
  ) {
    Task { @MainActor [weak self] in
      guard let self, !self.closed else { return }
      self.onDiagnostic?("peer_state_\(newState.rawValue)")
      if newState == .connected {
        self.recoveryTask?.cancel()
        self.recoveryTask = nil
        self.startDiagnostics()
        self.onConnected?()
      }
      if self.recoveryEnabled && (newState == .failed || newState == .disconnected) {
        guard self.recoveryTask == nil else { return }
        self.onDiagnostic?("transport_recovering")
        self.recoveryTask = Task { @MainActor [weak self] in
          do { try await Task.sleep(for: .seconds(10)) } catch { return }
          guard let self, !self.closed else { return }
          self.onDiagnostic?("transport_recovery_timeout")
          self.onFailure?()
        }
      } else if newState == .failed || newState == .disconnected || newState == .closed {
        self.onFailure?()
      }
    }
  }
}
