// Domain types owned by CRMMacOrphanNotifications. Wire types from
// CRMMacPiClient are mapped into these at the composition boundary
// so the notification module doesn't depend on transport DTOs.
//
// SessionMetadataLookup — narrow protocol the notification module
// uses to retrieve session title + time + directory URL for a
// given session UUID.
//
// The concrete adapter (AnarlogSessionMetadataLookup) lives in
// CRMMacAnarlogSource because that target already owns the
// AnarlogConfigSource + AnarlogPathResolver + AnarlogSessionMetaParser
// deps. CRMMacOrphanNotifications defines only the protocol so the
// dep graph stays acyclic: anarlog → notifications, never the
// reverse.
import Foundation

/// Snapshot of the data CRMMacOrphanNotifications needs to render
/// a notification for a session. Returned by SessionMetadataLookup.
public struct SessionMetadata: Sendable, Equatable {
    /// Session title from `_meta.json`. Nil when missing or empty;
    /// the notification falls back to "Untitled session".
    public let title: String?
    /// Session creation time from `_meta.json.created_at`. Nil when
    /// unavailable; the notification omits the time suffix.
    public let createdAt: Date?
    /// File URL of the session directory (the click target for
    /// orphan notifications). Nil when the directory doesn't exist
    /// on disk.
    public let sessionDirURL: URL?

    public init(title: String?, createdAt: Date?, sessionDirURL: URL?) {
        self.title = title
        self.createdAt = createdAt
        self.sessionDirURL = sessionDirURL
    }
}

/// Async, Sendable lookup contract. Returns nil for any failure
/// (config disabled, sessions root missing, session dir missing,
/// _meta.json missing/unreadable/unparseable) — the notification
/// path falls back to "Untitled session" without surfacing the
/// underlying error.
public protocol SessionMetadataLookup: Sendable {
    func lookup(sessionUUID: String) async -> SessionMetadata?
}

/// Convenience: a lookup that always returns nil. Useful as the
/// fallback when the daemon is composed without an Anarlog
/// config (then orphan notifications still render via "Untitled
/// session" + no time suffix).
public struct NilSessionMetadataLookup: SessionMetadataLookup {
    public init() {}
    public func lookup(sessionUUID: String) async -> SessionMetadata? { nil }
}
