import CallKit
import DJOneHubPhoneCore
import Foundation
import Security

func incomingCallUpdate(display: String?, moduleName: String? = nil) -> CXCallUpdate {
  let caller = IncomingCaller(display: display)
  let update = CXCallUpdate()
  update.remoteHandle = CXHandle(
    type: caller.phoneNumber == nil ? .generic : .phoneNumber,
    value: caller.phoneNumber ?? caller.display)
  update.localizedCallerName = caller.localizedName(moduleName: moduleName)
  update.hasVideo = false
  update.supportsHolding = false
  update.supportsGrouping = false
  update.supportsUngrouping = false
  update.supportsDTMF = false
  return update
}

@MainActor final class KeychainVault: IdentityVault {
  private var query: [String: Any] {
    [
      kSecClass as String: kSecClassGenericPassword,
      kSecAttrService as String: "org.djonehub.phone.identity.v1",
      kSecAttrAccount as String: "installation",
      kSecAttrSynchronizable as String: false,
    ]
  }
  func load() throws -> Data? {
    var q = query
    q[kSecReturnData as String] = true
    q[kSecMatchLimit as String] = kSecMatchLimitOne
    var result: CFTypeRef?
    let status = SecItemCopyMatching(q as CFDictionary, &result)
    if status == errSecItemNotFound { return nil }
    guard status == errSecSuccess, let data = result as? Data, data.count <= 32768 else {
      throw PhoneError.unavailable
    }
    return data
  }
  func create(_ data: Data) throws {
    var q = query
    q[kSecValueData as String] = data
    q[kSecAttrAccessible as String] = kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly
    guard SecItemAdd(q as CFDictionary, nil) == errSecSuccess else { throw PhoneError.unavailable }
  }
  func remove() throws {
    let status = SecItemDelete(query as CFDictionary)
    guard status == errSecSuccess || status == errSecItemNotFound else {
      throw PhoneError.unavailable
    }
  }
  func replace(_ data: Data) throws {
    guard
      SecItemUpdate(query as CFDictionary, [kSecValueData as String: data] as CFDictionary)
        == errSecSuccess
    else { throw PhoneError.unavailable }
  }
}

final class NetworkDelegate: NSObject, URLSessionTaskDelegate, URLSessionWebSocketDelegate,
  @unchecked Sendable
{
  private let lock = NSLock()
  private var accepted = false
  var protocolAccepted: Bool {
    lock.lock()
    defer { lock.unlock() }
    return accepted
  }
  func urlSession(
    _ session: URLSession, task: URLSessionTask,
    willPerformHTTPRedirection response: HTTPURLResponse, newRequest request: URLRequest,
    completionHandler: @escaping (URLRequest?) -> Void
  ) { completionHandler(nil) }
  func urlSession(
    _ session: URLSession, webSocketTask: URLSessionWebSocketTask,
    didOpenWithProtocol value: String?
  ) {
    lock.lock()
    accepted = value == "djonehub.calls.v1"
    lock.unlock()
    if !protocolAccepted { webSocketTask.cancel(with: .protocolError, reason: nil) }
  }
  func urlSession(
    _ session: URLSession, webSocketTask: URLSessionWebSocketTask,
    didCloseWith closeCode: URLSessionWebSocketTask.CloseCode, reason: Data?
  ) {}
  static func configuration() -> URLSessionConfiguration {
    let c = URLSessionConfiguration.ephemeral
    c.httpCookieStorage = nil
    c.urlCache = nil
    c.httpShouldSetCookies = false
    c.connectionProxyDictionary = [:]
    c.timeoutIntervalForRequest = 10
    c.timeoutIntervalForResource = 15
    c.waitsForConnectivity = false
    return c
  }
}
@MainActor final class RelayHTTP: IdentityHTTP {
  func supportsDialing(origin: URL) async -> Bool {
    let delegate = NetworkDelegate()
    let session = URLSession(
      configuration: NetworkDelegate.configuration(), delegate: delegate, delegateQueue: nil)
    defer { session.invalidateAndCancel() }
    do {
      let (bytes, response) = try await session.bytes(
        from: origin.appendingPathComponent("healthz"))
      guard let response = response as? HTTPURLResponse, response.statusCode == 200 else {
        return false
      }
      var data = Data()
      for try await byte in bytes {
        guard data.count < 8192 else { return false }
        data.append(byte)
      }
      guard let value = try JSONSerialization.jsonObject(with: data) as? [String: Any],
        value["status"] as? String == "ok", value["version"] as? Int == 1,
        let capabilities = value["capabilities"] as? [String: Bool]
      else { return false }
      return capabilities["outgoing_calls"] == true
    } catch { return false }
  }
  func post(origin: URL, path: String, body: Data, accessToken: String?) async throws -> Data {
    let delegate = NetworkDelegate()
    let session = URLSession(
      configuration: NetworkDelegate.configuration(), delegate: delegate, delegateQueue: nil)
    defer { session.invalidateAndCancel() }
    var request = URLRequest(url: origin.appendingPathComponent(path))
    request.httpMethod = "POST"
    request.httpBody = body
    if let accessToken {
      request.setValue("Bearer \(accessToken)", forHTTPHeaderField: "Authorization")
    }
    request.setValue("application/json", forHTTPHeaderField: "Content-Type")
    request.setValue("no-store", forHTTPHeaderField: "Cache-Control")
    let (bytes, response) = try await session.bytes(for: request)
    guard let r = response as? HTTPURLResponse else { throw PhoneError.invalidData }
    guard (200..<300).contains(r.statusCode) else {
      throw RelayHTTPFailure(statusCode: r.statusCode)
    }
    var data = Data()
    for try await byte in bytes {
      guard data.count < 32768 else { throw PhoneError.invalidData }
      data.append(byte)
    }
    return data
  }
}

struct RelayHTTPFailure: Error { let statusCode: Int }

// Preserve useful native failure codes without logging userInfo, URLs,
// token-bearing requests, SDP, ICE credentials or raw response bodies.
func phoneFailureCode(_ error: Error) -> String {
  if let e = error as? RelayHTTPFailure { return "http_\(e.statusCode)" }
  if let e = error as? PhoneError {
    switch e {
    case .invalidData: return "invalid_data"
    case .unavailable: return "unavailable"
    case .recoveryRequired: return "identity_recovery_required"
    case .alreadyPaired: return "already_paired"
    case .busy: return "identity_busy"
    case .denied: return "scope_denied"
    }
  }
  let e = error as NSError
  if e.domain == CXErrorDomain { return "callkit_general_\(e.code)" }
  if e.domain == CXErrorDomainIncomingCall { return "callkit_incoming_\(e.code)" }
  if e.domain == NSURLErrorDomain { return "url_\(e.code)" }
  if e.domain == NSOSStatusErrorDomain { return "audio_\(e.code)" }
  return "native_\(e.code)"
}

// URLSession may deliver a ping completion again when a completed socket is
// cancelled. Take the continuation under a lock before resuming outside it.
// Each ping owns a separate gate, so a late callback cannot finish a new ping.
final class PingCompletion: @unchecked Sendable {
  private let lock = NSLock()
  private var continuation: CheckedContinuation<Void, Error>?
  init(_ continuation: CheckedContinuation<Void, Error>) { self.continuation = continuation }
  func finish(_ error: Error?) {
    lock.lock()
    let pending = continuation
    continuation = nil
    lock.unlock()
    guard let pending else { return }
    if let error { pending.resume(throwing: error) } else { pending.resume() }
  }
}

@MainActor final class SignalSocket {
  let task: URLSessionWebSocketTask
  private let session: URLSession
  private let delegate: NetworkDelegate
  private var queue: [Signal] = []
  private var sending = false
  private(set) var pendingWriteSince: Date?
  private var closed = false
  var onFailure: (() -> Void)?
  init(identity: StoredIdentity, resume: DJOneHubPhoneCore.Binding? = nil) throws {
    guard let tokens = identity.tokens else { throw PhoneError.invalidData }
    var c = URLComponents(url: try Wire.origin(identity.relay), resolvingAgainstBaseURL: false)!
    c.scheme = "wss"
    c.path = "/v1/signaling"
    var r = URLRequest(url: c.url!)
    r.setValue("Bearer \(tokens.access_token)", forHTTPHeaderField: "Authorization")
    r.setValue("djonehub.calls.v1", forHTTPHeaderField: "Sec-WebSocket-Protocol")
    r.setValue("1", forHTTPHeaderField: "X-DJOneHub-Recovery")
    if let resume {
      let message = Signal(type: "resume", binding: resume, payload: [:])
      let encoded = try message.encoded().base64EncodedString()
        .replacingOccurrences(of: "+", with: "-").replacingOccurrences(of: "/", with: "_")
        .replacingOccurrences(of: "=", with: "")
      guard encoded.utf8.count <= 2048 else { throw PhoneError.invalidData }
      r.setValue(encoded, forHTTPHeaderField: "X-DJOneHub-Resume")
    }
    delegate = NetworkDelegate()
    let configuration = NetworkDelegate.configuration()
    configuration.timeoutIntervalForResource = 0
    configuration.timeoutIntervalForRequest = 30
    session = URLSession(configuration: configuration, delegate: delegate, delegateQueue: nil)
    task = session.webSocketTask(with: r)
    task.maximumMessageSize = 131072
    task.resume()
  }
  func receive() async throws -> Signal {
    let message = try await task.receive()
    guard delegate.protocolAccepted, case .string(let text) = message else {
      throw PhoneError.invalidData
    }
    return try Signal.decode(Data(text.utf8))
  }
  func send(_ message: Signal) {
    guard !closed else { return }
    guard queue.count < 8 else {
      onFailure?()
      close()
      return
    }
    queue.append(message)
    guard !sending else { return }
    sending = true
    Task { [weak self] in
      guard let self else { return }
      defer { self.sending = false }
      do {
        while !self.queue.isEmpty && !self.closed {
          let next = self.queue.removeFirst()
          guard let text = String(data: try next.encoded(), encoding: .utf8) else {
            throw PhoneError.invalidData
          }
          self.pendingWriteSince = Date()
          try await self.task.send(.string(text))
          self.pendingWriteSince = nil
        }
      } catch {
        if !self.closed {
          self.onFailure?()
          self.close()
        }
      }
    }
  }
  func ping() async throws {
    try await withCheckedThrowingContinuation { (c: CheckedContinuation<Void, Error>) in
      let completion = PingCompletion(c)
      task.sendPing { error in completion.finish(error) }
    }
  }
  func close() {
    closed = true
    queue.removeAll()
    task.cancel(with: .goingAway, reason: nil)
    session.invalidateAndCancel()
  }
}
