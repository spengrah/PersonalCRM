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
    /// Per-session orphan/conflict notifications the daemon has
    /// raised (or attempted to raise) and is tracking. Cleared when
    /// reconciliation (or a later ingest response) confirms the
    /// session has transitioned out of the needs-attention queue.
    ///
    /// Additive Codable field — defaults to [] for existing
    /// `state.json` files written before this field existed.
    public var pendingOrphanNotifications: [PendingOrphanNotification]
    /// Monotonic counter the orphan notification actor uses to
    /// ordering-compare pending entries across reconcile vs. consume
    /// races. Persisted so a daemon restart preserves ordering. The
    /// counter only ever increases; rollover is not a concern
    /// (UInt64 spans far beyond any realistic mutation volume).
    ///
    /// Additive Codable field — defaults to 0 for existing
    /// `state.json` files written before this field existed.
    public var notificationMutationSequence: UInt64

    public init(
        schemaVersion: Int = 1,
        hostID: UUID? = nil,
        lastHeartbeatAt: Date? = nil,
        lastKnownPiProtocolVersion: Int32? = nil,
        sources: [String: SourceState] = [:],
        pendingOrphanNotifications: [PendingOrphanNotification] = [],
        notificationMutationSequence: UInt64 = 0
    ) {
        self.schemaVersion = schemaVersion
        self.hostID = hostID
        self.lastHeartbeatAt = lastHeartbeatAt
        self.lastKnownPiProtocolVersion = lastKnownPiProtocolVersion
        self.sources = sources
        self.pendingOrphanNotifications = pendingOrphanNotifications
        self.notificationMutationSequence = notificationMutationSequence
    }

    private enum CodingKeys: String, CodingKey {
        case schemaVersion
        case hostID
        case lastHeartbeatAt
        case lastKnownPiProtocolVersion
        case sources
        case pendingOrphanNotifications
        case notificationMutationSequence
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        self.schemaVersion = try c.decode(Int.self, forKey: .schemaVersion)
        self.hostID = try c.decodeIfPresent(UUID.self, forKey: .hostID)
        self.lastHeartbeatAt = try c.decodeIfPresent(Date.self, forKey: .lastHeartbeatAt)
        self.lastKnownPiProtocolVersion = try c.decodeIfPresent(
            Int32.self, forKey: .lastKnownPiProtocolVersion)
        self.sources = try c.decodeIfPresent(
            [String: SourceState].self, forKey: .sources) ?? [:]
        self.pendingOrphanNotifications = try c.decodeIfPresent(
            [PendingOrphanNotification].self,
            forKey: .pendingOrphanNotifications) ?? []
        self.notificationMutationSequence = try c.decodeIfPresent(
            UInt64.self, forKey: .notificationMutationSequence) ?? 0
    }
}

/// One persisted entry in `DaemonState.pendingOrphanNotifications`.
/// Tracks an orphan or conflict notification the daemon has surfaced
/// (or attempted to surface) for a session that needs CRM attention.
///
/// `deliveryState` is tri-state. `"queued"` means the OS accepted the
/// `add(_:)` call. `"denied"` means user-notification authorization
/// was denied at add time, so the notification never reached the
/// user. `"failed"` means `add(_:)` threw a non-permission error.
/// Both `"denied"` and `"failed"` entries are RE-ATTEMPTED on the
/// next consume() / reconcile() call for the same (sessionUUID,
/// reason) — this prevents the permanent missed-notification trap
/// that would occur if we treated "in the pending list" as a hard
/// de-dup signal.
///
/// `mutationSequence` is a monotonic ordering token assigned on
/// every mutation. Replaces wall-clock comparison for the
/// reconcile-vs-consume race guard — the system clock can jump
/// backward; a sequence can't.
public struct PendingOrphanNotification: Codable, Equatable, Sendable {
    public let sessionUUID: String
    public let reason: String          // "orphan" | "conflict"
    public let notifiedAt: Date
    public var deliveryState: String   // "queued" | "denied" | "failed"
    public var mutationSequence: UInt64

    public init(
        sessionUUID: String,
        reason: String,
        notifiedAt: Date,
        deliveryState: String,
        mutationSequence: UInt64
    ) {
        self.sessionUUID = sessionUUID
        self.reason = reason
        self.notifiedAt = notifiedAt
        self.deliveryState = deliveryState
        self.mutationSequence = mutationSequence
    }

    private enum CodingKeys: String, CodingKey {
        case sessionUUID
        case reason
        case notifiedAt
        case deliveryState
        case mutationSequence
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        self.sessionUUID = try c.decode(String.self, forKey: .sessionUUID)
        self.reason = try c.decode(String.self, forKey: .reason)
        self.notifiedAt = try c.decode(Date.self, forKey: .notifiedAt)
        // Older entries (written before deliveryState landed) decode
        // as "queued" — the conservative default that assumes the
        // raise succeeded. The next reconcile will correct if it
        // didn't.
        self.deliveryState = try c.decodeIfPresent(
            String.self, forKey: .deliveryState) ?? "queued"
        // Older entries (without sequence numbers) decode as 0 —
        // they sort earliest, so any subsequent reconcile snapshot
        // will treat them as "present at snapshot time" and can
        // safely remove them.
        self.mutationSequence = try c.decodeIfPresent(
            UInt64.self, forKey: .mutationSequence) ?? 0
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
