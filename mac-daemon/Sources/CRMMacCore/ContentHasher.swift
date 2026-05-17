// ContentHasher implements the JCS+SHA-256 content-hash recipe shared
// with `backend/internal/service/external_contact_hash.go`. The Mac
// daemon computes this hash for every `external_contact.upserted`
// payload before constructing the `<entity>@<hash>` source_id; the Pi
// verifies the recipe byte-for-byte and rejects on mismatch.
//
// The recipe:
//   1. Strip the top-level `host_id` key from the input JSON bytes.
//   2. JCS-canonicalize the stripped bytes (subset per
//      JCSCanonicalizer).
//   3. SHA-256, lowercase hex.
//
// Byte parity with the Go side is the contract. The cross-language
// fixture (`backend/internal/service/testdata/external_contact_hash_parity.json`)
// is the authoritative test gate.
import Foundation
import CryptoKit

public enum ContentHasherError: Error, Equatable, CustomStringConvertible {
    case invalidJSON(reason: String)
    case unsupportedTopLevelType
    case canonicalize(reason: String)

    public var description: String {
        switch self {
        case .invalidJSON(let r):
            return "ContentHasher: invalid JSON (\(r))"
        case .unsupportedTopLevelType:
            return "ContentHasher: top-level must be a JSON object"
        case .canonicalize(let r):
            return "ContentHasher: canonicalize failed (\(r))"
        }
    }
}

public enum ContentHasher {
    /// Returns the lowercase-hex SHA-256 of the JCS-canonicalized
    /// payload with the top-level `host_id` key removed.
    ///
    /// `payload` MUST be a valid JSON object (top-level array is
    /// rejected — the Pi-side payload contract is always object-shaped).
    /// Returns a 64-character hex string.
    public static func contentHash(for payload: Data) throws -> String {
        let parsed: Any
        do {
            parsed = try JSONSerialization.jsonObject(with: payload, options: [])
        } catch {
            throw ContentHasherError.invalidJSON(reason: String(describing: error))
        }
        guard var obj = parsed as? [String: Any] else {
            throw ContentHasherError.unsupportedTopLevelType
        }
        // Strip the top-level host_id key. Mirrors
        // sjson.DeleteBytes(payload, "host_id") on the Go side. Both
        // implementations strip ONLY the top-level key; nested
        // host_id fields (if any) are preserved.
        obj.removeValue(forKey: "host_id")

        let canonical: Data
        do {
            canonical = try JCSCanonicalizer.canonicalize(value: obj)
        } catch {
            throw ContentHasherError.canonicalize(reason: String(describing: error))
        }
        let digest = SHA256.hash(data: canonical)
        return digest.map { String(format: "%02x", $0) }.joined()
    }
}
