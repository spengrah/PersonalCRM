// DaemonState is the on-disk representation of all non-secret daemon
// state. Loaded at process start, mutated by the heartbeat loop and
// per-source plugins, persisted via StateStore.
//
// Ships the schema and read/write only — source-specific cursor
// advancement methods land with each reader.
import Foundation

/// Top-level state file. Persisted at
/// `~/Library/Application Support/crm-mac/state.json`.
public struct DaemonState: Codable, Equatable, Sendable {
    /// Bumped when an incompatible schema change ships. Initial
    /// release is `1`.
    public var schemaVersion: Int
    /// Convenience copy of the paired host id. Authoritative copy lives
    /// in `config.json`; this field exists so `crm-mac status` can
    /// surface inconsistencies between the two files.
    public var hostID: UUID?
    /// Last successful heartbeat response from the Pi. Nil until first
    /// heartbeat after install.
    public var lastHeartbeatAt: Date?
    /// Pi-reported `protocol_version` from the most recent successful
    /// heartbeat. Used by source plugins to feature-gate themselves
    /// against older Pi instances (e.g. phone_calls requires Pi
    /// protocol_version >= 2 because the Pi must accept `call.*` event
    /// kinds). Nil means no successful heartbeat has been recorded yet.
    ///
    /// Additive Codable field — defaults to nil for existing
    /// `state.json` files written before this field existed.
    public var lastKnownPiProtocolVersion: Int32?
    /// Per-source cursor state. Keys are stable source identifiers
    /// (`"messages"`, `"icloud_contacts"`, `"phone_calls"`). Empty
    /// until source readers begin committing cursors.
    public var sources: [String: SourceState]

    public init(
        schemaVersion: Int = 1,
        hostID: UUID? = nil,
        lastHeartbeatAt: Date? = nil,
        lastKnownPiProtocolVersion: Int32? = nil,
        sources: [String: SourceState] = [:]
    ) {
        self.schemaVersion = schemaVersion
        self.hostID = hostID
        self.lastHeartbeatAt = lastHeartbeatAt
        self.lastKnownPiProtocolVersion = lastKnownPiProtocolVersion
        self.sources = sources
    }
}

/// Per-source persistent state. Source readers mutate this after
/// every tick.
public struct SourceState: Codable, Equatable, Sendable {
    /// Opaque cursor — interpretation is per-source. Empty string on
    /// first run before any cursor has been committed.
    public var cursor: String
    /// Pi-supplied cursor epoch as of the most recent successful poll.
    /// Increments when the Pi restores from backup; source readers
    /// compare this against the heartbeat response to detect mismatch.
    public var cursorEpoch: Int64
    /// True once the backfill window has been fully walked.
    public var backfillComplete: Bool
    /// Last time the scheduler invoked the source's tick — bumped at
    /// the START of every tick regardless of outcome. Used by
    /// Doctor's staleness check via `max(lastScheduledAt,
    /// lastPushedAt)` so a quiet-but-healthy source doesn't look
    /// stale (lastPushedAt only bumps on a successful publish, which
    /// a quiet source rarely does).
    ///
    /// Additive Codable field — defaults to nil for existing
    /// `state.json` files written before this column existed.
    public var lastScheduledAt: Date?
    public var lastPushedAt: Date?
    public var lastErrorAt: Date?
    /// Short human-readable error message. Long errors are truncated.
    public var lastError: String?

    public init(
        cursor: String = "",
        cursorEpoch: Int64 = 0,
        backfillComplete: Bool = false,
        lastScheduledAt: Date? = nil,
        lastPushedAt: Date? = nil,
        lastErrorAt: Date? = nil,
        lastError: String? = nil
    ) {
        self.cursor = cursor
        self.cursorEpoch = cursorEpoch
        self.backfillComplete = backfillComplete
        self.lastScheduledAt = lastScheduledAt
        self.lastPushedAt = lastPushedAt
        self.lastErrorAt = lastErrorAt
        self.lastError = lastError
    }
}

extension DaemonState {
    /// Current on-disk schema version. Bumped when a backward-incompatible
    /// change ships.
    public static let currentSchemaVersion: Int = 1
}
