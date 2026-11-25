import XCTest
@testable import DJOneHubPhoneCore

final class ModuleRoutesTests: XCTestCase {
  func routes(_ body: String, principal: Principal? = nil) throws -> [String: ModuleRoutes.Entry] {
    try JSONDecoder().decode(ModuleRoutes.self, from: Data(body.utf8))
      .validated(principal: principal ?? PhoneTests().principal())
  }

  func testRoutesRejectForeignHubAndUnavailable() throws {
    let ok = try routes(
      #"{"hub_id":"hub","available":true,"modules":[{"id":"module","effective_route":"agent","known":true,"ai_enabled":true}]}"#)
    XCTAssertEqual(ok["module"]?.route, .agent)
    for body in [
      #"{"hub_id":"foreign","available":true,"modules":[{"id":"module","effective_route":"local","known":true}]}"#,
      #"{"hub_id":"hub","available":true,"modules":[{"id":"foreign","effective_route":"local","known":true}]}"#,
      #"{"hub_id":"hub","available":true,"modules":[]}"#,
      #"{"hub_id":"hub","available":true,"modules":[{"id":"module","effective_route":"bogus","known":true}]}"#,
      #"{"hub_id":"hub","available":false,"modules":[{"id":"module","effective_route":"agent","known":true,"ai_enabled":true}]}"#,
    ] { XCTAssertThrowsError(try routes(body)) }
  }

  func testUnknownEntriesStayWithoutRoute() throws {
    let values = try routes(
      #"{"hub_id":"hub","available":true,"modules":[{"id":"module","effective_route":"","known":false}]}"#)
    XCTAssertNil(values["module"]?.route)
    XCTAssertFalse(values["module"]?.known ?? true)
  }

  func testCacheIsScopedToRelayInstallationAndHub() throws {
    let p = PhoneTests().principal()
    let entry = ModuleRoutes.Entry(id: "module", effective_route: "remote", known: true,
                                   target_installation_id: p.id, remote_enabled: true)
    let data = try JSONEncoder().encode(ModuleRouteCache(relay: "https://relay.example", principal: p, routes: ["module": entry]))
    let restored = try ModuleRouteCache.restore(data, relay: "https://relay.example", principal: p)
    XCTAssertEqual(restored["module"]?.route, .remote)
    XCTAssertThrowsError(try ModuleRouteCache.restore(data, relay: "https://other.example", principal: p))
  }

  @MainActor func testFetchUsesAccessWithoutRotatingCredentials() async throws {
    let vault = MemoryVault(), http = MockHTTP()
    let tokens = PhoneTests().tokens(Date().addingTimeInterval(900))
    let stored = StoredIdentity(relay: "https://relay.example", seed: Data(repeating: 1, count: 32), phase: "ready", tokens: tokens)
    vault.data = try Wire.encoder().encode(stored)
    let original = vault.data
    vault.failReplace = true
    http.response = { path, body in
      XCTAssertEqual(path, "/v1/module-routes")
      XCTAssertEqual(body, Data("{}".utf8))
      return Data(#"{"hub_id":"hub","available":true,"modules":[{"id":"module","effective_route":"agent","known":true,"ai_enabled":true}]}"#.utf8)
    }
    let identity = PhoneIdentity(vault: vault, http: http)
    let result = try await identity.moduleRoutes()
    XCTAssertEqual(result["module"]?.route, .agent)
    XCTAssertEqual(http.accessTokens, [tokens.access_token])
    XCTAssertEqual(vault.data, original)
    http.response = { _, _ in
      Data(#"{"hub_id":"hub","available":false,"modules":[{"id":"module","effective_route":"","known":false}]}"#.utf8)
    }
    do {
      _ = try await identity.moduleRoutes()
      XCTFail("unavailable snapshot must preserve cache")
    } catch {}
    XCTAssertEqual(vault.data, original)
  }
}
