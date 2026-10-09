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
