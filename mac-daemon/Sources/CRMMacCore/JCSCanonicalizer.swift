// JCSCanonicalizer is the Swift half of the JCS+SHA-256 content-hash
// recipe shared with the Pi-side `service.ComputeContentHash`. The
// daemon canonicalizes ExternalContact* payloads before computing the
// content hash that goes into the `entity@hash` source_id; the Pi
// recomputes against `gowebpki/jcs` and rejects on byte mismatch.
//
// Scope (a strict subset of RFC 8785 — see ContentHasher for the
// full hash recipe):
//   - Root MUST be a JSON object or array. Top-level fragments
//     (string / number / bool / null) trigger a precondition.
//   - String values: emit JSON escapes for `\\`, `"`, named control
//     characters (`\b \f \n \r \t`), and `\u00XX` (lowercase hex) for
//     other ASCII control characters < 0x20. Every other byte —
//     including BMP non-ASCII and supplementary-plane scalars — emits
//     as its literal UTF-8 byte sequence. NO Unicode normalization.
//     NO surrogate-pair escaping. The `/` character is NOT escaped.
//     Matches `gowebpki/jcs@v1.0.1/jcs.go:decorateString` byte-for-byte.
//   - Object keys: sorted lexicographically by UTF-16 code-unit
//     comparison (matches `gowebpki/jcs`'s sort key). Duplicate keys
//     after sorting throw `JCSError.duplicateKey`.
//   - Numbers: integer-only, restricted to the JavaScript safe-integer
//     range `[-(2^53-1), +(2^53-1)]`. Floats AND out-of-range integers
//     trigger a precondition — `gowebpki/jcs` collapses larger integers
//     via ES6 number semantics, which would silently produce different
//     canonical bytes than Swift's exact `String(intValue)`.
//   - Booleans / null: literal `true` / `false` / `null`.
//   - Arrays / objects: in input order (arrays) / sorted order
//     (objects); comma-separated; no whitespace.
//
// The canonicalizer is intentionally narrow: it covers only the value
// shapes the daemon's payload contract uses. The cross-language
// fixture battery (`backend/internal/service/testdata/external_contact_hash_parity.json`
// + `jcs_weird_input.json` / `jcs_weird_output.json`) is the
// authoritative correctness gate.
//
// Input shape: produced by `JSONSerialization.jsonObject(with:options: [])`
// so the canonicalizer reads `NSNull`, `NSNumber`, `String`, `[Any]`,
// and `[String: Any]`. The recipe deliberately works on the parsed
// representation rather than a custom decoder so JSON sourced from
// other code paths (e.g. `JSONEncoder` output, hand-built dictionaries)
// canonicalizes identically.
import Foundation

public enum JCSError: Error, Equatable, CustomStringConvertible {
    /// Top-level was neither object nor array (the canonicalizer's
    /// supported subset rejects fragments).
    case unsupportedTopLevelFragment
    /// Numeric value was not an integer or fell outside the JavaScript
    /// safe-integer range.
    case unsupportedNumeric(reason: String)
    /// Encountered a value type the supported subset doesn't handle.
    case unsupportedType(typeName: String)
    /// Two object keys with the same UTF-16 sort key.
    case duplicateKey(String)
    /// Input bytes did not parse as JSON.
    case invalidJSON(reason: String)

    public var description: String {
        switch self {
        case .unsupportedTopLevelFragment:
            return "JCS canonicalizer requires a top-level object or array; top-level fragments are out of scope"
        case .unsupportedNumeric(let reason):
            return "JCS canonicalizer rejects numeric value: \(reason)"
        case .unsupportedType(let typeName):
            return "JCS canonicalizer encountered unsupported value type: \(typeName)"
        case .duplicateKey(let key):
            return "JCS canonicalizer found duplicate object key: \(key)"
        case .invalidJSON(let reason):
            return "JCS canonicalizer cannot parse input as JSON: \(reason)"
        }
    }
}

public enum JCSCanonicalizer {
    /// The JavaScript safe-integer boundary. Integers outside the
    /// inclusive range `[-(2^53-1), +(2^53-1)]` trigger a precondition
    /// (treated as fatal — `gowebpki/jcs` would silently round these
    /// to the nearest IEEE 754 double, producing different canonical
    /// bytes).
    public static let maxSafeInteger: Int64 = 9_007_199_254_740_991
    public static let minSafeInteger: Int64 = -9_007_199_254_740_991

    /// Canonicalize the input JSON bytes. Returns UTF-8 canonical
    /// bytes. Throws `JCSError` on unsupported shapes.
    public static func canonicalize(_ input: Data) throws -> Data {
        let parsed: Any
        do {
            // No `.allowFragments`: top-level MUST be object or array.
            parsed = try JSONSerialization.jsonObject(with: input, options: [])
        } catch {
            throw JCSError.invalidJSON(reason: String(describing: error))
        }
        return try canonicalize(value: parsed)
    }

    /// Canonicalize a parsed value tree (object / array root). Used
    /// directly by code that already has the parsed representation.
    public static func canonicalize(value: Any) throws -> Data {
        var out = Data()
        if value is [Any] || value is [String: Any] {
            try emit(value, into: &out)
            return out
        }
        // JSONSerialization without `.allowFragments` should already
        // reject fragments at parse time; this branch covers the
        // value-API entry point.
        throw JCSError.unsupportedTopLevelFragment
    }

    // MARK: - emit

    /// Emit any supported JSON value into `out`. Recursive on
    /// arrays/objects.
    private static func emit(_ value: Any, into out: inout Data) throws {
        // Order matters: NSNumber must be inspected BEFORE String /
        // Array / Dictionary because Foundation bridges some numbers
        // through NSNumber that also conform to other casts.
        if value is NSNull {
            out.append(contentsOf: "null".utf8)
            return
        }
        if let n = value as? NSNumber {
            // CFBoolean is bridged via NSNumber; the only reliable
            // discriminator is the CF type id.
            if CFGetTypeID(n) == CFBooleanGetTypeID() {
                out.append(contentsOf: n.boolValue ? "true".utf8 : "false".utf8)
                return
            }
            try emitNumber(n, into: &out)
            return
        }
        if let s = value as? String {
            emitString(s, into: &out)
            return
        }
        if let arr = value as? [Any] {
            try emitArray(arr, into: &out)
            return
        }
        if let obj = value as? [String: Any] {
            try emitObject(obj, into: &out)
            return
        }
        throw JCSError.unsupportedType(typeName: String(describing: type(of: value)))
    }

    /// Emit an integer in the JavaScript safe-integer range. Throws
    /// on non-integers or out-of-range values. The daemon's payload
    /// contract has exactly one numeric field (`version: 1`); the
    /// preconditions act as tripwires for accidental shape drift.
    private static func emitNumber(_ n: NSNumber, into out: inout Data) throws {
        // Determine whether the underlying CFNumber is integer-typed.
        // CFNumberIsFloatType returns true for kCFNumberFloat32Type,
        // Float64Type, CGFloat, etc. — covers both Swift Double and
        // Float bridged through NSNumber.
        if CFNumberIsFloatType(n as CFNumber) {
            // Allow integer-valued doubles silently (e.g. parsing
            // `1` through JSONSerialization sometimes lands as a
            // double depending on representation) by checking
            // rounding parity. Reject any actual non-integer value.
            let d = n.doubleValue
            guard d.rounded() == d, d.isFinite else {
                throw JCSError.unsupportedNumeric(
                    reason: "non-integer floating-point value (\(d)); JCS canonicalizer rejects floats")
            }
            if d > Double(maxSafeInteger) || d < Double(minSafeInteger) {
                throw JCSError.unsupportedNumeric(
                    reason: "value \(d) outside JavaScript safe-integer range")
            }
            let asInt = Int64(d)
            out.append(contentsOf: String(asInt).utf8)
            return
        }
        let asInt = n.int64Value
        guard asInt >= minSafeInteger && asInt <= maxSafeInteger else {
            throw JCSError.unsupportedNumeric(
                reason: "integer \(asInt) outside JavaScript safe-integer range")
        }
        out.append(contentsOf: String(asInt).utf8)
    }

    /// Emit a JSON-quoted string. Mirrors `gowebpki/jcs`'s
    /// `decorateString` byte-for-byte: named escapes for the JSON
    /// standard set, `\u00XX` for other ASCII controls, and
    /// pass-through for every other byte.
    static func emitString(_ s: String, into out: inout Data) {
        out.append(0x22) // opening quote
        // Walk the underlying UTF-8 bytes. Swift strings are stored
        // as UTF-8 so `s.utf8` is a no-copy view.
        for byte in s.utf8 {
            switch byte {
            case 0x22: // " (U+0022)
                out.append(contentsOf: [0x5C, 0x22])
            case 0x5C: // \ (U+005C)
                out.append(contentsOf: [0x5C, 0x5C])
            case 0x08: // \b
                out.append(contentsOf: [0x5C, 0x62])
            case 0x09: // \t
                out.append(contentsOf: [0x5C, 0x74])
            case 0x0A: // \n
                out.append(contentsOf: [0x5C, 0x6E])
            case 0x0C: // \f
                out.append(contentsOf: [0x5C, 0x66])
            case 0x0D: // \r
                out.append(contentsOf: [0x5C, 0x72])
            default:
                if byte < 0x20 {
                    // Other ASCII control chars → \u00xx (lowercase hex,
                    // matches gowebpki/jcs's `fmt.Sprintf("\\u%04x", c)`).
                    let hex = String(format: "\\u%04x", byte)
                    out.append(contentsOf: hex.utf8)
                } else {
                    out.append(byte)
                }
            }
        }
        out.append(0x22) // closing quote
    }

    /// Emit a JSON array. Items in input order. No whitespace.
    private static func emitArray(_ arr: [Any], into out: inout Data) throws {
        out.append(0x5B) // [
        for (i, item) in arr.enumerated() {
            if i > 0 { out.append(0x2C) } // ,
            try emit(item, into: &out)
        }
        out.append(0x5D) // ]
    }

    /// Emit a JSON object. Keys sorted by UTF-16 code-unit
    /// comparison. Duplicate keys (post-sort) throw.
    private static func emitObject(_ obj: [String: Any], into out: inout Data) throws {
        // Build (key, utf16-sort-key, value) tuples and sort.
        struct Entry {
            let key: String
            let sortKey: [UInt16]
            let value: Any
        }
        var entries: [Entry] = []
        entries.reserveCapacity(obj.count)
        for (k, v) in obj {
            entries.append(Entry(key: k, sortKey: Array(k.utf16), value: v))
        }
        entries.sort { compareUTF16(a: $0.sortKey, b: $1.sortKey) < 0 }

        // Duplicate keys (post-sort) — Swift Dictionary can't carry
        // them at the parse level, but defense in depth.
        var prevSortKey: [UInt16]? = nil
        for e in entries {
            if let p = prevSortKey, compareUTF16(a: p, b: e.sortKey) == 0 {
                throw JCSError.duplicateKey(e.key)
            }
            prevSortKey = e.sortKey
        }

        out.append(0x7B) // {
        for (i, e) in entries.enumerated() {
            if i > 0 { out.append(0x2C) } // ,
            emitString(e.key, into: &out)
            out.append(0x3A) // :
            try emit(e.value, into: &out)
        }
        out.append(0x7D) // }
    }

    /// Compare two UTF-16 code-unit sequences lexicographically (by
    /// numeric value of each unit, then by length). Mirrors
    /// `gowebpki/jcs`'s `lexicographicallyPrecedes`.
    private static func compareUTF16(a: [UInt16], b: [UInt16]) -> Int {
        let minLen = min(a.count, b.count)
        for i in 0..<minLen {
            let diff = Int(a[i]) - Int(b[i])
            if diff < 0 { return -1 }
            if diff > 0 { return 1 }
        }
        if a.count < b.count { return -1 }
        if a.count > b.count { return 1 }
        return 0
    }
}
