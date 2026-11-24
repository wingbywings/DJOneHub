import XCTest
@testable import DJOneHubPhoneCore

final class SMSNotificationTests: XCTestCase {
  func testSMSDecodesUnicodeAndRejectsForeignDestinations() throws {
    let principal = PhoneTests().principal()
    var sms: [String: Any] = ["id": String(repeating: "a", count: 64), "hub_id": principal.hub_id,
      "module_id": principal.modules[0], "installation_id": principal.id, "sender": "10086",
      "content": "", "timestamp": "2026-11-07T10:00:00.123456789Z"]
    func decode(_ sms: [String: Any]) throws -> SMSNotification {
      let data = try JSONSerialization.data(withJSONObject: ["aps": ["alert": ["body": "验证码 1234🙂"]], "sms": sms])
      return try SMSNotification.decode(data, principal: principal)
    }
    let message = try decode(sms)
    XCTAssertEqual(message.content, "验证码 1234🙂")
    XCTAssertEqual(message.sender, "10086")
    for key in ["hub_id", "module_id", "installation_id", "id"] {
      var other = sms; other[key] = "foreign"
      XCTAssertThrowsError(try decode(other))
    }
    sms["timestamp"] = "invalid"
    XCTAssertThrowsError(try decode(sms))
  }

  @MainActor func testAlertRegistrationUsesSeparateAuthenticatedEndpoint() async throws {
    let vault = MemoryVault(), http = MockHTTP()
    let tokens = PhoneTests().tokens(Date().addingTimeInterval(900))
    vault.data = try Wire.encoder().encode(StoredIdentity(relay: "https://relay.example",
      seed: Data(repeating: 1, count: 32), phase: "ready", tokens: tokens))
    http.response = { path, body in
      XCTAssertEqual(path, "/v1/push/alerts")
      let payload = try JSONSerialization.jsonObject(with: body) as! [String: String]
      XCTAssertEqual(payload["environment"], "sandbox")
      return try JSONSerialization.data(withJSONObject: ["registered": payload["token"] != ""])
    }
    let identity = PhoneIdentity(vault: vault, http: http)
    try await identity.registerPush(environment: "sandbox", token: String(repeating: "b", count: 64), alerts: true)
    try await identity.registerPush(environment: "sandbox", token: "", alerts: true)
    XCTAssertEqual(http.accessTokens, [tokens.access_token, tokens.access_token])
  }
}
