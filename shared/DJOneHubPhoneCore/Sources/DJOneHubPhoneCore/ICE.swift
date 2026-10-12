import Darwin
import Foundation

// The same peer's SDP origin must advance, and both ICE credentials must
// change. A new wire negotiation ID cannot make an old SDP safe to replay.
public enum ICERevision {
  public static func isRestart(previous: String, next: String) -> Bool {
    func fields(_ sdp: String) -> (String, UInt64, String, String, Int)? {
      guard sdp.utf8.count <= 65536 else { return nil }
      let lines = sdp.components(separatedBy: .newlines).filter { !$0.isEmpty }
      let origins = lines.filter { $0.hasPrefix("o=") }
      let media = lines.filter { $0.hasPrefix("m=") }
      guard origins.count == 1, (1...2).contains(media.count), media[0].hasPrefix("m=audio "),
        media.count == 1 || media[1].hasPrefix("m=application ") else { return nil }
      let origin = origins[0].dropFirst(2).split(separator: " ")
      guard origin.count == 6, let version = UInt64(origin[2]) else { return nil }
      let audioLines = Array(lines.prefix(while: { !$0.hasPrefix("m=application ") }))
      guard let u = audioLines.last(where: { $0.hasPrefix("a=ice-ufrag:") }),
            let p = audioLines.last(where: { $0.hasPrefix("a=ice-pwd:") }) else { return nil }
      let username = String(u.dropFirst(12)), password = String(p.dropFirst(10))
      guard !username.isEmpty, !password.isEmpty else { return nil }
      return (String(origin[1]), version, username, password, media.count)
    }
    guard let old = fields(previous), let new = fields(next) else { return false }
    return old.4 == new.4 && old.0 == new.0 && new.1 > old.1 && new.2 != old.2 && new.3 != old.3
  }
}

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
