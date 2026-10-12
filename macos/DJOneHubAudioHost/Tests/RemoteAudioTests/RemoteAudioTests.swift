import Foundation
import XCTest
@testable import RemoteAudio

final class RemoteAudioTests: XCTestCase {
    func testSharedGoFixture() throws {
        var root = URL(fileURLWithPath: #filePath)
        for _ in 0..<5 { root.deleteLastPathComponent() }
        let hex = try String(contentsOf: root.appendingPathComponent("internal/remotemedia/testdata/pcm-v1.hex"), encoding: .utf8).trimmingCharacters(in: .whitespacesAndNewlines)
        let chars = Array(hex)
        let bytes = stride(from: 0, to: chars.count, by: 2).map { UInt8(String(chars[$0...$0+1]), radix: 16)! }
        let frame = try RemotePCMFrame(data: Data(bytes))
        XCTAssertEqual(frame.generation, 7); XCTAssertEqual(frame.sequence, 9); XCTAssertEqual(frame.sampleIndex, 720)
        XCTAssertEqual(frame.encoded(), Data(bytes)); XCTAssertEqual(frame.pcm.prefix(2), Data([216, 255]))
        XCTAssertThrowsError(try RemotePCMFrame(data: Data(bytes.dropLast())))
    }
    func testReframingPreservesCaptureClock() throws {
        var framer = RemotePCMFramer(generation: 7)
        XCTAssertTrue(try framer.append(Data(repeating: 1, count: 100)).isEmpty)
        let frames = try framer.append(Data(repeating: 2, count: 540))
        XCTAssertEqual(frames.count, 4)
        XCTAssertEqual(frames.map(\.sampleIndex), [0, 80, 160, 240])
        XCTAssertEqual(frames[0].pcm.prefix(100), Data(repeating: 1, count: 100))
        XCTAssertThrowsError(try framer.append(Data([1])))
        _ = try framer.append(Data(repeating: 0, count: 40))
        try framer.skip(samples: 1000)
        let next = try framer.append(Data(repeating: 0, count: 160))
        XCTAssertEqual(next.first?.sampleIndex, 1340) // 320 + 20 partial + 1000 skipped
        XCTAssertEqual(next.first?.sequence, 4)
    }
    func testReplayAndGenerationRejection() throws {
        var guardState = RemotePCMGuard(generation: 2)
        let frame = try RemotePCMFrame(direction: 2, generation: 2, sequence: 10, sampleIndex: 800, pcm: Data(repeating: 0, count: 160))
        try guardState.accept(frame)
        XCTAssertThrowsError(try guardState.accept(frame))
        let old = try RemotePCMFrame(direction: 2, generation: 1, sequence: 11, sampleIndex: 880, pcm: frame.pcm)
        XCTAssertThrowsError(try guardState.accept(old))
    }
    func testBoundedQueuePartialAcceptanceExpiryAndClear() {
        var queue = RemotePCMQueue()
        queue.append(Data(repeating: 1, count: 800), now: 0)
        queue.append(Data(repeating: 2, count: 800), now: 0)
        XCTAssertEqual(queue.samples, 480); XCTAssertEqual(queue.droppedSamples, 320)
        XCTAssertEqual(queue.peek(now: 0).count, 160)
        queue.consume(samples: 40)
        XCTAssertEqual(queue.samples, 440)
        XCTAssertTrue(queue.peek(now: 60_000_001).isEmpty)
        XCTAssertEqual(queue.droppedSamples, 760)
        queue.append(Data(repeating: 3, count: 160), now: 70_000_000)
        queue.clear(); XCTAssertTrue(queue.peek(now: 70_000_000).isEmpty)
    }
    func testIndependentUACClockWithPartialWrites() {
        for drift in [-500.0, 500.0] {
            var queue = RemotePCMQueue()
            func pcm(_ count: Int) -> Data { Data((0..<count).flatMap { _ in [UInt8(232), UInt8(3)] }) }
            queue.append(pcm(160), now: 0)
            var generated = 0.0
            for tick in 0..<30_000 { // Five minutes of independent sample clocks.
                let now = UInt64(tick) * 10_000_000
                generated += 80 * (1 + drift / 1_000_000)
                let count = Int(generated); generated -= Double(count)
                queue.append(pcm(count), now: now)
                for _ in 0..<2 {
                    let output = queue.peek(now: now)
                    guard output.count >= 80, output.prefix(80) == pcm(40) else {
                        XCTFail("Partial output starved/corrupted at tick \(tick), drift \(drift), queue \(queue.samples), ppm \(queue.clockCorrectionPPM)"); return
                    }
                    queue.consume(samples: 40)
                }
                XCTAssertLessThanOrEqual(queue.samples, 320)
            }
            XCTAssertEqual(queue.droppedSamples, 0, "Normal drift must not discard whole audio chunks")
            XCTAssertEqual(queue.clockCorrectionPPM, drift, accuracy: 40)
            queue.clear()
            XCTAssertEqual(queue.clockCorrectionPPM, 0)
            XCTAssertEqual(queue.samples, 0)
        }
    }
}
