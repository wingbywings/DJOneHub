import Foundation
import CryptoKit

public enum IdentityProofError: Error { case invalidField }
public enum GrantKind: String, Sendable { case enrollment, pairing }

public struct GrantProof: Encodable, Sendable {
    public let id: String
    public let secret: String
    public let publicKey: Data
    public let signature: Data
    enum CodingKeys: String, CodingKey { case id, secret, publicKey = "public_key", signature }
}
public struct RefreshProof: Encodable, Sendable {
    public let identityID: String
    public let refreshToken: String
    public let challengeID: String
    public let challenge: String
    public let signature: Data
    enum CodingKeys: String, CodingKey {
        case identityID = "identity_id", refreshToken = "refresh_token", challengeID = "challenge_id", challenge, signature
    }
}

// Ed25519 signing shared by future macOS enrollment and iOS pairing clients.
// The caller must store rawRepresentation in Keychain; this type never writes
// private keys, refresh tokens or proofs to disk or logs.
public struct IdentityKey {
    private let key: Curve25519.Signing.PrivateKey
    public init() { key = Curve25519.Signing.PrivateKey() }
    public init(rawRepresentation: Data) throws { key = try Curve25519.Signing.PrivateKey(rawRepresentation: rawRepresentation) }
    public var rawRepresentation: Data { key.rawRepresentation }
    public var publicKey: Data { key.publicKey.rawRepresentation }

    public func grantProof(kind: GrantKind, id: String, secret: String) throws -> GrantProof {
        let message = try grantMessage(kind: kind, id: id, secret: secret)
        return GrantProof(id: id, secret: secret, publicKey: publicKey, signature: try key.signature(for: message))
    }
    public func refreshProof(identityID: String, refreshToken: String, challengeID: String, challenge: String) throws -> RefreshProof {
        let message = try refreshMessage(identityID: identityID, refreshToken: refreshToken, challengeID: challengeID, challenge: challenge)
        return RefreshProof(identityID: identityID, refreshToken: refreshToken, challengeID: challengeID, challenge: challenge, signature: try key.signature(for: message))
    }

    // All interpolated fields are restricted to safe ASCII. Explicit ordering
    // avoids JSONEncoder key order and slash-escaping differences with Go.
    func grantMessage(kind: GrantKind, id: String, secret: String) throws -> Data {
        try Self.validateID(id); try Self.validateToken(secret)
        return Data("{\"domain\":\"DJOneHub/identity/grant/v1\",\"kind\":\"\(kind.rawValue)\",\"id\":\"\(id)\",\"secret\":\"\(secret)\",\"public_key\":\"\(publicKey.base64EncodedString())\"}".utf8)
    }
    func refreshMessage(identityID: String, refreshToken: String, challengeID: String, challenge: String) throws -> Data {
        try Self.validateID(identityID); try Self.validateID(challengeID)
        try Self.validateToken(refreshToken); try Self.validateToken(challenge)
        return Data("{\"domain\":\"DJOneHub/identity/refresh/v1\",\"identity_id\":\"\(identityID)\",\"refresh_token\":\"\(refreshToken)\",\"challenge_id\":\"\(challengeID)\",\"challenge\":\"\(challenge)\"}".utf8)
    }
    private static func safe(_ value: String) -> Bool {
        !value.isEmpty && value.utf8.allSatisfy { (48...57).contains($0) || (65...90).contains($0) || (97...122).contains($0) || $0 == 45 || $0 == 95 }
    }
    private static func validateID(_ value: String) throws { guard value.utf8.count <= 128, safe(value) else { throw IdentityProofError.invalidField } }
    private static func validateToken(_ value: String) throws { guard value.utf8.count == 43, safe(value) else { throw IdentityProofError.invalidField } }
}
