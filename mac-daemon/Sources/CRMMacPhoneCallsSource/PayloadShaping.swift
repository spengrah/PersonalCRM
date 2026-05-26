// PayloadShaping — shapes CallHistoryRow rows into the Pi-side
// CallPayload JSON envelope.
//
// Wire keys mirror backend/internal/events/kinds.go CallPayload exactly.
// snake_case via explicit CodingKeys; the daemon does NOT use a
// `keyEncodingStrategy = .convertToSnakeCase` shortcut so the wire
// shape is documented at the type level.
//
// `peer_normalized` is canonicalized via HandleNormalization (the same
// helper the messages plugin uses). The Pi re-canonicalizes defensively
// in verifyCallInvariants and rejects payloads where the two don't
// match (R-orig-12 + P2-F).
//
// `answered` is *bool* (Swift Bool?) — three-state. NULL outbound
// (ZANSWERED is unreliable per S2); the reader forces nil outbound. The
// payload omits the key entirely when nil to match the Pi-side
// `omitempty` JSON tag.
import Foundation

/// Direction discriminator. The IngestEvent envelope's `kind` carries
/// this; the payload struct is identical.
public enum CallDirection: String, Sendable {
    case received = "call.received"
    case sent     = "call.sent"
}

/// The wire envelope sent as the `payload` of an IngestEvent. Same
/// struct shape for `call.received` and `call.sent` — the kind
/// discriminator on the IngestEvent envelope (NOT the payload) tells
/// the Pi which direction.
public struct CallPayload: Encodable, Equatable, Sendable {
    public let version: Int
    public let hostID: UUID
    public let source: String
    public let callUniqueID: String
    public let peerHandle: String
    public let peerNormalized: String
    public let service: PhoneCallService
    public let direction: String  // "inbound" | "outbound" — mirrors kind
    public let answered: Bool?
    public let hasVoicemail: Bool
    public let durationSeconds: Int32
    public let startedAt: Date

    enum CodingKeys: String, CodingKey {
        case version
        case hostID          = "host_id"
        case source
        case callUniqueID    = "call_unique_id"
        case peerHandle      = "peer_handle"
        case peerNormalized  = "peer_normalized"
        case service
        case direction
        case answered
        case hasVoicemail    = "has_voicemail"
        case durationSeconds = "duration_seconds"
        case startedAt       = "started_at"
    }

    public init(
        version: Int = 1,
        hostID: UUID,
        source: String = "phone_calls",
        callUniqueID: String,
        peerHandle: String,
        peerNormalized: String,
        service: PhoneCallService,
        direction: String,
        answered: Bool?,
        hasVoicemail: Bool,
        durationSeconds: Int32,
        startedAt: Date
    ) {
        self.version = version
        self.hostID = hostID
        self.source = source
        self.callUniqueID = callUniqueID
        self.peerHandle = peerHandle
        self.peerNormalized = peerNormalized
        self.service = service
        self.direction = direction
        self.answered = answered
        self.hasVoicemail = hasVoicemail
        self.durationSeconds = durationSeconds
        self.startedAt = startedAt
    }

    public func encode(to encoder: Encoder) throws {
        var c = encoder.container(keyedBy: CodingKeys.self)
        try c.encode(version, forKey: .version)
        // UUID encoded as lowercase string for Go wire-shape parity:
        // Swift's default UUID encoding emits uppercase hex; Go emits
        // lowercase. The Pi accepts both but byte-for-byte parity
        // fixtures require lowercase.
        try c.encode(hostID.uuidString.lowercased(), forKey: .hostID)
        try c.encode(source, forKey: .source)
        try c.encode(callUniqueID, forKey: .callUniqueID)
        try c.encode(peerHandle, forKey: .peerHandle)
        try c.encode(peerNormalized, forKey: .peerNormalized)
        try c.encode(service.rawValue, forKey: .service)
        try c.encode(direction, forKey: .direction)
        // answered is omitempty on the Pi side; only emit when set.
        if let answered {
            try c.encode(answered, forKey: .answered)
        }
        try c.encode(hasVoicemail, forKey: .hasVoicemail)
        try c.encode(durationSeconds, forKey: .durationSeconds)
        try c.encode(startedAt, forKey: .startedAt)
    }
}

public enum CallPayloadShaping {
    /// Convert a CallHistoryRow into a (kind, payload) pair, given the
    /// canonicalized peer handle and host UUID. The canonical peer is
    /// supplied by the caller because the plugin holds the
    /// HandleNormalization helper from CRMMacMessagesSource.
    ///
    /// Returns nil if the row's service can't be resolved — should not
    /// happen because the reader already filters service-unknown rows.
    public static func shape(
        row: CallHistoryRow,
        peerNormalized: String,
        hostID: UUID
    ) -> (kind: CallDirection, payload: CallPayload)? {
        guard let service = ServiceDerivation.resolve(
            provider: row.serviceProvider,
            callType: row.callType
        ) else {
            return nil
        }
        let kind: CallDirection = row.originated ? .sent : .received
        let direction: String = row.originated ? "outbound" : "inbound"

        // Outbound: force `answered = nil` and `hasVoicemail = false`
        // regardless of source data. The Pi re-normalizes both, but
        // the daemon does it too so the wire matches the contract.
        let answered: Bool? = row.originated ? nil : row.answered
        let hasVoicemail = row.originated ? false : row.hasMessage

        let payload = CallPayload(
            version: CRMMacPhoneCallsSource.payloadVersion,
            hostID: hostID,
            source: "phone_calls",
            callUniqueID: row.uniqueID,
            peerHandle: row.address ?? "",
            peerNormalized: peerNormalized,
            service: service,
            direction: direction,
            answered: answered,
            hasVoicemail: hasVoicemail,
            durationSeconds: row.duration,
            startedAt: row.startedAt)
        return (kind, payload)
    }
}
