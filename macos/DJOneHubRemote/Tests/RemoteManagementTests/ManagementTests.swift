import XCTest
@testable import RemoteManagement
final class ManagementTests: XCTestCase {
  func testProfileArgumentValidation() throws {
    try ManagementClient.validate(profile: "default-1")
    for value in ["", "../default", "a b", "$(id)", String(repeating: "a", count: 65)] {
      XCTAssertThrowsError(try ManagementClient.validate(profile: value))
    }
  }
  func testEmptyInstallationsWireFormat() throws {
    XCTAssertNil(try JSONDecoder().decode([Installation]?.self, from: Data("null".utf8)))
    XCTAssertEqual(try JSONDecoder().decode([Installation].self, from: Data("[]".utf8)).count, 0)
  }
}

extension ManagementTests {
  func testActualProcessReadsChunkedOutputAndRejectsFailure() async throws {
    let dir = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
    try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: false)
    defer { try? FileManager.default.removeItem(at: dir) }
    let binary = dir.appendingPathComponent("hub")
    let script = """
    #!/bin/sh
    [ "$1" = remote ] && [ "$3" = --profile ] && [ "$4" = default ] || exit 2
    case "$2" in
      status)
        printf '%s' '{"profile":"default","relay_url":"https://example.test",'
        sleep 0.02
        printf '%s' '"phase":"ready","principal":{"modules":["m"]}}'
        ;;
      installations) printf '%s' null ;;
      revoke) exit 1 ;;
      *) exit 2 ;;
    esac
    """
    try Data(script.utf8).write(to: binary)
    try FileManager.default.setAttributes([.posixPermissions: 0o700], ofItemAtPath: binary.path)
    let client = ManagementClient(executable: binary)
    let status = try await client.status(profile: "default")
    XCTAssertEqual(status.principal.modules, ["m"])
    let installations = try await client.installations(profile: "default")
    XCTAssertTrue(installations.isEmpty)
    do { try await client.revoke(profile: "default", installation: "ios_1"); XCTFail("must reject") }
    catch {}
  }
}
