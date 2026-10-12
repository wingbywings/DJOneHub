import XCTest
import Foundation
@testable import RemoteAudio

final class NativeControlTests: XCTestCase {
    private func directory() throws -> URL {
        let path = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: path, withIntermediateDirectories: false, attributes: [.posixPermissions: 0o700])
        addTeardownBlock { try FileManager.default.removeItem(at: path) }
        return path
    }
    private func bootstrap(_ url: URL, base: String = "http://127.0.0.1:12345", token: String = String(repeating: "a", count: 64), mode: Int = 0o600) throws {
        let data = try JSONSerialization.data(withJSONObject: ["version": 1, "base_url": base, "token": token])
        try data.write(to: url)
        try FileManager.default.setAttributes([.posixPermissions: mode], ofItemAtPath: url.path)
    }
    func testPrivateBootstrapAndRotation() throws {
        let file = try directory().appendingPathComponent("control.json")
        try bootstrap(file)
        let first = try NativeControlCredentials.load(path: file.path)
        XCTAssertEqual(first.baseURL.port, 12345)
        try bootstrap(file, base: "http://127.0.0.1:12346", token: String(repeating: "b", count: 64))
        let second = try NativeControlCredentials.load(path: file.path)
        XCTAssertNotEqual(first.token, second.token)
        XCTAssertEqual(second.baseURL.port, 12346)
    }
    func testPublicFileAndSymlinkRejected() throws {
        let dir = try directory(), file = dir.appendingPathComponent("control.json")
        try bootstrap(file, mode: 0o644)
        XCTAssertThrowsError(try NativeControlCredentials.load(path: file.path))
        try bootstrap(file)
        let link = dir.appendingPathComponent("link.json")
        try FileManager.default.createSymbolicLink(at: link, withDestinationURL: file)
        XCTAssertThrowsError(try NativeControlCredentials.load(path: link.path))
        XCTAssertThrowsError(try NativeControlCredentials.load(path: "relative.json"))
    }
    func testNonLoopbackAndCredentialURLsRejected() throws {
        let file = try directory().appendingPathComponent("control.json")
        for base in ["http://localhost:12345", "http://192.0.2.1:12345", "https://example.com", "http://user:password@127.0.0.1:12345", "http://127.0.0.1:12345/config", "http://127.0.0.1:12345/?token=x"] {
            try bootstrap(file, base: base)
            XCTAssertThrowsError(try NativeControlCredentials.load(path: file.path), base)
        }
        try bootstrap(file, token: "short")
        XCTAssertThrowsError(try NativeControlCredentials.load(path: file.path))
    }
    func testMediaPathCannotEscapeNativeOrigin() throws {
        let file = try directory().appendingPathComponent("control.json")
        try bootstrap(file)
        let client = NativeControlClient(path: file.path)
        XCTAssertEqual(try client.mediaURL(path: "/media/device/call").absoluteString, "http://127.0.0.1:12345/media/device/call")
        for path in ["https://example.com/media", "//example.com/media", "/config", "/media/device/call?token=secret"] {
            XCTAssertThrowsError(try client.mediaURL(path: path))
        }
    }
}

extension NativeControlTests {
    func testEventShapeAndRevisionBounds() throws {
        XCTAssertEqual(try NativeChangeEvent.decode(Data(#"{"type":"changed","revision":0}"#.utf8)), 0)
        XCTAssertEqual(try NativeChangeEvent.decode(Data(#"{"type":"changed","revision":18446744073709551615}"#.utf8)), UInt64.max)
        for text in [#"{"type":"command","revision":1}"#, #"{"type":"changed","revision":true}"#,
                     #"{"type":"changed","revision":-1}"#, #"{"type":"changed","revision":1,"token":"secret"}"#,
                     #"{"type":"changed"}"#, String(repeating: "x", count: 1025)] {
            XCTAssertThrowsError(try NativeChangeEvent.decode(Data(text.utf8)))
        }
    }
    func testRefreshPolicyKeepsLegacyLeaseAndTransitionBudget() {
        XCTAssertEqual(NativeRefreshPolicy.interval(eventsOnline: true, remoteRunning: true, transitioning: false), 5)
        XCTAssertLessThan(NativeRefreshPolicy.interval(eventsOnline: true, remoteRunning: false, transitioning: false), 5)
        XCTAssertEqual(NativeRefreshPolicy.interval(eventsOnline: false, remoteRunning: true, transitioning: false), 0.5)
        XCTAssertEqual(NativeRefreshPolicy.interval(eventsOnline: true, remoteRunning: false, transitioning: true), 0.5)
    }
}

extension NativeControlTests {
    func testLiveNativeEventsAndCancellation() async throws {
        guard let path = ProcessInfo.processInfo.environment["DJONEHUB_TEST_NATIVE_BOOTSTRAP"] else {
            throw XCTSkip("requires the Go native event fixture")
        }
        let client = NativeControlClient(path: path)
        let (stream, continuation) = AsyncStream<UInt64>.makeStream(bufferingPolicy: .bufferingNewest(8))
        let subscription = Task {
            defer { continuation.finish() }
            try await client.events { revision in continuation.yield(revision) }
        }
        defer { subscription.cancel(); continuation.finish() }
        try await withThrowingTaskGroup(of: Void.self) { group in
            group.addTask {
                var previous: UInt64?
                var received = 0
                for await revision in stream {
                    if let previous { XCTAssertGreaterThan(revision, previous) }
                    previous = revision
                    received += 1
                    if received == 3 { return }
                }
                throw NativeControlError.invalidResponse
            }
            group.addTask { try await Task.sleep(for: .seconds(5)); throw NativeControlError.invalidResponse }
            _ = try await group.next()
            group.cancelAll()
        }
        subscription.cancel()
        // Await cancellation: the receive operation must not leave a live socket.
        _ = await subscription.result
    }
}
