import Foundation

final class AgentMediaClient {
    var onAudio: ((Data) -> Void)?
    var onEvent: ((String) -> Void)?
    var onError: ((String) -> Void)?
    var onConnectionState: ((Bool) -> Void)?

    private let lock = NSLock()
    private let session = URLSession(configuration: .ephemeral)
    private var task: URLSessionWebSocketTask?
    private var endpointURL: URL?
    private var retryWorkItem: DispatchWorkItem?
    private var reconnectAttempt = 0
    private var generation: UInt64 = 0

    func start(url: URL) {
        stop()
        var components = URLComponents(url: url, resolvingAgainstBaseURL: true)
        components?.scheme = url.scheme == "https" ? "wss" : "ws"
        guard let websocketURL = components?.url else {
            onError?("AI 媒体桥地址无效")
            return
        }
        let current: UInt64 = lock.withLock {
            generation &+= 1
            endpointURL = websocketURL
            reconnectAttempt = 0
            let created = session.webSocketTask(with: websocketURL)
            task = created
            created.resume()
            return generation
        }
        receive(generation: current)
    }

    func send(_ pcm: Data) {
        guard !pcm.isEmpty else { return }
        let current = lock.withLock { (task, generation) }
        guard let task = current.0 else { return }
        task.send(.data(pcm)) { [weak self] error in
            if let error { self?.handleDisconnect(error, generation: current.1, prefix: "AI 媒体发送失败") }
        }
    }

    func sendEvent(type: String) {
        guard let data = try? JSONSerialization.data(withJSONObject: ["type": type]),
              let message = String(data: data, encoding: .utf8) else { return }
        let current = lock.withLock { (task, generation) }
        guard let task = current.0 else { return }
        task.send(.string(message)) { [weak self] error in
            if let error { self?.handleDisconnect(error, generation: current.1, prefix: "AI 媒体事件发送失败") }
        }
    }

    func stop() {
        let previous: URLSessionWebSocketTask? = lock.withLock {
            generation &+= 1
            endpointURL = nil
            retryWorkItem?.cancel()
            retryWorkItem = nil
            reconnectAttempt = 0
            let previous = task
            task = nil
            return previous
        }
        previous?.cancel(with: .normalClosure, reason: nil)
        onConnectionState?(false)
    }

    private func receive(generation expected: UInt64) {
        guard let current = lock.withLock({ generation == expected ? task : nil }) else { return }
        current.receive { [weak self] result in
            guard let self, self.lock.withLock({ self.generation == expected }) else { return }
            switch result {
            case .success(.data(let pcm)):
                self.markConnected(generation: expected)
                self.onAudio?(pcm)
                self.receive(generation: expected)
            case .success(.string(let event)):
                self.markConnected(generation: expected)
                self.onEvent?(event)
                self.receive(generation: expected)
            case .failure(let error):
                self.handleDisconnect(error, generation: expected, prefix: "AI 媒体桥已断开")
            @unknown default:
                self.handleDisconnect(nil, generation: expected, prefix: "AI 媒体桥返回未知消息")
            }
        }
    }

    private func markConnected(generation expected: UInt64) {
        let changed = lock.withLock { () -> Bool in
            guard generation == expected, task != nil else { return false }
            let changed = reconnectAttempt != 0
            reconnectAttempt = 0
            return changed
        }
        if changed { onConnectionState?(true) }
    }

    private func handleDisconnect(_ error: Error?, generation expected: UInt64, prefix: String) {
        let shouldRetry: Bool = lock.withLock {
            guard generation == expected, endpointURL != nil, task != nil else { return false }
            task = nil
            return true
        }
        guard shouldRetry else { return }
        onConnectionState?(false)
        if let error {
            onError?("\(prefix)：\(error.localizedDescription)")
        } else {
            onError?(prefix)
        }
        scheduleReconnect()
    }

    private func scheduleReconnect() {
        let scheduled: (DispatchWorkItem, TimeInterval)? = lock.withLock {
            guard endpointURL != nil, task == nil, retryWorkItem == nil else { return nil }
            reconnectAttempt += 1
            let delay = min(pow(2.0, Double(max(0, reconnectAttempt - 1))), 8.0)
            let work = DispatchWorkItem { [weak self] in self?.reconnectNow() }
            retryWorkItem = work
            return (work, delay)
        }
        guard let scheduled else { return }
        DispatchQueue.global(qos: .utility).asyncAfter(deadline: .now() + scheduled.1, execute: scheduled.0)
    }

    private func reconnectNow() {
        let current: UInt64? = lock.withLock {
            retryWorkItem = nil
            guard let endpointURL, task == nil else { return nil }
            generation &+= 1
            let created = session.webSocketTask(with: endpointURL)
            task = created
            created.resume()
            return generation
        }
        guard let current else { return }
        receive(generation: current)
    }
}
