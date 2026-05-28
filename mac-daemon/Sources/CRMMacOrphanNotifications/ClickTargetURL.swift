// Pure helper that builds the click-action URL for a notification.
//
// Separated from OrphanNotificationCenter / OrphanNotificationDelegate
// so it can be unit-tested without involving the actor or any OS
// API. The delegate calls this helper at tap time and passes the
// result to WorkspaceOpener.
import Foundation

/// Decides which URL to open when a notification is tapped.
///
/// Orphan notifications open the local session directory
/// (`metadata.sessionDirURL`); the user lands in Finder ready to
/// edit `_meta.json.participants`.
///
/// Conflict notifications open the Pi UI's needs-attention tab
/// pre-scoped to the specific session via query params.
///
/// Returns nil for unknown reasons, missing orphan metadata, or
/// any URL-construction failure. The caller logs + no-ops the
/// tap.
public func clickTargetURL(
    reason: String,
    sessionUUID: String,
    metadata: SessionMetadata?,
    piURL: URL
) -> URL? {
    switch reason {
    case NotificationReason.orphan:
        return metadata?.sessionDirURL
    case NotificationReason.conflict:
        // Build via URLComponents so the session UUID is
        // percent-encoded safely. String concat would silently
        // corrupt any UUID with characters URLs treat specially.
        guard var comps = URLComponents(url: piURL, resolvingAgainstBaseURL: false) else {
            return nil
        }
        let basePath = comps.path.hasSuffix("/")
            ? String(comps.path.dropLast())
            : comps.path
        comps.path = basePath + "/imports"
        comps.queryItems = [
            URLQueryItem(name: "tab", value: "needs-attention"),
            URLQueryItem(name: "session", value: sessionUUID),
        ]
        return comps.url
    default:
        return nil
    }
}
