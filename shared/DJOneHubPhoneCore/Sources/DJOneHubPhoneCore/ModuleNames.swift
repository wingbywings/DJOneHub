import Foundation

public struct ModuleNames: Codable, Sendable {
  public struct Entry: Codable, Sendable {
    public let id: String
    public let alias: String
  }
  public let hub_id: String
  public let available: Bool
  public let modules: [Entry]

  public func validated(principal: Principal) throws -> [String: String] {
    guard hub_id == principal.hub_id, modules.count == principal.modules.count,
      Set(modules.map(\.id)) == Set(principal.modules),
      modules.allSatisfy({ Self.validAlias($0.alias) })
    else { throw PhoneError.invalidData }
    guard available else { throw PhoneError.unavailable }
    return Dictionary(uniqueKeysWithValues: modules.map {
      ($0.id, $0.alias.trimmingCharacters(in: .whitespacesAndNewlines))
    })
  }

  static func validAlias(_ value: String) -> Bool {
    value.utf8.count <= 320 && value.unicodeScalars.count <= 80
      && !value.unicodeScalars.contains {
        $0.properties.generalCategory == .control || $0.properties.generalCategory == .format
      }
  }
}

/// Only presentation metadata is cached. Routing and authorization always use module IDs.
public struct ModuleNameCache: Codable, Sendable {
  private let relay: String
  private let installation: String
  private let hub: String
  private let names: [String: String]

  public init(relay: String, principal: Principal, names: [String: String]) {
    self.relay = relay
    installation = principal.id
    hub = principal.hub_id
    self.names = names
  }
  public static func restore(_ data: Data, relay: String, principal: Principal) throws -> [String: String] {
    guard data.count <= 32768 else { throw PhoneError.invalidData }
    let cache = try JSONDecoder().decode(Self.self, from: data)
    guard cache.relay == relay, cache.installation == principal.id, cache.hub == principal.hub_id,
      cache.names.count <= 32, cache.names.keys.allSatisfy(principal.modules.contains),
      cache.names.values.allSatisfy(ModuleNames.validAlias)
    else { throw PhoneError.invalidData }
    return cache.names
  }
}

public enum ModuleLineDisplay {
  public static func name(for id: String, names: [String: String], modules: [String]) -> String {
    let label = names[id].flatMap { $0.isEmpty ? nil : $0 } ?? id
    if modules.contains(id), modules.contains(where: {
      $0 != id && (names[$0].flatMap { $0.isEmpty ? nil : $0 } ?? $0) == label
    }) { return "\(label)（\(id)）" }
    return label
  }
}
