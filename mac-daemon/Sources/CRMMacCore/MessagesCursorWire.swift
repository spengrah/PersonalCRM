// MessagesCursorWire — JSON-packed envelope of the Pi-side opaque
// cursor for the messages source.
//
// Lives in CRMMacCore so both CRMMacMessagesSource (which produces +
// consumes the cursor in the source plugin) and CRMMacLifecycle (which
// produces + consumes it in the CLI ops subcommands) can share a single
// definition. Avoids the drift risk of redeclaring the type in two
// targets.
//
// The Pi stores `SourceState.cursor` as an opaque TEXT column; the
// daemon owns its semantics. This struct is JSON-encoded into that
// column on every successful commit, and JSON-decoded on every read.
import Foundation

public struct MessagesCursorWire: Codable, Equatable, Sendable {
    /// chat.db `message.ROWID` floor below which backfill still has
    /// rows to walk. Nil before the install-time MAX(ROWID) has been
    /// captured (first tick); descends as backfill makes progress.
    public var backfillCursor: Int64?

    /// chat.db `message.ROWID` at-or-below which live events have been
    /// emitted. Walks UP from `installMaxRowID`.
    public var liveCursor: Int64?

    /// Install-time MAX(ROWID), captured lazily on the first tick when
    /// `liveCursor == nil`. Defines the boundary between backfill
    /// (below) and live (above).
    public var installMaxRowID: Int64?

    /// 2026-01-01 floor. Stored explicitly so the floor can be moved
    /// later without code-change risk.
    public var backfillFloorSentAt: Date

    /// True once `backfillCursor` has reached `backfillFloorSentAt`.
    public var backfillComplete: Bool

    /// Operator-queued one-shot 30-day backwards scans the next tick
    /// should drain. Persisted so the `scan` subcommand survives a
    /// daemon restart. Capped at `pendingScansCap`; older entries
    /// dropped on overflow.
    public var pendingScans: [MessagesCursorPendingScan]

    /// SHA-256 (lowercase hex) of the sorted canonical
    /// known-identifiers set as of the last successful heartbeat-diff.
    /// Used after restart to detect changes (equal hash = no work;
    /// different hash = a contact was added/removed offline). Fixed
    /// 64 hex chars when set. Nil before the first heartbeat.
    public var knownIdentifiersHash: String?

    /// One-time gate for the outbound re-backfill. Installs that
    /// advanced their cursors while inbound-only emission was in effect
    /// never emitted outbound rows; the first tick after the outbound
    /// branch ships resets the backfill walk once to re-cover the span
    /// down to the floor (re-emitted inbound rows dedup at the Pi; the
    /// outbound rows emit for the first time). Additive Codable: legacy
    /// cursors without the key decode as false. Set true after the reset
    /// so it happens exactly once.
    public var outboundBackfillDone: Bool

    /// Cap on pending scans persisted in the cursor JSON.
    public static let pendingScansCap: Int = 256

    /// 2026-01-01T00:00:00Z — the spec-defined backfill floor. Older
    /// chat.db rows are not emitted. Lives in CRMMacCore so every
    /// caller (source plugin, CLI ops, lifecycle tests) can reference
    /// it without pulling in the GRDB-bearing messages source target.
    public static let defaultBackfillFloor = Date(timeIntervalSince1970: 1_767_225_600)

    public init(
        backfillCursor: Int64? = nil,
        liveCursor: Int64? = nil,
        installMaxRowID: Int64? = nil,
        backfillFloorSentAt: Date,
        backfillComplete: Bool = false,
        pendingScans: [MessagesCursorPendingScan] = [],
        knownIdentifiersHash: String? = nil,
        outboundBackfillDone: Bool = false
    ) {
        self.backfillCursor = backfillCursor
        self.liveCursor = liveCursor
        self.installMaxRowID = installMaxRowID
        self.backfillFloorSentAt = backfillFloorSentAt
        self.backfillComplete = backfillComplete
        self.pendingScans = pendingScans
        self.knownIdentifiersHash = knownIdentifiersHash
        self.outboundBackfillDone = outboundBackfillDone
    }

    enum CodingKeys: String, CodingKey {
        case backfillCursor        = "backfill_cursor"
        case liveCursor            = "live_cursor"
        case installMaxRowID       = "install_max_rowid"
        case backfillFloorSentAt   = "backfill_floor_sent_at"
        case backfillComplete      = "backfill_complete"
        case pendingScans          = "pending_scans"
        case knownIdentifiersHash  = "known_identifiers_hash"
        case outboundBackfillDone  = "outbound_backfill_done"
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        self.backfillCursor = try c.decodeIfPresent(Int64.self, forKey: .backfillCursor)
        self.liveCursor = try c.decodeIfPresent(Int64.self, forKey: .liveCursor)
        self.installMaxRowID = try c.decodeIfPresent(Int64.self, forKey: .installMaxRowID)
        self.backfillFloorSentAt = try c.decode(Date.self, forKey: .backfillFloorSentAt)
        self.backfillComplete = try c.decode(Bool.self, forKey: .backfillComplete)
        self.pendingScans = try c.decodeIfPresent([MessagesCursorPendingScan].self, forKey: .pendingScans) ?? []
        self.knownIdentifiersHash = try c.decodeIfPresent(String.self, forKey: .knownIdentifiersHash)
        // Additive: legacy cursors lack the key and decode as false.
        self.outboundBackfillDone = try c.decodeIfPresent(Bool.self, forKey: .outboundBackfillDone) ?? false
    }
}

/// One queued targeted scan.  The next messages tick reads & drains
/// the queue at the top, before live/backfill batches.
public struct MessagesCursorPendingScan: Codable, Equatable, Sendable {
    /// Canonicalized phone/email handle the operator wants to scan
    /// for. Already passed through HandleNormalization.canonicalize
    /// at queue time.
    public let normalizedHandle: String

    /// Lower bound for the scan window (chat.db message.date >= since).
    public let since: Date

    /// Resume coordinate: the LOWEST chat.db `message.ROWID` already
    /// confirmed-published for this scan. The next page reads
    /// `... AND ROWID < progressBelowRowID` (descending walk). Nil
    /// means "not started" — scan from the most recent matching row.
    /// Additive Codable field; operator-CLI-queued and pre-existing
    /// entries decode as nil.
    public let progressBelowRowID: Int64?

    public init(normalizedHandle: String, since: Date, progressBelowRowID: Int64? = nil) {
        self.normalizedHandle = normalizedHandle
        self.since = since
        self.progressBelowRowID = progressBelowRowID
    }

    enum CodingKeys: String, CodingKey {
        case normalizedHandle  = "normalized_handle"
        case since
        case progressBelowRowID = "progress_below_rowid"
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        self.normalizedHandle = try c.decode(String.self, forKey: .normalizedHandle)
        self.since = try c.decode(Date.self, forKey: .since)
        self.progressBelowRowID = try c.decodeIfPresent(Int64.self, forKey: .progressBelowRowID)
    }
}

public enum MessagesCursorWireCodec {
    /// JSON-encode a cursor for storage in the Pi-side opaque string.
    /// Uses ISO-8601 date encoding (without fractional seconds) to
    /// match the Pi-side JSON shape.
    public static func encode(_ cursor: MessagesCursorWire) throws -> String {
        let encoder = JSONEncoder()
        encoder.dateEncodingStrategy = .iso8601
        encoder.outputFormatting = [.sortedKeys, .withoutEscapingSlashes]
        let data = try encoder.encode(cursor)
        return String(decoding: data, as: UTF8.self)
    }

    /// JSON-decode a cursor from the Pi-side opaque string. An empty
    /// string (fresh-install case) returns nil — callers construct a
    /// fresh `MessagesCursorWire` in that case.
    public static func decode(_ raw: String) throws -> MessagesCursorWire? {
        if raw.isEmpty { return nil }
        let decoder = JSONDecoder()
        decoder.dateDecodingStrategy = .iso8601
        return try decoder.decode(MessagesCursorWire.self, from: Data(raw.utf8))
    }
}
