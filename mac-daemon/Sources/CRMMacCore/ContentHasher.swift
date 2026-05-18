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
        // Duplicate-key check at the byte level. Foundation's
        // JSONSerialization silently collapses duplicate object keys
        // to the last value, but gowebpki/jcs on the Go side fails
        // the JCS step. Reject up-front so cross-language hash
        // agreement is preserved on the supported input surface.
        // The check covers the TOP-LEVEL object only; nested objects
        // would also be covered by a fully recursive scan but the
        // Pi-side payload contract emits single-key nested objects,
        // so a top-level check matches the worst case the daemon
        // exercises in practice.
        if let dup = Self.duplicateTopLevelKey(in: payload) {
            throw ContentHasherError.invalidJSON(reason: "duplicate top-level key: \(dup)")
        }
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

    /// Scan the raw bytes of a top-level JSON object for duplicate
    /// keys at depth 1. Returns the first duplicated key name, or
    /// nil if all top-level keys are unique. Walks the bytes
    /// directly to bypass JSONSerialization's silent dedup. Best-
    /// effort: skips on malformed bytes (the subsequent
    /// JSONSerialization call surfaces those as `.invalidJSON`).
    static func duplicateTopLevelKey(in payload: Data) -> String? {
        var i = 0
        let bytes = [UInt8](payload)
        // Skip whitespace.
        while i < bytes.count, Self.isJSONWhitespace(bytes[i]) { i += 1 }
        guard i < bytes.count, bytes[i] == UInt8(ascii: "{") else {
            return nil
        }
        i += 1
        var seen: Set<String> = []
        while i < bytes.count {
            while i < bytes.count, Self.isJSONWhitespace(bytes[i]) { i += 1 }
            if i >= bytes.count { return nil }
            if bytes[i] == UInt8(ascii: "}") { return nil }
            if bytes[i] == UInt8(ascii: ",") { i += 1; continue }
            if bytes[i] != UInt8(ascii: "\"") { return nil }
            // Parse the key string.
            guard let (key, after) = Self.parseJSONString(bytes: bytes, from: i) else {
                return nil
            }
            if !seen.insert(key).inserted {
                return key
            }
            i = after
            while i < bytes.count, Self.isJSONWhitespace(bytes[i]) { i += 1 }
            if i >= bytes.count || bytes[i] != UInt8(ascii: ":") { return nil }
            i += 1
            // Skip the value (depth-aware walk over strings,
            // objects, arrays, literals).
            guard let valueEnd = Self.skipJSONValue(bytes: bytes, from: i) else {
                return nil
            }
            i = valueEnd
        }
        return nil
    }

    private static func isJSONWhitespace(_ b: UInt8) -> Bool {
        b == 0x20 || b == 0x09 || b == 0x0A || b == 0x0D
    }

    /// Parse a JSON string starting at the opening `"`. Returns the
    /// decoded key (unescaped enough to detect equality — escapes
    /// preserved literally; matches the on-disk lexical sense) and
    /// the index just past the closing quote. Nil on malformed input.
    private static func parseJSONString(bytes: [UInt8], from start: Int) -> (String, Int)? {
        guard start < bytes.count, bytes[start] == UInt8(ascii: "\"") else { return nil }
        var i = start + 1
        var buf: [UInt8] = []
        while i < bytes.count {
            let b = bytes[i]
            if b == UInt8(ascii: "\\") {
                if i + 1 >= bytes.count { return nil }
                buf.append(b)
                buf.append(bytes[i + 1])
                i += 2
                continue
            }
            if b == UInt8(ascii: "\"") {
                if let s = String(bytes: buf, encoding: .utf8) {
                    return (s, i + 1)
                }
                return nil
            }
            buf.append(b)
            i += 1
        }
        return nil
    }

    /// Skip past a JSON value, tracking brace/bracket nesting and
    /// strings (which can contain commas and braces). Returns the
    /// index just past the value, or nil on malformed input.
    private static func skipJSONValue(bytes: [UInt8], from start: Int) -> Int? {
        var i = start
        while i < bytes.count, isJSONWhitespace(bytes[i]) { i += 1 }
        if i >= bytes.count { return nil }
        let b = bytes[i]
        if b == UInt8(ascii: "\"") {
            return parseJSONString(bytes: bytes, from: i)?.1
        }
        if b == UInt8(ascii: "{") || b == UInt8(ascii: "[") {
            var depth = 1
            i += 1
            while i < bytes.count, depth > 0 {
                let c = bytes[i]
                if c == UInt8(ascii: "\"") {
                    guard let after = parseJSONString(bytes: bytes, from: i)?.1 else {
                        return nil
                    }
                    i = after
                    continue
                }
                if c == UInt8(ascii: "{") || c == UInt8(ascii: "[") {
                    depth += 1
                } else if c == UInt8(ascii: "}") || c == UInt8(ascii: "]") {
                    depth -= 1
                }
                i += 1
            }
            return depth == 0 ? i : nil
        }
        // literal (number/true/false/null) — walk until comma,
        // closing brace/bracket, or whitespace at depth 0.
        while i < bytes.count {
            let c = bytes[i]
            if c == UInt8(ascii: ",") || c == UInt8(ascii: "}") || c == UInt8(ascii: "]")
                || isJSONWhitespace(c) {
                return i
            }
            i += 1
        }
        return i
    }
}
