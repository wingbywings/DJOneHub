import DJOneHubPhoneCore
import Foundation

public struct ProfileStatus: Decodable, Sendable {
  public struct Principal: Decodable, Sendable { public let modules: [String] }
  public let profile: String
  public let relay_url: String
  public let phase: String
  public let principal: Principal
}
public struct Installation: Decodable, Identifiable, Sendable {
  public let id: String
  public let modules: [String]
}
public enum ManagementError: Error { case invalid, commandFailed }

/// Invokes the packaged Hub executable directly, retaining its Keychain identity.
/// No shell interpolation, browser endpoint, environment token or secret argv.
public struct ManagementClient: Sendable {
  public let executable: URL
  public init(executable: URL) { self.executable = executable }
  public static func validate(profile: String) throws {
    guard Wire.id(profile), profile.utf8.count <= 64 else { throw ManagementError.invalid }
  }
  public func status(profile: String) async throws -> ProfileStatus {
    let data = try await run(profile: profile, operation: "status")
    return try JSONDecoder().decode(ProfileStatus.self, from: data)
  }
  public func installations(profile: String) async throws -> [Installation] {
    let data = try await run(profile: profile, operation: "installations")
    // Go encodes a nil installation slice as null.
    return try JSONDecoder().decode([Installation]?.self, from: data) ?? []
  }
  public func revoke(profile: String, installation: String) async throws {
    guard Wire.id(installation) else { throw ManagementError.invalid }
    _ = try await run(profile: profile, operation: "revoke", arguments: ["--installation", installation])
  }
  public func pairing(profile: String, modules: [String]) async throws -> Data {
    guard !modules.isEmpty, modules.count <= 32, modules.allSatisfy(Wire.id),
      Set(modules).count == modules.count else { throw ManagementError.invalid }
    let directory = FileManager.default.temporaryDirectory.appendingPathComponent("djonehub-pair-" + UUID().uuidString)
    try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: false,
      attributes: [.posixPermissions: 0o700])
    defer { try? FileManager.default.removeItem(at: directory) }
    let file = directory.appendingPathComponent("pairing.json")
    _ = try await run(profile: profile, operation: "pair", arguments: ["--modules", modules.joined(separator: ","), "--out", file.path])
    let handle = try FileHandle(forReadingFrom: file)
    defer { try? handle.close() }
    let data = try handle.read(upToCount: 8193) ?? Data()
    let parsed = try PairingFile.parse(data)
    guard Set(parsed.grant.modules) == Set(modules) else { throw ManagementError.invalid }
    // Compact JSON improves QR density. The file is deleted before returning.
    return try Wire.encoder().encode(parsed)
  }
  private func run(profile: String, operation: String, arguments: [String] = []) async throws -> Data {
    try Self.validate(profile: profile)
    return try await Task.detached {
      let process = Process()
      process.executableURL = executable
      process.arguments = ["remote", operation, "--profile", profile] + arguments
      let pipe = Pipe()
      process.standardOutput = pipe
      process.standardError = FileHandle.nullDevice
      try process.run()
      // All operations are bounded; a blocked Keychain prompt cannot hang the UI.
      let timeout = DispatchWorkItem { if process.isRunning { process.terminate() } }
      DispatchQueue.global().asyncAfter(deadline: .now() + 30, execute: timeout)
      defer { timeout.cancel(); try? pipe.fileHandleForReading.close() }
      var data = Data()
      do {
        while data.count <= 131072 {
          let chunk = try pipe.fileHandleForReading.read(upToCount: min(8192, 131073 - data.count)) ?? Data()
          if chunk.isEmpty { break }
          data.append(chunk)
        }
      } catch { process.terminate(); process.waitUntilExit(); throw error }
      if data.count > 131072 { process.terminate() }
      process.waitUntilExit()
      guard process.terminationStatus == 0, data.count <= 131072 else { throw ManagementError.commandFailed }
      return data
    }.value
  }
}
