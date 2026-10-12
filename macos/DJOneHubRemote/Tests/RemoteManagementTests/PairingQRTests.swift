import Foundation
import Vision
import XCTest
@testable import RemoteManagement

final class PairingQRTests: XCTestCase {
  func testNativeQRCodeRoundTrip() throws {
    let data = try payload()
    let image = try PairingQRCode.image(data)
    let request = VNDetectBarcodesRequest()
    request.symbologies = [.qr]
    try VNImageRequestHandler(cgImage: image).perform([request])
    XCTAssertEqual(request.results?.first?.payloadStringValue, String(data: data, encoding: .utf8))
  }
  func testRejectsExpiredOrUntrustedOriginBeforeRendering() throws {
    XCTAssertThrowsError(try PairingQRCode.image(payload(origin: "http://example.test")))
    XCTAssertThrowsError(try PairingQRCode.image(payload(lifetime: -1)))
    XCTAssertThrowsError(try PairingQRCode.image(payload(origin: "https://example.test/unexpected")))
  }
  private func payload(origin: String = "https://example.test", lifetime: TimeInterval = 300) throws -> Data {
    try JSONSerialization.data(withJSONObject: ["version": 1, "relay_url": origin,
      "grant": ["id": "grant_test", "secret": String(repeating: "a", count: 43),
        "expires_at": ISO8601DateFormatter().string(from: Date().addingTimeInterval(lifetime)),
        "modules": ["dji-module-1", "dji-module-2"]]])
  }
}
