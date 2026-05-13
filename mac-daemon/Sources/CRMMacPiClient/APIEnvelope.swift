// APIEnvelope wraps every Pi-side JSON response.
//
// The Pi handler convention (backend/internal/api/response.go) is:
//   {"success": bool, "data": {...}, "error": {...}, "meta": {...}}
// Both `data` and `error` are optional and mutually exclusive on
// success/failure. Endpoint-specific `Data` types are decoded
// generically — the same envelope works for pair / heartbeat /
// known-identifiers.
import Foundation

public struct APIEnvelope<Data: Decodable>: Decodable {
    public let success: Bool
    public let data: Data?
    public let error: APIError?
    // `meta` is present on paginated responses; daemon endpoints
    // don't use it but the field is decoded as `Any?`-ish via
    // JSONValue so the envelope stays forward-compatible without
    // forcing every consumer to define a meta type.
    public let meta: JSONValue?

    public init(success: Bool, data: Data?, error: APIError?, meta: JSONValue? = nil) {
        self.success = success
        self.data = data
        self.error = error
        self.meta = meta
    }
}

public struct APIError: Decodable, Equatable {
    public let code: String
    public let message: String
    public let details: String?

    public init(code: String, message: String, details: String? = nil) {
        self.code = code
        self.message = message
        self.details = details
    }
}

/// Minimal JSON value to keep envelope decoding tolerant of unknown
/// meta shapes. Treated as opaque by the daemon.
public enum JSONValue: Decodable {
    case null
    case bool(Bool)
    case number(Double)
    case string(String)
    case array([JSONValue])
    case object([String: JSONValue])

    public init(from decoder: Decoder) throws {
        let container = try decoder.singleValueContainer()
        if container.decodeNil() {
            self = .null
            return
        }
        if let v = try? container.decode(Bool.self) { self = .bool(v); return }
        if let v = try? container.decode(Double.self) { self = .number(v); return }
        if let v = try? container.decode(String.self) { self = .string(v); return }
        if let v = try? container.decode([JSONValue].self) { self = .array(v); return }
        if let v = try? container.decode([String: JSONValue].self) { self = .object(v); return }
        throw DecodingError.dataCorruptedError(
            in: container, debugDescription: "unrecognized JSON value")
    }
}
