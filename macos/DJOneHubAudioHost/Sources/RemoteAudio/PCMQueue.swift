import Foundation

// Only short UAC supply buffering, not a network jitter buffer. The caller
// serializes access. Expired audio is removed even when below the size cap.
public struct RemotePCMQueue {
    private struct Chunk { var pcm: Data; let at: UInt64 }
    private var chunks: [Chunk] = []
    private var startedAt: UInt64?, updatedAt: UInt64?
    private var filteredError = 0.0, integral = 0.0, phase = 0.0
    private var renderStep = 1.0
    public private(set) var clockCorrectionPPM = 0.0
    public private(set) var droppedSamples: UInt64 = 0
    public var samples: Int { chunks.reduce(0) { $0 + $1.pcm.count / 2 } }
    public init() {}
    public mutating func clear() {
        chunks.removeAll(keepingCapacity: true)
        startedAt = nil; updatedAt = nil
        filteredError = 0; integral = 0; phase = 0; renderStep = 1; clockCorrectionPPM = 0
    }
    public mutating func append(_ pcm: Data, now: UInt64) {
        guard pcm.count % 2 == 0, !pcm.isEmpty else { return }
        expire(now: now)
        let excess = max(0, pcm.count / 2 - 480)
        droppedSamples += UInt64(excess)
        chunks.append(Chunk(pcm: Data(pcm.suffix(960)), at: now))
        var overflow = max(0, samples - 480)
        while overflow > 0 {
            let n = min(overflow, chunks[0].pcm.count / 2)
            chunks[0].pcm.removeFirst(n * 2); droppedSamples += UInt64(n); overflow -= n
            if chunks[0].pcm.isEmpty { chunks.removeFirst() }
        }
    }
    // Supply at most 10 ms per pump pass. Consume only samples UAC accepted.
    public mutating func peek(now: UInt64) -> Data {
        expire(now: now)
        updateClock(now: now)
        renderStep = 1 + clockCorrectionPPM / 1_000_000
        if phase == 0 && renderStep == 1 { return sourcePrefix(bytes: 160) }
        let source = Array(sourcePrefix(bytes: 164))
        let count = source.count / 2
        guard count >= 2, Double(count - 1) > phase else { return Data() }
        let outputs = min(80, Int((Double(count - 1) - phase) / renderStep) + 1)
        var result = Data(capacity: outputs * 2)
        func sample(_ index: Int) -> Double {
            Double(Int16(bitPattern: UInt16(source[index * 2]) | UInt16(source[index * 2 + 1]) << 8))
        }
        for i in 0..<outputs {
            let position = phase + Double(i) * renderStep
            let index = Int(position), fraction = position - Double(Int(position))
            let first = sample(index)
            let value = first + (sample(min(index + 1, count - 1)) - first) * fraction
            let bits = UInt16(bitPattern: Int16(max(-32768, min(32767, value))))
            result.append(UInt8(truncatingIfNeeded: bits)); result.append(UInt8(truncatingIfNeeded: bits >> 8))
        }
        return result
    }
    // Consume only output accepted by UAC. Fractional input phase survives
    // partial writes, so normal clock mismatch never drops a complete frame.
    public mutating func consume(samples requested: Int) {
        guard requested > 0 else { return }
        let advance = phase + Double(requested) * renderStep
        var remaining = Int(advance)
        phase = advance - Double(remaining)
        while remaining > 0, !chunks.isEmpty {
            let n = min(remaining, chunks[0].pcm.count / 2)
            chunks[0].pcm.removeFirst(n * 2); remaining -= n
            if chunks[0].pcm.isEmpty { chunks.removeFirst() }
        }
        if remaining > 0 { phase = 0 }
    }
    private func sourcePrefix(bytes: Int) -> Data {
        var data = Data()
        for chunk in chunks {
            data.append(chunk.pcm.prefix(bytes - data.count))
            if data.count == bytes { break }
        }
        return data
    }
    private mutating func updateClock(now: UInt64) {
        guard let previous = updatedAt, let started = startedAt else {
            startedAt = now; updatedAt = now; return
        }
        guard now > previous else { return }
        updatedAt = now
        let dt = Double(now - previous) / 1_000_000_000
        guard dt <= 0.1 else { return }
        // About 20–25 ms software supply target; the UAC ring stays <=20 ms.
        filteredError += min(1, dt / 2) * (Double(samples) - 200 - filteredError)
        guard now - started >= 3_000_000_000 else { return }
        integral = max(-1000, min(1000, integral + filteredError * 0.1 * dt))
        let target = max(-1000, min(1000, 12 * filteredError + integral))
        clockCorrectionPPM += max(-100 * dt, min(100 * dt, target - clockCorrectionPPM))
    }

    private mutating func expire(now: UInt64) {
        while let first = chunks.first, now >= first.at, now - first.at > 60_000_000 {
            droppedSamples += UInt64(first.pcm.count / 2); chunks.removeFirst()
        }
    }
}
