import CryptoKit
import DJOneHubIdentity
import Foundation

public enum PhoneError: Error {
  case invalidData, unavailable, recoveryRequired, alreadyPaired, busy, denied
}
public enum Wire {
  public static func id(_ value: String) -> Bool {
    !value.isEmpty && value.utf8.count <= 128
      && value.utf8.allSatisfy {
        (48...57).contains($0) || (65...90).contains($0) || (97...122).contains($0) || $0 == 45
          || $0 == 95
      }
  }
  public static func token(_ value: String) -> Bool { value.utf8.count == 43 && id(value) }
  public static func date(_ value: String) -> Date? {
    let f = ISO8601DateFormatter()
    f.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
    if let d = f.date(from: value) { return d }
    f.formatOptions = [.withInternetDateTime]
    return f.date(from: value)
  }
  public static func decoder() -> JSONDecoder {
    let d = JSONDecoder()
    d.dateDecodingStrategy = .custom { decoder in
      let value = try decoder.singleValueContainer().decode(String.self)
      guard let date = date(value) else { throw PhoneError.invalidData }
      return date
    }
    return d
  }
  public static func encoder() -> JSONEncoder {
    let e = JSONEncoder()
    e.dateEncodingStrategy = .iso8601
    return e
  }
  public static func origin(_ value: String) throws -> URL {
    guard let c = URLComponents(string: value), c.scheme == "https", let host = c.host,
      !host.isEmpty,
      c.user == nil, c.password == nil, c.query == nil, c.fragment == nil,
      c.path.isEmpty || c.path == "/", let url = c.url
    else { throw PhoneError.invalidData }
    return url
  }
}
public struct Grant: Codable, Sendable {
  public let id: String
  public let secret: String
  public let expires_at: Date
  public let modules: [String]
}
public struct PairingFile: Codable, Sendable {
  public let version: Int
  public let relay_url: String
  public let grant: Grant
  public static func parse(_ data: Data, now: Date = Date()) throws -> Self {
    guard data.count <= 8192 else { throw PhoneError.invalidData }
    let value = try Wire.decoder().decode(Self.self, from: data)
    _ = try Wire.origin(value.relay_url)
    guard value.version == 1, Wire.id(value.grant.id), Wire.token(value.grant.secret),
      value.grant.expires_at > now, !value.grant.modules.isEmpty, value.grant.modules.count <= 32,
      Set(value.grant.modules).count == value.grant.modules.count,
      value.grant.modules.allSatisfy(Wire.id), value.grant.expires_at <= now.addingTimeInterval(600)
    else { throw PhoneError.invalidData }
    return value
  }
}
public struct Principal: Codable, Equatable, Sendable {
  public let id: String
  public let tenant: String
  public let kind: String
  public let hub_id: String
  public let modules: [String]
  public let version: UInt64
  public let access_expires_at: Date
  public func sameIdentity(_ other: Self) -> Bool {
    id == other.id && tenant == other.tenant && kind == other.kind && hub_id == other.hub_id
      && modules == other.modules && version == other.version
  }
}
public struct Tokens: Codable, Sendable {
  public let access_token: String
  public let refresh_token: String
  public let refresh_expires_at: Date
  public let principal: Principal
  public func validate(now: Date = Date(), allowExpiredAccess: Bool = false) throws {
    let p = principal
    guard Wire.token(access_token), Wire.token(refresh_token), refresh_expires_at > now,
      allowExpiredAccess || p.access_expires_at > now,
      Wire.id(p.id), Wire.id(p.tenant), Wire.id(p.hub_id), p.kind == "installation", p.version > 0,
      !p.modules.isEmpty, p.modules.count <= 32, Set(p.modules).count == p.modules.count,
      p.modules.allSatisfy(Wire.id)
    else { throw PhoneError.invalidData }
  }
}
public struct StoredIdentity: Codable, Sendable {
  public var version = 1
  public var relay: String
  public var seed: Data
  public var phase: String
  public var tokens: Tokens?
}
@MainActor public protocol IdentityVault {
  func load() throws -> Data?
  func create(_ data: Data) throws
  func replace(_ data: Data) throws
}
@MainActor public protocol IdentityHTTP {
  func post(origin: URL, path: String, body: Data, accessToken: String?) async throws -> Data
}

extension IdentityHTTP {
  public func post(origin: URL, path: String, body: Data) async throws -> Data {
    try await post(origin: origin, path: path, body: body, accessToken: nil)
  }
}

// Single installation per app. A durable pending marker precedes any potentially
// consuming POST. Neither retries nor a restart may silently reuse spent proofs.
@MainActor public final class PhoneIdentity {
  private let vault: IdentityVault
  private let http: IdentityHTTP
  private var busy = false
  public init(vault: IdentityVault, http: IdentityHTTP) {
    self.vault = vault
    self.http = http
  }
  public func status() throws -> StoredIdentity? {
    guard let data = try vault.load() else { return nil }
    guard data.count <= 32768 else { throw PhoneError.invalidData }
    let stored = try Wire.decoder().decode(StoredIdentity.self, from: data)
    guard stored.version == 1, stored.seed.count == 32,
      ["pairing_pending", "ready", "refresh_pending"].contains(stored.phase)
    else { throw PhoneError.invalidData }
    _ = try Wire.origin(stored.relay)
    return stored
  }
  public func fingerprint() throws -> String? {
    guard let record = try status() else { return nil }
    let key = try IdentityKey(rawRepresentation: record.seed)
    return SHA256.hash(data: key.publicKey).map { String(format: "%02x", $0) }.joined()
  }
  public func pair(_ file: PairingFile) async throws {
    guard !busy else { throw PhoneError.busy }
    busy = true
    defer { busy = false }
    guard try status() == nil else { throw PhoneError.alreadyPaired }
    let origin = try Wire.origin(file.relay_url)
    guard file.version == 1, file.grant.expires_at > Date() else { throw PhoneError.invalidData }
    let key = IdentityKey()
    let proof = try key.grantProof(kind: .pairing, id: file.grant.id, secret: file.grant.secret)
    var record = StoredIdentity(
      relay: origin.absoluteString, seed: key.rawRepresentation, phase: "pairing_pending")
    try vault.create(Wire.encoder().encode(record))
    let data = try await http.post(
      origin: origin, path: "/v1/pairings/redeem", body: Wire.encoder().encode(proof))
    let tokens = try Wire.decoder().decode(Tokens.self, from: data)
    try tokens.validate()
    guard tokens.principal.modules == file.grant.modules else { throw PhoneError.invalidData }
    record.tokens = tokens
    record.phase = "ready"
    try vault.replace(Wire.encoder().encode(record))
  }
  public func credentials(forceRefresh: Bool = false) async throws -> StoredIdentity {
    guard !busy else { throw PhoneError.busy }
    busy = true
    defer { busy = false }
    guard var r = try status(), r.phase == "ready", let old = r.tokens else {
      throw PhoneError.recoveryRequired
    }
    try old.validate(allowExpiredAccess: true)
    if !forceRefresh && old.principal.access_expires_at > Date().addingTimeInterval(60) { return r }
    struct Request: Encodable {
      let identity_id: String
      let refresh_token: String
    }
    struct Challenge: Decodable {
      let id: String
      let value: String
      let expires_at: Date
    }
    let origin = try Wire.origin(r.relay)
    let body = Request(identity_id: old.principal.id, refresh_token: old.refresh_token)
    let data = try await http.post(
      origin: origin, path: "/v1/auth/challenge", body: Wire.encoder().encode(body))
    let challenge = try Wire.decoder().decode(Challenge.self, from: data)
    guard challenge.expires_at > Date(), challenge.expires_at < Date().addingTimeInterval(120)
    else { throw PhoneError.invalidData }
    let proof = try IdentityKey(rawRepresentation: r.seed).refreshProof(
      identityID: old.principal.id, refreshToken: old.refresh_token, challengeID: challenge.id,
      challenge: challenge.value)
    r.phase = "refresh_pending"
    try vault.replace(Wire.encoder().encode(r))
    let response = try await http.post(
      origin: origin, path: "/v1/auth/refresh", body: Wire.encoder().encode(proof))
    let next = try Wire.decoder().decode(Tokens.self, from: response)
    try next.validate()
    guard old.principal.sameIdentity(next.principal), next.refresh_token != old.refresh_token else {
      throw PhoneError.invalidData
    }
    r.tokens = next
    r.phase = "ready"
    try vault.replace(Wire.encoder().encode(r))
    return r
  }
  public func ice(module: String) async throws -> ICECredentials {
    let record = try await credentials()
    guard let tokens = record.tokens, tokens.principal.modules.contains(module) else {
      throw PhoneError.denied
    }
    let body = try Wire.encoder().encode(["module_id": module])
    let data = try await http.post(
      origin: Wire.origin(record.relay), path: "/v1/ice", body: body,
      accessToken: tokens.access_token)
    let config = try Wire.decoder().decode(ICECredentials.self, from: data)
    try config.validate()
    return config
  }

  public func registerPush(environment: String, token: String) async throws {
    guard ["sandbox", "production"].contains(environment),
      token.isEmpty
        || (token.count >= 32 && token.count <= 1024 && token.count % 2 == 0
          && token.utf8.allSatisfy({ (48...57).contains($0) || (97...102).contains($0) }))
    else { throw PhoneError.invalidData }
    let record = try await credentials()
    guard let tokens = record.tokens else { throw PhoneError.denied }
    let body = try Wire.encoder().encode(["environment": environment, "token": token])
    let data = try await http.post(
      origin: Wire.origin(record.relay), path: "/v1/push", body: body,
      accessToken: tokens.access_token)
    struct Result: Decodable { let registered: Bool }
    guard try Wire.decoder().decode(Result.self, from: data).registered == !token.isEmpty else {
      throw PhoneError.invalidData
    }
  }

}
