import Foundation
import RemoteAudio

// A single-use authenticated local bridge. Reconnect needs a fresh backend
// media generation; never reuse AI retry logic or replay buffered PCM.
final class RemoteMediaClient {
    var onAudio: ((Data) -> Void)?
    var onReady: (() -> Void)?
    var onError: ((String) -> Void)?
    private struct Pending { let data: Data; let at: UInt64 }
    private let lock = NSLock()
    private let session: URLSession = {
        let config = URLSessionConfiguration.ephemeral
        config.connectionProxyDictionary = [:]
        return URLSession(configuration: config, delegate: NativeNoRedirect(), delegateQueue: nil)
    }()
    private var task: URLSessionWebSocketTask?
    private var epoch: UInt64 = 0
    private var callUUID = ""
    private var mediaGeneration: UInt64 = 0
    private var framer = RemotePCMFramer(generation: 1)
    private var receiveGuard = RemotePCMGuard(generation: 1)
    private var queue: [Pending] = []
    private var sending = false
    private var writeID: UInt64 = 0
    private var ready = false

    func start(url: URL, token: String, callUUID: String, generation: UInt64) throws {
        stop()
        guard ["127.0.0.1", "::1", "[::1]"].contains(url.host?.lowercased() ?? ""),
              ["http", "https"].contains(url.scheme ?? ""), url.user == nil, url.password == nil,
              url.query == nil, !token.isEmpty, !callUUID.isEmpty, generation > 0 else {
            throw RemotePCMError.invalidFrame
        }
        guard var components = URLComponents(url: url, resolvingAgainstBaseURL: false) else { throw RemotePCMError.invalidFrame }
        components.scheme = url.scheme == "https" ? "wss" : "ws"
        guard let websocketURL = components.url else { throw RemotePCMError.invalidFrame }
        var request = URLRequest(url: websocketURL)
        request.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
        let current = lock.withLock { () -> UInt64 in
            epoch &+= 1; self.callUUID = callUUID; mediaGeneration = generation
            framer = RemotePCMFramer(generation: generation); receiveGuard = RemotePCMGuard(generation: generation)
            let created = session.webSocketTask(with: request); created.maximumMessageSize = 1024
            task = created; created.resume(); return epoch
        }
        receive(current)
        DispatchQueue.global().asyncAfter(deadline: .now() + 3) { [weak self] in
            guard let self, self.lock.withLock({ self.epoch == current && !self.ready }) else { return }
            self.fail(current, "远程本机媒体握手超时")
        }
    }
    func stop() {
        let old = lock.withLock { () -> URLSessionWebSocketTask? in
            epoch &+= 1; let old = task; task = nil
            ready = false; sending = false; queue.removeAll(); return old
        }
        old?.cancel(with: .normalClosure, reason: nil)
    }
    func send(_ pcm: Data, droppedSamples: UInt64 = 0) {
        let current = lock.withLock { epoch }
        do {
            try lock.withLock {
                guard epoch == current, task != nil, ready else { return }
                let now = DispatchTime.now().uptimeNanoseconds
                try framer.skip(samples: droppedSamples)
                for frame in try framer.append(pcm) { queue.append(Pending(data: frame.encoded(), at: now)) }
                if queue.count > 6 { queue.removeFirst(queue.count - 6) }
            }
            drain()
        } catch {
            fail(current, "远程 PCM 数据无效")
        }
    }
    private func drain() {
        let work = lock.withLock { () -> (URLSessionWebSocketTask, Pending, UInt64, UInt64)? in
            guard let task, ready, !sending else { return nil }
            let now = DispatchTime.now().uptimeNanoseconds
            queue.removeAll { now >= $0.at && now - $0.at > 60_000_000 }
            guard !queue.isEmpty else { return nil }
            sending = true; writeID &+= 1; return (task, queue.removeFirst(), epoch, writeID)
        }
        guard let (connection, pending, current, writing) = work else { return }
        connection.send(.data(pending.data)) { [weak self] error in
            guard let self else { return }
            if error != nil { self.fail(current, "远程本机媒体发送失败"); return }
            let valid = self.lock.withLock { () -> Bool in
                guard self.epoch == current else { return false }; self.sending = false; return true
            }
            if valid { self.drain() }
        }
        // Bounded WebSocket write latency includes its internal send queue.
        DispatchQueue.global().asyncAfter(deadline: .now() + .milliseconds(100)) { [weak self] in
            guard let self else { return }
            // Each writer uses an exact frame identity, not just a busy flag.
            if self.lock.withLock({ self.epoch == current && self.sending && self.writeID == writing }) { self.fail(current, "远程本机媒体发送超时") }
        }
    }
    private func receive(_ current: UInt64) {
        guard let connection = lock.withLock({ epoch == current ? task : nil }) else { return }
        connection.receive { [weak self] result in
            guard let self else { return }
            do {
                switch result {
                case .success(.string(let text)):
                    guard let data = text.data(using: .utf8), let event = try JSONSerialization.jsonObject(with: data) as? [String: Any] else { throw RemotePCMError.invalidFrame }
                    let accepted = self.lock.withLock { () -> Bool in
                        guard self.epoch == current, !self.ready,
                              event["type"] as? String == "ready", event["version"] as? Int == 1,
                              event["call_uuid"] as? String == self.callUUID,
                              (event["generation"] as? NSNumber)?.uint64Value == self.mediaGeneration,
                              event["sample_rate"] as? Int == 8000, event["frame_samples"] as? Int == 80 else { return false }
                        self.ready = true; return true
                    }
                    guard accepted else { throw RemotePCMError.invalidFrame }
                    self.deliver(current) { $0.onReady?() }
                case .success(.data(let data)):
                    let frame = try RemotePCMFrame(data: data)
                    try self.lock.withLock {
                        guard self.epoch == current, self.ready else { throw RemotePCMError.staleFrame }
                        try self.receiveGuard.accept(frame)
                        // Queue directly into the thread-safe short UAC buffer;
                        // the main run loop must not become an audio backlog.
                        self.onAudio?(frame.pcm)
                    }
                case .failure: self.fail(current, "远程本机媒体已断开"); return
                @unknown default: throw RemotePCMError.invalidFrame
                }
                self.receive(current)
            } catch { self.fail(current, "远程本机媒体协议错误") }
        }
    }
    private func deliver(_ current: UInt64, _ body: @escaping (RemoteMediaClient) -> Void) {
        DispatchQueue.main.async { [weak self] in
            guard let self, self.lock.withLock({ self.epoch == current && self.task != nil }) else { return }
            body(self)
        }
    }
    private func fail(_ current: UInt64, _ message: String) {
        DispatchQueue.main.async { [weak self] in
            guard let self, self.lock.withLock({ self.epoch == current && self.task != nil }) else { return }
            self.stop(); self.onError?(message)
        }
    }
}
