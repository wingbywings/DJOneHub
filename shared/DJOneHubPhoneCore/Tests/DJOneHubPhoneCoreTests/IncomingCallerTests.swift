import XCTest
@testable import DJOneHubPhoneCore

final class IncomingCallerTests: XCTestCase {
  func testModuleRemarkChangesOnlyDisplayName() {
    let caller = IncomingCaller(display: "+86 (138) 0013-8000")
    XCTAssertEqual(caller.localizedName(moduleName: " 移动主卡 "), "+86 (138) 0013-8000 · 移动主卡")
    XCTAssertEqual(caller.phoneNumber, "+8613800138000")
    XCTAssertEqual(caller.display, "+86 (138) 0013-8000")
    XCTAssertEqual(caller.localizedName(moduleName: "主卡（module-a）"), "+86 (138) 0013-8000 · 主卡（module-a）")
    XCTAssertEqual(IncomingCaller(display: nil).localizedName(moduleName: "备用卡"), "DJOneHub · 备用卡")
  }

  func testMissingOrUnsafeModuleNameKeepsCallerDisplay() {
    let caller = IncomingCaller(display: "10086")
    for value in [nil, "", "   ", "主\n卡", "主\u{202e}卡", String(repeating: "卡", count: 171)] as [String?] {
      XCTAssertEqual(caller.localizedName(moduleName: value), "10086")
    }
  }

  func testPhoneNumberAndModuleRemainDistinct() {
    let caller = IncomingCaller(display: " +86 (138) 0013-8000 ")
    XCTAssertEqual(caller.display, "+86 (138) 0013-8000")
    XCTAssertEqual(caller.phoneNumber, "+8613800138000")
    XCTAssertEqual(IncomingCaller(display: "10086").phoneNumber, "10086")
    XCTAssertEqual(IncomingCaller(display: "dji-module-a").display, "dji-module-a")
    XCTAssertNil(IncomingCaller(display: "dji-module-a").phoneNumber)
  }

  func testUnavailableOrInvalidDisplayFallsBackWithoutInventingNumber() {
    for value in [nil, "", "   ", "138\n00138000", String(repeating: "x", count: 129)] as [String?] {
      let caller = IncomingCaller(display: value)
      XCTAssertEqual(caller.display, "DJOneHub")
      XCTAssertNil(caller.phoneNumber)
    }
  }
}
