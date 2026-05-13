// Endpoint data shapes mirror the Pi-side response structs at
// backend/internal/api/handlers/mac_host.go. Field names use
// snake_case to match the Pi's JSON; Swift CodingKeys map to camelCase
// property names.
import Foundation

/// Decoded `data` payload of `POST /api/v1/host`. Daemon stores the
/// API key in Keychain and the host ID + cursor epoch on disk.
public struct PairData: Decodable, Equatable {
    public let hostID: UUID
    public let apiKey: String
    public let cursorEpoch: Int64

    public init(hostID: UUID, apiKey: String, cursorEpoch: Int64) {
        self.hostID = hostID
        self.apiKey = apiKey
        self.cursorEpoch = cursorEpoch
    }

    private enum CodingKeys: String, CodingKey {
        case hostID = "host_id"
        case apiKey = "api_key"
        case cursorEpoch = "cursor_epoch"
    }
}

/// Body of `POST /api/v1/host/:id/heartbeat`.
public struct HeartbeatBody: Encodable, Equatable {
    public let daemonVersion: String
    public let protocolVersion: Int32
    /// Opaque permissions JSON. Empty `{}` until source readers
    /// populate it.
    public let permissions: Data
    /// Opaque per-source health JSON. Empty `{}` until source readers
    /// populate it.
    public let sourceHealth: Data

    public init(
        daemonVersion: String,
        protocolVersion: Int32,
        permissions: Data,
        sourceHealth: Data
    ) {
        self.daemonVersion = daemonVersion
        self.protocolVersion = protocolVersion
        self.permissions = permissions
        self.sourceHealth = sourceHealth
    }

    public func encode(to encoder: Encoder) throws {
        var c = encoder.container(keyedBy: CodingKeys.self)
        try c.encode(daemonVersion, forKey: .daemonVersion)
        try c.encode(protocolVersion, forKey: .protocolVersion)
        // Encode the raw JSON bytes as-is rather than re-serializing
        // through some intermediate map type.
        try c.encode(JSONRawValue(data: permissions), forKey: .permissions)
        try c.encode(JSONRawValue(data: sourceHealth), forKey: .sourceHealth)
    }

    private enum CodingKeys: String, CodingKey {
        case daemonVersion = "daemon_version"
        case protocolVersion = "protocol_version"
        case permissions
        case sourceHealth = "source_health"
    }
}

/// Decoded `data` payload of the heartbeat endpoint.
public struct HeartbeatData: Decodable, Equatable {
    public let ok: Bool
    public let cursorEpoch: Int64
    public let protocolVersion: Int32
    public let minProtocolVersion: Int32

    public init(
        ok: Bool,
        cursorEpoch: Int64,
        protocolVersion: Int32,
        minProtocolVersion: Int32
    ) {
        self.ok = ok
        self.cursorEpoch = cursorEpoch
        self.protocolVersion = protocolVersion
        self.minProtocolVersion = minProtocolVersion
    }

    private enum CodingKeys: String, CodingKey {
        case ok
        case cursorEpoch = "cursor_epoch"
        case protocolVersion = "protocol_version"
        case minProtocolVersion = "min_protocol_version"
    }
}

/// Decoded `data` payload of `GET /api/v1/host/:id/known-identifiers`.
public struct KnownIdentifiersData: Decodable, Equatable {
    public let phones: [String]
    public let emails: [String]

    public init(phones: [String], emails: [String]) {
        self.phones = phones
        self.emails = emails
    }
}

/// Helper that encodes a pre-serialized JSON `Data` value verbatim.
/// Used by HeartbeatBody for permissions / sourceHealth.
private struct JSONRawValue: Encodable {
    let data: Data

    func encode(to encoder: Encoder) throws {
        // Decode the raw bytes back into a JSONValue and re-encode via
        // the current Encoder so the output matches the surrounding
        // encoding strategy. This handles both `{"k":"v"}` and `{}`.
        let value = try JSONDecoder().decode(JSONValue.self, from: data)
        try value.encode(to: encoder)
    }
}

/// Encodable mirror of CRMMacPiClient.JSONValue. Defined locally to
/// avoid Conditional Conformance complexity — the encoded form matches
/// the original JSON bytes structurally.
extension JSONValue: Encodable {
    public func encode(to encoder: Encoder) throws {
        var c = encoder.singleValueContainer()
        switch self {
        case .null: try c.encodeNil()
        case .bool(let v): try c.encode(v)
        case .number(let v): try c.encode(v)
        case .string(let v): try c.encode(v)
        case .array(let v): try c.encode(v)
        case .object(let v): try c.encode(v)
        }
    }
}
