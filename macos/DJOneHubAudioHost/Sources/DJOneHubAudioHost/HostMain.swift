import Foundation
import Darwin

private struct DeviceSummary: Decodable {
    let id: String
    let state: String
}

private struct CallRecord: Decodable {
    let id: String
    let state: String
}

private struct CallStatus: Decodable {
    let active: CallRecord?
}

private struct AudioHostConfig: Decodable {
    let deviceID: String
    let vendorID: UInt16
    let productID: UInt16
    let locationID: UInt32
    let routeReady: Bool
    let routeError: String?
    let callID: String?
    let callActive: Bool
    let muted: Bool
    let recording: Bool
    let mediaMode: String
    let agentProvider: String?
    let agentRevision: UInt64?
    let agentMediaURL: String?

    enum CodingKeys: String, CodingKey {
        case deviceID = "device_id"
        case vendorID = "vendor_id"
        case productID = "product_id"
        case locationID = "location_id"
        case routeReady = "route_ready"
        case routeError = "route_error"
        case callID = "call_id"
        case callActive = "call_active"
        case muted, recording
        case mediaMode = "media_mode"
        case agentProvider = "agent_provider"
        case agentRevision = "agent_revision"
        case agentMediaURL = "agent_media_url"
    }
}

private struct APIErrorBody: Decodable { let error: String? }

private final class APIClient {
    let baseURL: URL
    let session: URLSession

    init(baseURL: URL) {
        self.baseURL = baseURL
        let configuration = URLSessionConfiguration.ephemeral
        configuration.timeoutIntervalForRequest = 5
        self.session = URLSession(configuration: configuration)
    }

    func devices() async throws -> [DeviceSummary] {
        try await get(path: "/api/devices")
    }

    func callStatus(deviceID: String?) async throws -> CallStatus {
        try await get(path: devicePath(deviceID, "/calls/status"))
    }

    func audioConfig(deviceID: String?) async throws -> AudioHostConfig {
        try await get(path: devicePath(deviceID, "/calls/audio/host/config"))
    }

    func absoluteURL(path: String) -> URL { url(path) }

    func register(
        deviceID: String?, enabled: Bool, running: Bool, muted: Bool,
        recording: Bool, recordingPath: String?, error: String?
    ) async {
        let body: [String: Any] = [
            "enabled": enabled,
            "running": running,
            "muted": muted,
            "recording": recording,
            "recording_path": recordingPath ?? "",
            "error": error ?? "",
        ]
        try? await post(path: devicePath(deviceID, "/calls/audio/host/register"), body: body)
    }

    private func devicePath(_ deviceID: String?, _ suffix: String) -> String {
        guard let deviceID, !deviceID.isEmpty else { return "/api" + suffix }
        return "/api/devices/\(deviceID.addingPercentEncoding(withAllowedCharacters: .urlPathAllowed) ?? deviceID)" + suffix
    }

    private func get<T: Decodable>(path: String) async throws -> T {
        let (data, response) = try await session.data(from: url(path))
        try validate(response: response, data: data)
        return try JSONDecoder.djOneHub.decode(T.self, from: data)
    }

    private func post(path: String, body: [String: Any]) async throws {
        var request = URLRequest(url: url(path))
        request.httpMethod = "POST"
        request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        request.httpBody = try JSONSerialization.data(withJSONObject: body)
        let (data, response) = try await session.data(for: request)
        try validate(response: response, data: data)
    }

    private func validate(response: URLResponse, data: Data) throws {
        guard let response = response as? HTTPURLResponse else {
            throw HostError.message("DJOneHub 返回了无效 HTTP 响应")
        }
        guard (200 ..< 300).contains(response.statusCode) else {
            let message = (try? JSONDecoder().decode(APIErrorBody.self, from: data).error) ?? "HTTP \(response.statusCode)"
            throw HostError.message(message)
        }
    }

    private func url(_ path: String) -> URL {
        URL(string: path, relativeTo: baseURL)!.absoluteURL
    }
}

private enum HostError: LocalizedError {
    case message(String)
    var errorDescription: String? {
        switch self { case .message(let value): return value }
    }
}

private extension JSONDecoder {
    static var djOneHub: JSONDecoder {
        let decoder = JSONDecoder()
        decoder.dateDecodingStrategy = .iso8601
        return decoder
    }
}

@main
struct DJOneHubAudioHostMain {
    static func main() async {
        let arguments = CommandLine.arguments
        let baseText = value(after: "--base-url", in: arguments) ?? "http://127.0.0.1:7575"
        let fixedDeviceID = value(after: "--device-id", in: arguments)
        guard
            let baseURL = URL(string: baseText),
            let scheme = baseURL.scheme?.lowercased(),
            (scheme == "http" || scheme == "https"),
            baseURL.host != nil
        else {
            fputs("DJOneHubAudioHost: invalid --base-url; use plain text such as http://127.0.0.1:7575\n", stderr)
            return
        }
        let client = APIClient(baseURL: baseURL)
        let controller = AudioHostController(client: client, fixedDeviceID: fixedDeviceID)
        let runTask = Task { await controller.run() }
        await waitForTerminationSignal()
        runTask.cancel()
        await runTask.value
    }

    private static func value(after name: String, in arguments: [String]) -> String? {
        guard let index = arguments.firstIndex(of: name), arguments.indices.contains(index + 1) else { return nil }
        return arguments[index + 1]
    }

    private static func waitForTerminationSignal() async {
        signal(SIGINT, SIG_IGN)
        signal(SIGTERM, SIG_IGN)
        await withCheckedContinuation { continuation in
            let lock = NSLock()
            var resumed = false
            let interrupt = DispatchSource.makeSignalSource(signal: SIGINT, queue: .main)
            let terminate = DispatchSource.makeSignalSource(signal: SIGTERM, queue: .main)
            let finish = {
                lock.lock()
                defer { lock.unlock() }
                guard !resumed else { return }
                resumed = true
                interrupt.cancel()
                terminate.cancel()
                continuation.resume()
            }
            interrupt.setEventHandler(handler: finish)
            terminate.setEventHandler(handler: finish)
            interrupt.resume()
            terminate.resume()
        }
    }
}

@MainActor
private final class AudioHostController {
    private let client: APIClient
    private let fixedDeviceID: String?
    private let audio = VoiceAudioService()
    private let agentMedia = AgentMediaClient()
    private var currentDeviceID: String?
    private var currentCallID: String?
    private var isMuted = false
    private var isRecording = false
    private var recordingPath: String?
    private var lastError: String?
    private var knownDeviceIDs: [String] = []
    private var isAgentMode = false
    private var currentAgentRevision: UInt64?

    init(client: APIClient, fixedDeviceID: String?) {
        self.client = client
        self.fixedDeviceID = fixedDeviceID
        audio.onError = { [weak self] message in self?.lastError = message }
        audio.onAgentInput = { [weak self] pcm in self?.agentMedia.send(pcm) }
        agentMedia.onAudio = { [weak self] pcm in self?.audio.enqueueAgentOutput(pcm) }
        agentMedia.onEvent = { [weak self] raw in
            guard
                let data = raw.data(using: .utf8),
                let event = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
                let type = event["type"] as? String
            else { return }
            if type == "speech.started" {
                self?.audio.clearAgentOutput()
            } else if type == "audio.done" {
                self?.audio.finishAgentOutputTurn()
            }
        }
        agentMedia.onError = { [weak self] message in self?.lastError = message }
        agentMedia.onConnectionState = { [weak self] connected in
            if connected { self?.lastError = nil }
        }
    }

    func run() async {
        print("DJOneHubAudioHost connected to \(client.baseURL.absoluteString)")
        while !Task.isCancelled {
            await pollOnce()
            try? await Task.sleep(for: .milliseconds(500))
        }
        stopAudio()
        currentDeviceID = nil
        await report(enabled: false)
    }

    private func pollOnce() async {
        do {
            guard let target = try await activeTarget() else {
                if audio.isRunning { stopAudio() }
                currentDeviceID = nil
                lastError = nil
                await report(enabled: true)
                return
            }
            if currentDeviceID != target.deviceID || currentCallID != target.call.id {
                stopAudio()
                currentDeviceID = target.deviceID
                currentCallID = target.call.id
            }
            let config = try await client.audioConfig(deviceID: target.deviceID)
            guard config.callActive, config.callID == target.call.id else {
                stopAudio()
                await report(enabled: true)
                return
            }
            guard config.routeReady else {
                lastError = config.routeError
                await report(enabled: true)
                return
            }
            let wantsAgent = config.mediaMode == "agent"
            let agentProfileChanged = wantsAgent && isAgentMode && currentAgentRevision != config.agentRevision
            if audio.isRunning, wantsAgent != isAgentMode || agentProfileChanged {
                stopAudio()
                currentCallID = target.call.id
            }
            if !audio.isRunning {
                try await startAudio(config: config)
            }
            if config.muted != isMuted {
                isMuted = config.muted
                audio.setMuted(isMuted)
            }
            if config.recording != isRecording {
                if config.recording {
                    recordingPath = try audio.startRecording()
                    isRecording = true
                } else {
                    await stopRecording()
                }
            }
            lastError = nil
            await report(enabled: true)
        } catch {
            lastError = error.localizedDescription
            await report(enabled: true)
        }
    }

    private func activeTarget() async throws -> (deviceID: String?, call: CallRecord)? {
        if let fixedDeviceID {
            knownDeviceIDs = [fixedDeviceID]
            let status = try await client.callStatus(deviceID: fixedDeviceID)
            guard let call = status.active, call.state == "active" else { return nil }
            return (fixedDeviceID, call)
        }
        let devices: [DeviceSummary]
        do {
            devices = try await client.devices().filter { $0.state == "ready" }
            knownDeviceIDs = devices.map(\.id)
        } catch {
            knownDeviceIDs = []
            let status = try await client.callStatus(deviceID: nil)
            guard let call = status.active, call.state == "active" else { return nil }
            return (nil, call)
        }
        for device in devices {
            if let call = try? await client.callStatus(deviceID: device.id).active, call.state == "active" {
                return (device.id, call)
            }
        }
        return nil
    }

    private func startAudio(config: AudioHostConfig) async throws {
        isAgentMode = config.mediaMode == "agent"
        currentAgentRevision = config.agentRevision
        audio.configureAgentMode(isAgentMode)
        if !isAgentMode {
            let granted = await withCheckedContinuation { continuation in
                audio.requestMicrophoneAccess { continuation.resume(returning: $0) }
            }
            guard granted else { throw HostError.message("需要麦克风权限才能进行双向通话") }
        }
        let result = await withCheckedContinuation { continuation in
            audio.startUAC(
                vendorID: config.vendorID,
                productID: config.productID,
                matchingLocationID: config.locationID
            ) { continuation.resume(returning: $0) }
        }
        switch result {
        case .success:
            audio.setMediaEnabled(true)
            if isAgentMode {
                guard let path = config.agentMediaURL, !path.isEmpty else {
                    audio.stop()
                    throw HostError.message("AI 模式缺少本地媒体桥地址")
                }
                agentMedia.start(url: client.absoluteURL(path: path))
            }
            isMuted = config.muted
            audio.setMuted(isMuted)
        case .failure(let message):
            throw HostError.message(message)
        }
    }

    private func stopAudio() {
        agentMedia.stop()
        if isRecording { audio.stopRecording { _ in } }
        audio.stop()
        isRecording = false
        recordingPath = nil
        isMuted = false
        isAgentMode = false
        currentAgentRevision = nil
        audio.configureAgentMode(false)
        currentCallID = nil
    }

    private func stopRecording() async {
        let path = await withCheckedContinuation { continuation in
            audio.stopRecording { continuation.resume(returning: $0) }
        }
        recordingPath = path ?? recordingPath
        isRecording = false
    }

    private func report(enabled: Bool) async {
        let targetDeviceID = currentDeviceID
        let deviceIDs = fixedDeviceID.map { [$0] } ?? knownDeviceIDs
        if deviceIDs.isEmpty {
            await client.register(
                deviceID: targetDeviceID, enabled: enabled, running: audio.isRunning,
                muted: isMuted, recording: isRecording, recordingPath: recordingPath, error: lastError
            )
            return
        }
        for deviceID in deviceIDs {
            let isTarget = targetDeviceID == deviceID
            await client.register(
                deviceID: deviceID,
                enabled: enabled,
                running: isTarget && audio.isRunning,
                muted: isTarget && isMuted,
                recording: isTarget && isRecording,
                recordingPath: isTarget ? recordingPath : nil,
                error: isTarget ? lastError : nil
            )
        }
    }
}
