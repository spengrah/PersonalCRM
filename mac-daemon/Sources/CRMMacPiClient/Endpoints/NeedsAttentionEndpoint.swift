// NeedsAttentionEndpoint — wire shape for GET
// /api/v1/meeting-notes/needs-attention?host_id=<uuid>.
//
// The Pi-side handler returns a standard enveloped response
// ({success, data, error, meta}); this file declares only the
// `data` payload shape — the daemon decodes via the existing
// decodeSuccess helper.
//
// The daemon reads a subset of the Pi's full response: id,
// anarlogSessionID, linkageState, title, meetingAt. Other fields
// (summary_excerpt, candidates, mac_host_id, linked_kind,
// linked_id) are silently ignored — they're consumed by the
// Pi-side UI, not the daemon's notification logic. The decoder
// tolerates them via Codable's default behavior (unknown JSON
// keys are skipped).
//
// `linkage_state` is the Pi's internal state string
// ("conflict_pending" | "orphan_needs_review"). The daemon maps
// it to its user-facing "reason" string ("conflict" | "orphan")
// via LinkageStateMapping.mapLinkageStateToReason — that helper
// lives in CRMMacOrphanNotifications, not here, because the
// mapping is a domain decision owned by the notification module.
import Foundation

/// Decoded `data` payload of `GET
/// /api/v1/meeting-notes/needs-attention`. One entry per
/// meeting_note row currently in the conflict_pending or
/// orphan_needs_review state.
public struct NeedsAttentionListItem: Decodable, Equatable, Sendable {
    /// The meeting_note row's UUID. Used as the cursor key for
    /// future per-row operations (resolve-link, etc.); the daemon
    /// doesn't currently dispatch on this field but stores it for
    /// observability.
    public let id: UUID
    /// The Anarlog session UUID. This is what the daemon's
    /// pending-notifications list keys on — it matches the
    /// IngestEventsData.needsAttention[].sessionID emitted by
    /// the ingest path.
    public let anarlogSessionID: UUID
    /// One of "conflict_pending" or "orphan_needs_review"; future
    /// states map to nil reason and are dropped + logged.
    public let linkageState: String
    /// Human-readable session title pulled from the meeting_note
    /// row. Nil when the original ingest had no title; the
    /// notification falls back to the local filesystem lookup,
    /// then to "Untitled session".
    public let title: String?
    /// Pi-formatted RFC3339 timestamp of the session. Decoded as
    /// String (not Date) because the daemon only renders it via
    /// the local DateFormatter; round-tripping through Date would
    /// introduce locale brittleness with no benefit.
    public let meetingAt: String

    public init(
        id: UUID,
        anarlogSessionID: UUID,
        linkageState: String,
        title: String?,
        meetingAt: String
    ) {
        self.id = id
        self.anarlogSessionID = anarlogSessionID
        self.linkageState = linkageState
        self.title = title
        self.meetingAt = meetingAt
    }

    private enum CodingKeys: String, CodingKey {
        case id
        case anarlogSessionID = "anarlog_session_id"
        case linkageState = "linkage_state"
        case title
        case meetingAt = "meeting_at"
    }
}
