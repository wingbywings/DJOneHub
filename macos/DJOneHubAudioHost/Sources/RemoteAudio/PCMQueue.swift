import Foundation

// Only short UAC supply buffering, not a network jitter buffer. The caller
// serializes access. Expired audio is removed even when below the size cap.
public struct RemotePCMQueue {
    private struct Chunk { var pcm: Data; let at: UInt64 }
    private var chunks: [Chunk] = []
    public private(set) var droppedSamples: UInt64 = 0
    public var samples: Int { chunks.reduce(0) { $0 + $1.pcm.count / 2 } }
    public init() {}
    public mutating func clear() { chunks.removeAll(keepingCapacity: true) }
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
        var data = Data()
        for chunk in chunks {
            data.append(chunk.pcm.prefix(160 - data.count))
            if data.count == 160 { break }
        }
        return data
    }
    public mutating func consume(samples requested: Int) {
        var remaining = max(0, requested)
        while remaining > 0, !chunks.isEmpty {
            let n = min(remaining, chunks[0].pcm.count / 2)
            chunks[0].pcm.removeFirst(n * 2); remaining -= n
            if chunks[0].pcm.isEmpty { chunks.removeFirst() }
        }
    }
    private mutating func expire(now: UInt64) {
        while let first = chunks.first, now >= first.at, now - first.at > 60_000_000 {
            droppedSamples += UInt64(first.pcm.count / 2); chunks.removeFirst()
        }
    }
}
