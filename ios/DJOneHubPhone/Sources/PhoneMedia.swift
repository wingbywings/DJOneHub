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

@MainActor final class PhoneMedia: NSObject, RTCPeerConnectionDelegate {
  let binding: Binding
  var onCandidate: (([String: Scalar]) -> Void)?
  var onConnected: (() -> Void)?
  var onFailure: (() -> Void)?
  var onDiagnostic: ((String) -> Void)?
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
  private var remoteSet = false, closed = false, muted = false, audioEnabled = false,
    systemAudioActive = false
  init(binding: Binding, ice: ICECredentials = .direct) throws {
    try ice.validate()
    self.binding = binding
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
  func answer(_ sdp: String) async throws -> String {
    guard !closed, sdp.utf8.count <= 65536,
      sdp.components(separatedBy: "\n").filter({ $0.hasPrefix("m=") }).count == 1,
      sdp.contains("m=audio ")
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
    return answer.sdp
  }
  func candidate(_ payload: [String: Scalar]) {
    guard !closed, let sdp = payload["candidate"]?.string, !sdp.isEmpty else { return }
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
  func close() {
    guard !closed else { return }
    setAudio(false)
    closed = true
    diagnosticsTask?.cancel()
    diagnosticsTask = nil
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
    dataChannel.close()
    Task { @MainActor [weak self] in
      guard let self, !self.closed else { return }
      self.onFailure?()
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
        self.startDiagnostics()
        self.onConnected?()
      }
      if newState == .failed || newState == .disconnected || newState == .closed {
        self.onFailure?()
      }
    }
  }
}
