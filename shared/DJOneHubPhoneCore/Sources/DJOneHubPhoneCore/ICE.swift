import Darwin
import Foundation

public struct ICEServer: Codable, Sendable {
  public let urls: [String]
  public let username: String
  public let credential: String
}
public struct ICECredentials: Codable, Sendable {
  public let enabled: Bool
  public let servers: [ICEServer]
  public let relay_only: Bool
  public let expires_at: Date?
  public static let direct = ICECredentials(
    enabled: false, servers: [], relay_only: false, expires_at: nil)
  public func validate(now: Date = Date()) throws {
    if !enabled {
      guard servers.isEmpty, !relay_only, expires_at == nil else { throw PhoneError.invalidData }
      return
    }
    guard let expiry = expires_at, expiry > now.addingTimeInterval(60),
      expiry <= now.addingTimeInterval(4 * 3600 + 60), servers.count == 1
    else { throw PhoneError.invalidData }
    let server = servers[0]
    let parts = server.username.split(separator: ":", omittingEmptySubsequences: false)
    guard !server.urls.isEmpty, server.urls.count <= 4, parts.count == 2,
      Int64(parts[0]) == Int64(expiry.timeIntervalSince1970), parts[1].count == 32,
      parts[1].allSatisfy({ $0.isHexDigit }), Data(base64Encoded: server.credential)?.count == 20,
      server.urls.allSatisfy(Self.validURL)
    else { throw PhoneError.invalidData }
  }
  public static func validURL(_ value: String) -> Bool {
    guard value.utf8.count <= 512, let colon = value.firstIndex(of: ":") else { return false }
    let scheme = String(value[..<colon])
    let rest = String(value[value.index(after: colon)...])
    guard scheme == "turn" || scheme == "turns", !rest.hasPrefix("//"),
      let q = rest.firstIndex(of: "?")
    else { return false }
    let address = String(rest[..<q])
    let query = String(rest[rest.index(after: q)...])
    guard query == "transport=udp" || query == "transport=tcp",
      scheme != "turns" || query == "transport=tcp",
      let parsed = URLComponents(string: "https://" + address), parsed.user == nil,
      parsed.password == nil,
      parsed.path.isEmpty, parsed.query == nil, parsed.fragment == nil,
      let host = parsed.host, !host.isEmpty, let port = parsed.port, (1...65535).contains(port)
    else { return false }
    if host.contains(":") {
      var address = in6_addr()
      let literal = host.trimmingCharacters(in: CharacterSet(charactersIn: "[]"))
      return inet_pton(AF_INET6, literal, &address) == 1
    }
    return host.utf8.allSatisfy {
      (48...57).contains($0) || (65...90).contains($0) || (97...122).contains($0)
        || [45, 46, 58, 91, 93].contains($0)
    }
  }
}
