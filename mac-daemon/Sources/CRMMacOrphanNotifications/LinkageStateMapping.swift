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

/// Composes a per-(reason, sessionUUID, sequence) identifier for the
/// OS notification. Distinct identifiers for orphan vs conflict on
/// the same session prevent the OS from silently replacing one with
/// the other — `UNUserNotificationCenter.add(_:)` REPLACES a request
/// with an existing identifier.
///
/// The trailing sequence component is the entry's mutationSequence
/// at the moment the OS request was issued. Including it eliminates
/// a reconcile-vs-consume TOCTOU race: when the reconcile loop
/// removes a stale notification, it removes the identifier for the
/// sequence it observed in the snapshot — a freshly-raised
/// notification at a higher sequence has a different identifier and
/// is not collaterally stripped from Notification Center.
public func notificationIdentifier(
    reason: String,
    sessionUUID: String,
    sequence: UInt64
) -> String {
    "\(reason):\(sessionUUID):\(sequence)"
}

/// True iff the identifier was minted by an older daemon build that
/// used the unversioned `<reason>:<uuid>` scheme. New identifiers
/// have a trailing `:<sequence>` component, so a legacy id splits
/// to exactly two `:`-separated fields and starts with a known
/// reason prefix. UUIDs use hyphens, never colons, so this split is
/// reliable. Used at daemon startup to sweep ghost notifications
/// the new code can't track.
public func isLegacyNotificationIdentifier(_ id: String) -> Bool {
    let parts = id.split(separator: ":", omittingEmptySubsequences: false)
    guard parts.count == 2 else { return false }
    let prefix = String(parts[0])
    return prefix == NotificationReason.orphan ||
        prefix == NotificationReason.conflict
}
