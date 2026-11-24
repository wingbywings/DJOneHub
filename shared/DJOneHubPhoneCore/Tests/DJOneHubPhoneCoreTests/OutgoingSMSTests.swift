import XCTest
@testable import DJOneHubPhoneCore

final class OutgoingSMSTests: XCTestCase {
  func testMainlandMobileNormalizationPreservesOtherDestinationsAndLegacyRequests() throws {
    for (input, expected) in [
      ("138 0013-8000", "+8613800138000"), ("19912345678", "+8619912345678"),
      ("8613800138000", "+8613800138000"), ("0086 13800138000", "+8613800138000"),
      ("+8613800138000", "+8613800138000"), ("+14155552671", "+14155552671"),
      ("10086", "10086"), ("95588", "95588"), ("02112345678", "02112345678"),
      ("12345678901", "12345678901"), ("1380013800", "1380013800"), ("138001380000", "138001380000")
    ] {
      XCTAssertEqual(OutgoingSMS.normalizePhone(input), expected)
      XCTAssertEqual(OutgoingSMS.normalizePhone(expected), expected)
    }
    let request = try OutgoingSMS(module: "module", phone: "13800138000", message: "测试")
    XCTAssertEqual(request.phone, "+8613800138000")
    let encoded = try JSONEncoder().encode(request)
    var stored = try XCTUnwrap(JSONSerialization.jsonObject(with: encoded) as? [String: Any])
    stored["phone"] = "13800138000"
    let legacy = try JSONDecoder().decode(OutgoingSMS.self, from: JSONSerialization.data(withJSONObject: stored))
    try legacy.validate()
    XCTAssertEqual(legacy.phone, "13800138000")
    XCTAssertEqual(legacy.id, request.id)
  }

  func testNumbersAndSegmentBoundaries() throws {
    XCTAssertEqual(OutgoingSMS.normalizePhone("+86 (138) 0013-8000"), "+8613800138000")
    XCTAssertEqual(OutgoingSMS.normalizePhone("10086"), "10086")
    for number in ["", "+", "10086;AT", "１２３", "123#", String(repeating: "1", count: 21)] {
      XCTAssertNil(OutgoingSMS.normalizePhone(number))
    }
    for (text, count) in [(String(repeating: "a", count: 160), 1), (String(repeating: "a", count: 161), 2),
      (String(repeating: "^", count: 81), 2), (String(repeating: "中", count: 70), 1),
      (String(repeating: "中", count: 71), 2), (String(repeating: "🙂", count: 36), 2),
      (String(repeating: "中", count: 670), 10), (String(repeating: "中", count: 671), 11)] {
      XCTAssertEqual(OutgoingSMS.segmentCount(text), count)
    }
    XCTAssertThrowsError(try OutgoingSMS(module: "module", phone: "10086", message: " \n "))
    XCTAssertThrowsError(try OutgoingSMS(module: "module", phone: "10086", message: String(repeating: "中", count: 671)))
  }

  @MainActor func testRetriesPreserveRequestAndCheckResultBinding() async throws {
    let vault = MemoryVault(), http = MockHTTP()
    let tokens = PhoneTests().tokens(Date().addingTimeInterval(900))
    vault.data = try Wire.encoder().encode(StoredIdentity(relay: "https://relay.example",
      seed: Data(repeating: 1, count: 32), phase: "ready", tokens: tokens))
    let request = try OutgoingSMS(module: "module", phone: "10086", message: "测试🙂")
    var payloads: [Data] = []
    var wrong = false
    http.response = { path, body in
      XCTAssertEqual(path, "/v1/sms/outgoing"); payloads.append(body)
      return try JSONSerialization.data(withJSONObject: ["id": request.id,
        "installation_id": wrong ? "foreign" : tokens.principal.id, "state": "sent", "segments": 1, "code": ""])
    }
    let identity = PhoneIdentity(vault: vault, http: http)
    let sent = try await identity.sendSMS(request)
    XCTAssertEqual(sent.state, "sent")
    let restored = try JSONDecoder().decode(OutgoingSMS.self, from: JSONEncoder().encode(request))
    _ = try await identity.sendSMS(restored)
    XCTAssertEqual(try JSONSerialization.jsonObject(with: payloads[0]) as? NSDictionary,
      try JSONSerialization.jsonObject(with: payloads[1]) as? NSDictionary)
    XCTAssertEqual(http.accessTokens, [tokens.access_token, tokens.access_token])
    wrong = true
    do { _ = try await identity.sendSMS(request); XCTFail("foreign result accepted") } catch {}
  }
}
