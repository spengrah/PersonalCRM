// LinkageStateMapping — pure mapping from the Pi's internal
// linkage_state strings to the daemon's user-facing reason
// strings.
//
// The ingest path's NeedsAttentionItem.reason field is already
// pre-mapped on the Pi side (see service.NeedsAttentionReason*),
// so this helper is reconcile-path only.
import Foundation

/// Pi-internal linkage_state values, named here so the mapping
/// helper has typed constants to test against (avoids magic
/// strings in the actor body).
public enum PiLinkageState {
    public static let conflictPending = "conflict_pending"
    public static let orphanNeedsReview = "orphan_needs_review"
}

/// Daemon-side user-facing reason values, named here for the same
/// reason. These match the Pi's pre-mapped values that arrive on
/// the ingest path (so `consume(...)` and `reconcile(...)` end up
/// using the same reason vocabulary downstream).
public enum NotificationReason {
    public static let conflict = "conflict"
    public static let orphan = "orphan"
}

/// Maps a Pi-supplied `linkage_state` to the daemon's user-facing
/// reason. Returns nil for any unrecognized value — the caller
/// logs + skips so a future Pi-side state doesn't crash the
/// daemon.
public func mapLinkageStateToReason(_ linkageState: String) -> String? {
    switch linkageState {
    case PiLinkageState.conflictPending:
        return NotificationReason.conflict
    case PiLinkageState.orphanNeedsReview:
        return NotificationReason.orphan
    default:
        return nil
    }
}

/// Composes a stable per-(reason, sessionUUID) identifier for the
/// OS notification. Distinct identifiers for orphan vs conflict on
/// the same session prevent the OS from silently replacing one
/// with the other — `UNUserNotificationCenter.add(_:)` REPLACES a
/// request with an existing identifier.
public func notificationIdentifier(reason: String, sessionUUID: String) -> String {
    "\(reason):\(sessionUUID)"
}
