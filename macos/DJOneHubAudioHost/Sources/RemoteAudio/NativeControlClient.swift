import Foundation
import Darwin

public enum NativeControlError: Error { case unsafeBootstrap, invalidResponse, invalidMediaPath }

public struct NativeControlCredentials: Decodable, Sendable {
    public let version: Int
    public let baseURL: URL
    public let token: String
    enum CodingKeys: String, CodingKey { case version, baseURL = "base_url", token }

    public static func load(path: String) throws -> Self {
        guard path.hasPrefix("/") else { throw NativeControlError.unsafeBootstrap }
        let fd = open(path, O_RDONLY | O_NOFOLLOW | O_NONBLOCK | O_CLOEXEC)
        guard fd >= 0 else { throw NativeControlError.unsafeBootstrap }
        let handle = FileHandle(fileDescriptor: fd, closeOnDealloc: true)
        defer { try? handle.close() }
        var info = stat()
        guard fstat(fd, &info) == 0, info.st_mode & S_IFMT == S_IFREG,
              info.st_uid == getuid(), info.st_mode & 0o077 == 0,
              info.st_size > 0, info.st_size <= 4096 else { throw NativeControlError.unsafeBootstrap }
        let data = try handle.read(upToCount: 4097) ?? Data()
        guard data.count <= 4096 else { throw NativeControlError.unsafeBootstrap }
        let credentials = try JSONDecoder().decode(Self.self, from: data)
        guard credentials.version == 1, credentials.baseURL.scheme == "http",
              credentials.baseURL.host == "127.0.0.1", (credentials.baseURL.port ?? 0) > 0,
              credentials.baseURL.user == nil, credentials.baseURL.password == nil,
              credentials.baseURL.query == nil, credentials.baseURL.fragment == nil,
              ["", "/"].contains(credentials.baseURL.path), credentials.token.count == 64,
              credentials.token.utf8.allSatisfy({ (48...57).contains($0) || (97...102).contains($0) }) else {
            throw NativeControlError.unsafeBootstrap
        }
        return credentials
    }
}

// Credentials must never follow a redirect to another service or origin.
public final class NativeNoRedirect: NSObject, URLSessionTaskDelegate, @unchecked Sendable {
    public func urlSession(_ session: URLSession, task: URLSessionTask,
                           willPerformHTTPRedirection response: HTTPURLResponse,
                           newRequest request: URLRequest,
                           completionHandler: @escaping (URLRequest?) -> Void) { completionHandler(nil) }
}

public final class NativeControlClient: @unchecked Sendable {
    private let path: String
    private let hostID = UUID().uuidString
    private let session: URLSession
    public init(path: String) {
        self.path = path
        let config = URLSessionConfiguration.ephemeral
        config.timeoutIntervalForRequest = 1
        config.timeoutIntervalForResource = 2
        config.connectionProxyDictionary = [:]
        session = URLSession(configuration: config, delegate: NativeNoRedirect(), delegateQueue: nil)
    }
    public func configuration(deviceID: String?) async throws -> Data {
        try await request(endpoint: "/config", deviceID: deviceID, body: nil)
    }
    public func report(deviceID: String?, callUUID: String, generation: UInt64, sequence: UInt64, running: Bool) async throws {
        let data = try JSONSerialization.data(withJSONObject: ["call_uuid": callUUID, "generation": generation, "sequence": sequence, "running": running])
        _ = try await request(endpoint: "/status", deviceID: deviceID, body: data)
    }
    public func mediaURL(path mediaPath: String) throws -> URL {
        guard mediaPath.hasPrefix("/media/"), !mediaPath.contains("?"), !mediaPath.contains("#"),
              let url = URL(string: mediaPath, relativeTo: try NativeControlCredentials.load(path: path).baseURL)?.absoluteURL,
              url.host == "127.0.0.1" else { throw NativeControlError.invalidMediaPath }
        return url
    }
    private func request(endpoint: String, deviceID: String?, body: Data?) async throws -> Data {
        // Reread after every restart; never retain the old bootstrap token.
        let credentials = try NativeControlCredentials.load(path: path)
        var components = URLComponents(url: credentials.baseURL, resolvingAgainstBaseURL: false)!
        components.path = endpoint
        components.queryItems = [URLQueryItem(name: "device_id", value: deviceID ?? "")]
        var request = URLRequest(url: components.url!)
        request.setValue("Bearer \(credentials.token)", forHTTPHeaderField: "Authorization")
        request.setValue(hostID, forHTTPHeaderField: "X-DJOneHub-Host-ID")
        request.httpMethod = body == nil ? "GET" : "POST"
        request.httpBody = body
        if body != nil { request.setValue("application/json", forHTTPHeaderField: "Content-Type") }
        let (data, response) = try await session.data(for: request)
        guard let http = response as? HTTPURLResponse, http.statusCode == 200, data.count <= 65536 else { throw NativeControlError.invalidResponse }
        return data
    }
}
