// Domain types owned by CRMMacOrphanNotifications. Wire types from
// CRMMacPiClient are mapped into these at the composition boundary
// so the notification module doesn't depend on transport DTOs.
//
// SessionMetadataLookup — narrow protocol the notification module
// uses to retrieve a session title, time, and optional click target
// for a given session UUID.
//
// The concrete adapter (AnarlogSessionMetadataLookup) lives in
// CRMMacAnarlogSource, which reads session data through the Anarlog
// CLI and owns AnarlogConfigSource and the CLI process client.
// CRMMacOrphanNotifications defines only the protocol so the
// dependency graph stays acyclic: anarlog → notifications, never
// the reverse.
import Foundation

/// Snapshot of the data CRMMacOrphanNotifications needs to render
/// a notification for a session. Returned by SessionMetadataLookup.
public struct SessionMetadata: Sendable, Equatable {
    /// Session title from the CLI record's `title`. Nil when missing
    /// or empty; the notification falls back to "Untitled session".
    public let title: String?
    /// Session creation time from the CLI record's `created_at`. Nil
    /// when unavailable; the notification omits the time suffix.
    public let createdAt: Date?
    /// Always nil for the CLI-backed lookup. No click target reads it.
    public let sessionDirURL: URL?

    public init(title: String?, createdAt: Date?, sessionDirURL: URL?) {
        self.title = title
        self.createdAt = createdAt
        self.sessionDirURL = sessionDirURL
    }
}

/// Async, Sendable lookup contract. Returns nil for any failure
/// (config disabled, CLI failure, unknown session) — the notification
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
