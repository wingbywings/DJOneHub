import Foundation

public enum RemotePCMError: Error { case invalidFrame, staleFrame }

public struct RemotePCMFrame: Equatable {
    public static let sampleCount = 80
    public static let byteCount = 192
    public let direction: UInt8
    public let generation: UInt64
    public let sequence: UInt64
    public let sampleIndex: UInt64
    public let pcm: Data

    public init(direction: UInt8, generation: UInt64, sequence: UInt64, sampleIndex: UInt64, pcm: Data) throws {
        guard (direction == 1 || direction == 2), generation != 0,
              pcm.count == 160, sampleIndex <= UInt64.max - 80 else { throw RemotePCMError.invalidFrame }
        self.direction = direction; self.generation = generation
        self.sequence = sequence; self.sampleIndex = sampleIndex; self.pcm = pcm
    }
    public init(data: Data) throws {
        let bytes = Array(data)
        guard bytes.count == Self.byteCount, Array(bytes[0..<4]) == Array("DJRM".utf8),
              bytes[4] == 1, bytes[6] == 80, bytes[7] == 0 else { throw RemotePCMError.invalidFrame }
        func uint64(_ offset: Int) -> UInt64 {
            (0..<8).reduce(0) { $0 | UInt64(bytes[offset + $1]) << ($1 * 8) }
        }
        try self.init(direction: bytes[5], generation: uint64(8), sequence: uint64(16), sampleIndex: uint64(24), pcm: Data(bytes[32...]))
    }
    public func encoded() -> Data {
        var bytes = Array("DJRM".utf8) + [UInt8(1), direction, 80, 0]
        for value in [generation, sequence, sampleIndex] {
            bytes.append(contentsOf: (0..<8).map { UInt8(truncatingIfNeeded: value >> ($0 * 8)) })
        }
        bytes.append(contentsOf: pcm)
        return Data(bytes)
    }
}

public struct RemotePCMGuard {
    private let generation: UInt64
    private var sequence: UInt64?
    private var end: UInt64 = 0
    public init(generation: UInt64) { self.generation = generation }
    public mutating func accept(_ frame: RemotePCMFrame) throws {
        guard frame.generation == generation, frame.direction == 2 else { throw RemotePCMError.invalidFrame }
        if let sequence, frame.sequence <= sequence || frame.sampleIndex < end { throw RemotePCMError.staleFrame }
        sequence = frame.sequence; end = frame.sampleIndex + 80
    }
}

// State belongs to one connection. Sample indices keep advancing even if the
// downstream bounded send queue drops complete frames.
public struct RemotePCMFramer {
    private let generation: UInt64
    private var pending = Data()
    private var sequence: UInt64 = 0
    private var sampleIndex: UInt64 = 0
    public init(generation: UInt64) { self.generation = generation }
    public mutating func skip(samples: UInt64) throws {
        guard samples > 0 else { return }
        let partial = UInt64(pending.count / 2)
        guard samples <= UInt64.max - partial, sampleIndex <= UInt64.max - samples - partial else { throw RemotePCMError.invalidFrame }
        sampleIndex += samples + partial
        pending.removeAll(keepingCapacity: true)
    }
    public mutating func append(_ pcm: Data) throws -> [RemotePCMFrame] {
        guard pcm.count % 2 == 0, pcm.count <= 8192 else { throw RemotePCMError.invalidFrame }
        pending.append(pcm)
        var result: [RemotePCMFrame] = []
        while pending.count >= 160 {
            guard sequence < UInt64.max else { throw RemotePCMError.invalidFrame }
            result.append(try RemotePCMFrame(direction: 1, generation: generation, sequence: sequence, sampleIndex: sampleIndex, pcm: Data(pending.prefix(160))))
            pending.removeFirst(160); sequence += 1; sampleIndex += 80
        }
        return result
    }
}
