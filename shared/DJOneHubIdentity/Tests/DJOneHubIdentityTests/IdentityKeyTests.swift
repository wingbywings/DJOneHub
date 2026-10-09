import XCTest
import Foundation
import CryptoKit
@testable import DJOneHubIdentity

final class IdentityKeyTests: XCTestCase {
    private func fixture() throws -> [String: String] {
        var root = URL(fileURLWithPath: #filePath).deletingLastPathComponent()
        for _ in 0..<8 {
            if FileManager.default.fileExists(atPath: root.appendingPathComponent("go.mod").path) { break }
            root.deleteLastPathComponent()
        }
        let data = try Data(contentsOf: root.appendingPathComponent("internal/relay/identity/testdata/proof-v1.json"))
        return try JSONDecoder().decode([String: String].self, from: data)
    }
    func testGoGrantAndRefreshSignaturesMatchCryptoKit() throws {
        let f = try fixture()
        let hex = Array(try XCTUnwrap(f["seed_hex"]))
        let seed = try Data(stride(from: 0, to: hex.count, by: 2).map { try XCTUnwrap(UInt8(String(hex[$0...$0+1]), radix: 16)) })
        let key = try IdentityKey(rawRepresentation: seed)
        let verifier = try Curve25519.Signing.PublicKey(rawRepresentation: key.publicKey)
        XCTAssertEqual(key.publicKey.base64EncodedString(), f["public_key"])
        let grant = try key.grantProof(kind: .pairing, id: XCTUnwrap(f["grant_id"]), secret: XCTUnwrap(f["grant_secret"]))
        let grantMessage = try key.grantMessage(kind: .pairing, id: grant.id, secret: grant.secret)
        XCTAssertTrue(verifier.isValidSignature(try XCTUnwrap(Data(base64Encoded: XCTUnwrap(f["grant_signature"]))), for: grantMessage))
        XCTAssertTrue(verifier.isValidSignature(grant.signature, for: grantMessage))
        XCTAssertEqual(String(data: try key.grantMessage(kind: .pairing, id: grant.id, secret: grant.secret), encoding: .utf8), f["grant_message"])
        let refresh = try key.refreshProof(identityID: XCTUnwrap(f["identity_id"]), refreshToken: XCTUnwrap(f["refresh_token"]), challengeID: XCTUnwrap(f["challenge_id"]), challenge: XCTUnwrap(f["challenge"]))
        let refreshMessage = try key.refreshMessage(identityID: refresh.identityID, refreshToken: refresh.refreshToken, challengeID: refresh.challengeID, challenge: refresh.challenge)
        XCTAssertTrue(verifier.isValidSignature(try XCTUnwrap(Data(base64Encoded: XCTUnwrap(f["refresh_signature"]))), for: refreshMessage))
        XCTAssertTrue(verifier.isValidSignature(refresh.signature, for: refreshMessage))
        XCTAssertEqual(String(data: try key.refreshMessage(identityID: refresh.identityID, refreshToken: refresh.refreshToken, challengeID: refresh.challengeID, challenge: refresh.challenge), encoding: .utf8), f["refresh_message"])
        let body = try XCTUnwrap(JSONSerialization.jsonObject(with: JSONEncoder().encode(grant)) as? [String: String])
        XCTAssertEqual(body["public_key"], f["public_key"])
        XCTAssertEqual(body["signature"], grant.signature.base64EncodedString())
    }
    func testUnsafeCanonicalFieldsRejected() throws {
        let key = IdentityKey(), token = String(repeating: "A", count: 43)
        XCTAssertThrowsError(try key.grantProof(kind: .pairing, id: "id\"\n", secret: token))
        XCTAssertThrowsError(try key.grantProof(kind: .pairing, id: "grant", secret: "short"))
        XCTAssertThrowsError(try key.refreshProof(identityID: "ios", refreshToken: token, challengeID: "", challenge: token))
        XCTAssertThrowsError(try key.refreshProof(identityID: "ios", refreshToken: token, challengeID: "id", challenge: String(repeating: "é", count: 43)))
    }
    func testPrivateKeyRoundTripAndProofDomainBinding() throws {
        let key = IdentityKey(), token = String(repeating: "A", count: 43)
        let restored = try IdentityKey(rawRepresentation: key.rawRepresentation)
        XCTAssertEqual(key.publicKey, restored.publicKey)
        let first = try key.grantProof(kind: .pairing, id: "grant", secret: token)
        let second = try restored.grantProof(kind: .pairing, id: "grant", secret: token)
        let verifier = try Curve25519.Signing.PublicKey(rawRepresentation: key.publicKey)
        let message = try key.grantMessage(kind: .pairing, id: "grant", secret: token)
        XCTAssertTrue(verifier.isValidSignature(first.signature, for: message))
        XCTAssertTrue(verifier.isValidSignature(second.signature, for: message))
        XCTAssertFalse(verifier.isValidSignature(first.signature, for: try key.grantMessage(kind: .enrollment, id: "grant", secret: token)))
    }
}
