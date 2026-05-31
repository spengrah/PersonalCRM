// Pure helper that builds the click-action URL for a notification.
//
// Separated from OrphanNotificationCenter / OrphanNotificationDelegate
// so it can be unit-tested without involving the actor or any OS
// API. The delegate calls this helper at tap time and passes the
// result to WorkspaceOpener.
import Foundation

/// Decides which URL to open when a notification is tapped.
///
/// Orphan notifications launch (or focus) the Anarlog app via the
/// bare `hyprnote://` scheme so the user can re-tag the session
/// in-app. Anarlog exposes no per-note deep link, so the bare app
/// scheme is the only available target — it cannot scope to the
/// specific session.
///
/// Conflict notifications open the Pi UI's Interactions tab
/// (`tab=interactions`) pre-scoped to the specific session via
/// query params.
///
/// Returns nil for unknown reasons or conflict URL-construction
/// failure. The orphan branch always returns the app scheme (it no
/// longer depends on metadata). The caller logs + no-ops the tap.
public func clickTargetURL(
    reason: String,
    sessionUUID: String,
    metadata: SessionMetadata?,
    piURL: URL
) -> URL? {
    switch reason {
    case NotificationReason.orphan:
        // Launch/focus the Anarlog app so the user can re-tag the
        // session in-app. Anarlog has no per-note deep link, so the
        // bare scheme is the only available target. Independent of
        // metadata — always non-nil.
        return URL(string: "hyprnote://")
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
            URLQueryItem(name: "tab", value: "interactions"),
            URLQueryItem(name: "session", value: sessionUUID),
        ]
        return comps.url
    default:
        return nil
    }
}
