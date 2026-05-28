// Domain-side input types for the orphan-notification flow.
// CRMMacOrphanNotifications consumes these instead of the raw
// transport DTOs from CRMMacPiClient — that keeps PiClient wire
// shapes from leaking into the notification module's public
// surface. The composition boundary (DaemonCommand for production,
// SmokeTests for tests) maps the wire DTOs to these.
//
// These mirror the relevant fields of NeedsAttentionItem (ingest
// path) and NeedsAttentionListItem (reconcile path), minus the
// fields the notification module doesn't read.
import Foundation

/// One needs-attention entry for the consume() / ingest path.
/// Mirrors CRMMacPiClient.NeedsAttentionItem.
public struct NotificationConsumeItem: Sendable, Equatable {
    public let sessionID: String
    /// One of the canonical user-facing reason strings:
    /// "orphan" or "conflict". Other values are dropped + logged.
    public let reason: String

    public init(sessionID: String, reason: String) {
        self.sessionID = sessionID
        self.reason = reason
    }
}

/// One row from the Pi's /needs-attention response, for the
/// reconcile() path. Mirrors CRMMacPiClient.NeedsAttentionListItem
/// minus the fields the daemon ignores (id, mac_host_id,
/// summary_excerpt, candidates, linked_*).
public struct NotificationReconcileItem: Sendable, Equatable {
    public let anarlogSessionID: String   // lowercased UUID
    /// Pi-internal linkage_state ("conflict_pending" |
    /// "orphan_needs_review"). Mapped to a user-facing reason via
    /// LinkageStateMapping; unrecognized values are dropped.
    public let linkageState: String
    public let title: String?
    public let meetingAt: String          // RFC3339 string

    public init(
        anarlogSessionID: String,
        linkageState: String,
        title: String?,
        meetingAt: String
    ) {
        self.anarlogSessionID = anarlogSessionID
        self.linkageState = linkageState
        self.title = title
        self.meetingAt = meetingAt
    }
}
