// IngestEndpoint — wire shapes + helpers for POST /api/v1/ingest/events.
//
// The Pi's IngestResponse (backend/internal/api/handlers/ingest.go:113)
// is deliberately NOT wrapped in api.APIResponse — see the inline
// comment in the Go source. So the Swift decoder reads the raw
// top-level JSON, NOT via APIEnvelope<T>.
//
// Wire keys are snake_case via explicit CodingKeys. The `payload` of an
// IngestEvent is raw JSON bytes (NOT base64) — Swift's default `Data`
// encoder emits base64 which the Pi rejects, so we use RawJSON to
// inline the payload as a nested JSON object.
import Foundation

/// The top-level POST body. The Pi enforces ≤ 500 events and 8 MiB body;
/// our daemon caps at 200 events / 1 MB to stay well below.
public struct IngestEventsBody: Encodable, Equatable, Sendable {
    public let events: [IngestEvent]

    public init(events: [IngestEvent]) {
        self.events = events
    }
}

/// Mirrors backend/internal/api/handlers/ingest.go:84
/// IngestEventRequest exactly:
///   source     string
///   source_id  string (optional in Pi struct; we always set)
///   kind       string
///   payload    json.RawMessage (inline JSON object — NOT base64)
///   observed_at *time.Time (RFC3339)
public struct IngestEvent: Encodable, Equatable, Sendable {
    public let source: String
    public let sourceID: String
    public let kind: String
    public let payload: RawJSON
    public let observedAt: Date

    public init(source: String, sourceID: String, kind: String,
                payload: RawJSON, observedAt: Date) {
        self.source = source
        self.sourceID = sourceID
        self.kind = kind
        self.payload = payload
        self.observedAt = observedAt
    }

    enum CodingKeys: String, CodingKey {
        case source
        case sourceID   = "source_id"
        case kind
        case payload
        case observedAt = "observed_at"
    }
}

/// Wrapper that encodes raw JSON bytes verbatim into a parent encoder
/// as a nested JSON object — NOT base64-encoded like Swift's default
/// Data encoding.  Validates that the bytes parse as JSON; throws an
/// EncodingError if they don't.
public struct RawJSON: Encodable, Equatable, Sendable {
    public let bytes: Data

    public init(_ bytes: Data) {
        self.bytes = bytes
    }

    public init(_ string: String) {
        self.bytes = Data(string.utf8)
    }

    public func encode(to encoder: Encoder) throws {
        let parsed: Any
        do {
            parsed = try JSONSerialization.jsonObject(
                with: bytes, options: [.fragmentsAllowed])
        } catch {
            throw EncodingError.invalidValue(bytes, .init(
                codingPath: encoder.codingPath,
                debugDescription: "RawJSON expects valid JSON bytes (\(error))"))
        }
        // Re-serialize the parsed value via JSONValue (the same value
        // type used by APIEnvelope/Endpoints.swift) so the output
        // routes back through the parent encoder's container.
        let jsonValue = JSONValue.from(parsed)
        var container = encoder.singleValueContainer()
        try container.encode(jsonValue)
    }
}

/// Mirrors IngestResponse.  Pi-side response is NOT enveloped, so
/// callers decode directly via JSONDecoder.decode(IngestEventsData.self).
public struct IngestEventsData: Decodable, Equatable, Sendable {
    public let accepted: Int
    public let duplicate: Int
    public let rejected: Int
    public let errors: [IngestEventError]

    public init(accepted: Int, duplicate: Int, rejected: Int,
                errors: [IngestEventError]) {
        self.accepted = accepted
        self.duplicate = duplicate
        self.rejected = rejected
        self.errors = errors
    }
}

public struct IngestEventError: Decodable, Equatable, Sendable {
    public let index: Int
    public let code: String
    public let message: String

    public init(index: Int, code: String, message: String) {
        self.index = index
        self.code = code
        self.message = message
    }
}

// MARK: - JSONValue conversion

extension JSONValue {
    /// Convert a JSONSerialization-parsed Any tree into a JSONValue.
    /// Mirrors the inverse of the Encodable conformance in Endpoints.swift.
    static func from(_ any: Any) -> JSONValue {
        if any is NSNull {
            return .null
        }
        // JSONSerialization wraps every numeric literal — including
        // booleans — as NSNumber. `any as? Bool` returns true for
        // NSNumber(value: 0) and NSNumber(value: 1), so the only
        // reliable way to tell `true` from `1` is CFBooleanGetTypeID.
        if let n = any as? NSNumber {
            if CFGetTypeID(n) == CFBooleanGetTypeID() {
                return .bool(n.boolValue)
            }
            return .number(n.doubleValue)
        }
        if let s = any as? String {
            return .string(s)
        }
        if let a = any as? [Any] {
            return .array(a.map(JSONValue.from))
        }
        if let o = any as? [String: Any] {
            var out: [String: JSONValue] = [:]
            for (k, v) in o {
                out[k] = JSONValue.from(v)
            }
            return .object(out)
        }
        return .null
    }
}
