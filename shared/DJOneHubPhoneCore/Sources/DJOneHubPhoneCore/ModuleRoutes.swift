import Foundation

/// Per-line Hub answer route (`agent` / `remote` / `local`), mirrored from
/// `callcontrol.SelectRoute`. Missing / unavailable snapshots stay unknown so
/// the App never invents a global “AI 接听中”.
public enum LineAnswerRoute: String, Codable, Sendable, Equatable {
  case agent, remote, local
}

public struct ModuleRoutes: Codable, Sendable {
  public struct Entry: Codable, Sendable {
    public let id: String
    public let effective_route: String
    public let known: Bool
    public let target_installation_id: String?
    public let ai_enabled: Bool?
    public let remote_enabled: Bool?

    public init(
      id: String, effective_route: String = "", known: Bool = false,
      target_installation_id: String? = nil, ai_enabled: Bool? = nil, remote_enabled: Bool? = nil
    ) {
      self.id = id
      self.effective_route = effective_route
      self.known = known
      self.target_installation_id = target_installation_id
      self.ai_enabled = ai_enabled
      self.remote_enabled = remote_enabled
    }

    public var route: LineAnswerRoute? {
      guard known else { return nil }
      return LineAnswerRoute(rawValue: effective_route)
    }
  }

  public let hub_id: String
  public let available: Bool
  public let modules: [Entry]

  /// Returns known routes keyed by module id. Unavailable catalogues throw so
  /// callers keep the previous (or empty) map — never invent AI for every line.
  public func validated(principal: Principal) throws -> [String: Entry] {
    guard hub_id == principal.hub_id, modules.count == principal.modules.count,
      Set(modules.map(\.id)) == Set(principal.modules),
      modules.allSatisfy(Self.validEntry)
    else { throw PhoneError.invalidData }
    guard available else { throw PhoneError.unavailable }
    return Dictionary(uniqueKeysWithValues: modules.map { ($0.id, $0) })
  }

  static func validEntry(_ entry: Entry) -> Bool {
    guard Wire.id(entry.id) else { return false }
    if let target = entry.target_installation_id, !target.isEmpty, !Wire.id(target) {
      return false
    }
    if !entry.known {
      return entry.effective_route.isEmpty
        && (entry.target_installation_id == nil || entry.target_installation_id?.isEmpty == true)
        && entry.ai_enabled != true && entry.remote_enabled != true
    }
    return LineAnswerRoute(rawValue: entry.effective_route) != nil
  }
}

/// Presentation cache only. Authorization and dial still use module IDs.
public struct ModuleRouteCache: Codable, Sendable {
  private let relay: String
  private let installation: String
  private let hub: String
  private let routes: [String: ModuleRoutes.Entry]

  public init(relay: String, principal: Principal, routes: [String: ModuleRoutes.Entry]) {
    self.relay = relay
    installation = principal.id
    hub = principal.hub_id
    self.routes = routes
  }

  public static func restore(_ data: Data, relay: String, principal: Principal) throws -> [String: ModuleRoutes.Entry] {
    guard data.count <= 65536 else { throw PhoneError.invalidData }
    let cache = try JSONDecoder().decode(Self.self, from: data)
    guard cache.relay == relay, cache.installation == principal.id, cache.hub == principal.hub_id,
      cache.routes.count <= 32, cache.routes.keys.allSatisfy(principal.modules.contains),
      cache.routes.values.allSatisfy(ModuleRoutes.validEntry)
    else { throw PhoneError.invalidData }
    return cache.routes
  }
}
