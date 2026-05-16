// SourceHealthSnapshot — per-source health value the heartbeat body
// builder reads to populate the sourceHealth payload.
//
// Lives in CRMMacCore (Foundation only) so both HeartbeatLoop (in
// CRMMacLifecycle) and source plugins (e.g. MessagesSourcePlugin in
// CRMMacMessagesSource) can share without creating a target dependency
// cycle.
//
// Convention: plugins write to the registry after each tick (or on
// state transitions); the heartbeat reads on each tick and embeds the
// snapshot into the heartbeat body.
import Foundation

/// Health surface for a single source.  Mirrors the heartbeat JSON
/// keys consumed on the Pi side; encoded as part of the heartbeat
/// body via HeartbeatBodyBuilder.
public struct SourceHealthSnapshot: Codable, Equatable, Sendable {
    /// Whether the source is wired up + considered healthy.
    public var enabled: Bool

    /// Last time the scheduler invoked tick().
    public var lastScheduledAt: Date?

    /// Last time the source successfully published at least one event
    /// (or, for sources that just observe, completed a tick without
    /// error).
    public var lastPushedAt: Date?

    /// Source-defined cursor watermark observed at last poll. For
    /// messages this is `liveCursor` (chat.db message.ROWID).
    public var observedCursor: Int64?

    /// Source-defined cursor watermark last successfully committed
    /// Pi-side. May lag behind `observedCursor` if a batch was
    /// observed but not yet committed (e.g. mid-publish).
    public var pushedCursor: Int64?

    /// Schema version label (e.g. "chat_db_v1" or
    /// "chat_db_drift:message.guid" — see SchemaHealth.label in
    /// CRMMacMessagesSource).
    public var schemaVersion: String?

    /// For sources with a backfill cursor: whether backfill has
    /// completed walking back to its floor.
    public var backfillComplete: Bool?

    /// Last error encountered (free-form; expected values: "fda_required",
    /// "schema_drift:<col>", "auth_revoked", etc.).
    public var lastError: String?
    public var lastErrorAt: Date?

    public init(
        enabled: Bool = false,
        lastScheduledAt: Date? = nil,
        lastPushedAt: Date? = nil,
        observedCursor: Int64? = nil,
        pushedCursor: Int64? = nil,
        schemaVersion: String? = nil,
        backfillComplete: Bool? = nil,
        lastError: String? = nil,
        lastErrorAt: Date? = nil
    ) {
        self.enabled = enabled
        self.lastScheduledAt = lastScheduledAt
        self.lastPushedAt = lastPushedAt
        self.observedCursor = observedCursor
        self.pushedCursor = pushedCursor
        self.schemaVersion = schemaVersion
        self.backfillComplete = backfillComplete
        self.lastError = lastError
        self.lastErrorAt = lastErrorAt
    }

    enum CodingKeys: String, CodingKey {
        case enabled
        case lastScheduledAt = "last_scheduled_at"
        case lastPushedAt    = "last_pushed_at"
        case observedCursor  = "observed_cursor"
        case pushedCursor    = "pushed_cursor"
        case schemaVersion   = "schema_version"
        case backfillComplete = "backfill_complete"
        case lastError       = "last_error"
        case lastErrorAt     = "last_error_at"
    }
}

/// In-process registry that aggregates per-source snapshots. The
/// heartbeat body builder reads the latest snapshot for every
/// registered source.
public actor SourceHealthRegistry {
    private var snapshots: [SourceID: SourceHealthSnapshot] = [:]

    public init() {}

    /// Set / replace the snapshot for `id`.
    public func update(_ id: SourceID, _ snapshot: SourceHealthSnapshot) {
        snapshots[id] = snapshot
    }

    /// Read the current snapshot for `id`, or nil if no plugin has
    /// reported yet.
    public func read(_ id: SourceID) -> SourceHealthSnapshot? {
        snapshots[id]
    }

    /// Snapshot all known sources.  Heartbeat reads this on every tick.
    public func all() -> [SourceID: SourceHealthSnapshot] {
        snapshots
    }
}
