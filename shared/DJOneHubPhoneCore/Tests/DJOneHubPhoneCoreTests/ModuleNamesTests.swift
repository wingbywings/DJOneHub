import XCTest
@testable import DJOneHubPhoneCore

final class ModuleNamesTests: XCTestCase {
  func names(_ body: String, principal: Principal? = nil) throws -> [String: String] {
    try JSONDecoder().decode(ModuleNames.self, from: Data(body.utf8))
      .validated(principal: principal ?? PhoneTests().principal())
  }
  func testNamesRejectForeignHubModulesDuplicatesAndUnsafeAliases() throws {
    XCTAssertEqual(try names(#"{"hub_id":"hub","available":true,"modules":[{"id":"module","alias":" 移动主卡 "}]}"#), ["module":"移动主卡"])
    for body in [
      #"{"hub_id":"foreign","available":true,"modules":[{"id":"module","alias":"备注"}]}"#,
      #"{"hub_id":"hub","available":true,"modules":[{"id":"foreign","alias":"备注"}]}"#,
      #"{"hub_id":"hub","available":true,"modules":[]}"#,
      #"{"hub_id":"hub","available":true,"modules":[{"id":"module","alias":"坏\n名字"}]}"#,
      #"{"hub_id":"hub","available":true,"modules":[{"id":"module","alias":"坏\u202e名字"}]}"#,
      #"{"hub_id":"hub","available":false,"modules":[{"id":"module","alias":""}]}"#,
    ] { XCTAssertThrowsError(try names(body)) }
    let p = Principal(id:"phone", tenant:"tenant", kind:"installation", hub_id:"hub", modules:["module","other"], version:1, access_expires_at:Date().addingTimeInterval(900))
    XCTAssertThrowsError(try names(#"{"hub_id":"hub","available":true,"modules":[{"id":"module","alias":"一"},{"id":"module","alias":"二"}]}"#, principal:p))
  }
  func testDisplayUsesNamesDisambiguatesAndNeverChangesID() {
    let ids = ["a","b","c","d"]
    let aliases = ["a":"主卡","b":"主卡","c":""]
    XCTAssertEqual(ModuleLineDisplay.name(for:"a", names:aliases, modules:ids),"主卡（a）")
    XCTAssertEqual(ModuleLineDisplay.name(for:"b", names:aliases, modules:ids),"主卡（b）")
    XCTAssertEqual(ModuleLineDisplay.name(for:"c", names:aliases, modules:ids),"c")
    XCTAssertEqual(ModuleLineDisplay.name(for:"d", names:aliases, modules:ids),"d")
    XCTAssertEqual(ModuleLineDisplay.name(for:"old-id", names:aliases, modules:ids),"old-id")
    XCTAssertEqual(ModuleLineDisplay.name(for:"a", names:["a":"移动"], modules:ids),"移动")
  }
  func testCacheIsScopedToRelayInstallationAndHub() throws {
    let p = PhoneTests().principal()
    let data = try JSONEncoder().encode(ModuleNameCache(relay:"https://relay.example", principal:p, names:["module":"移动主卡"]))
    XCTAssertEqual(try ModuleNameCache.restore(data, relay:"https://relay.example", principal:p),["module":"移动主卡"])
    XCTAssertThrowsError(try ModuleNameCache.restore(data, relay:"https://other.example", principal:p))
    for q in [
      Principal(id:"other-phone",tenant:p.tenant,kind:p.kind,hub_id:p.hub_id,modules:p.modules,version:1,access_expires_at:p.access_expires_at),
      Principal(id:p.id,tenant:p.tenant,kind:p.kind,hub_id:"other-hub",modules:p.modules,version:1,access_expires_at:p.access_expires_at),
      Principal(id:p.id,tenant:p.tenant,kind:p.kind,hub_id:p.hub_id,modules:["other"],version:1,access_expires_at:p.access_expires_at),
    ] { XCTAssertThrowsError(try ModuleNameCache.restore(data, relay:"https://relay.example", principal:q)) }
  }
  @MainActor func testFetchUsesAccessWithoutRotatingOrWritingCredentials() async throws {
    let vault = MemoryVault(), http = MockHTTP()
    let tokens = PhoneTests().tokens(Date().addingTimeInterval(900))
    let stored = StoredIdentity(relay:"https://relay.example", seed:Data(repeating:1,count:32), phase:"ready", tokens:tokens)
    vault.data = try Wire.encoder().encode(stored)
    let original = vault.data
    vault.failReplace = true
    http.response = { path,body in
      XCTAssertEqual(path,"/v1/module-names"); XCTAssertEqual(body,Data("{}".utf8))
      return Data(#"{"hub_id":"hub","available":true,"modules":[{"id":"module","alias":"移动主卡"}]}"#.utf8)
    }
    let identity = PhoneIdentity(vault:vault,http:http)
    let result = try await identity.moduleNames()
    XCTAssertEqual(result,["module":"移动主卡"])
    XCTAssertEqual(http.accessTokens,[tokens.access_token])
    XCTAssertEqual(vault.data,original)
    http.response = { _,_ in Data(#"{"hub_id":"hub","available":false,"modules":[{"id":"module","alias":""}]}"#.utf8) }
    do { _ = try await identity.moduleNames(); XCTFail("unavailable snapshot must preserve cache") } catch {}
    XCTAssertEqual(vault.data,original)
  }
  @MainActor func testPendingOrExpiredCredentialsDoNotTriggerRefresh() async throws {
    for phase in ["ready","refresh_pending"] {
      let vault = MemoryVault(), http = MockHTTP()
      vault.data = try Wire.encoder().encode(StoredIdentity(relay:"https://relay.example",seed:Data(repeating:1,count:32),phase:phase,tokens:PhoneTests().tokens(.distantPast)))
      let original = vault.data
      do { _ = try await PhoneIdentity(vault:vault,http:http).moduleNames(); XCTFail("expired or pending credential accepted") } catch {}
      XCTAssertTrue(http.calls.isEmpty)
      XCTAssertEqual(vault.data,original)
    }
  }
}
