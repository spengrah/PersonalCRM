// MessagesCursor — JSON-packed payload of the Pi-side opaque cursor
// string for the messages source.
//
// The Pi stores `SourceState.cursor` as an opaque TEXT column; the
// daemon owns its semantics. We JSON-encode this struct into that
// column on every successful commit, and JSON-decode on every read.
//
// Local `state.json` mirrors the Pi-side cursor as a write-through
// cache for fast restart — but the Pi is the source of truth. See
// `.ai/log/plan/mac-daemon-phase-1-pr7-messages-source.md` §"Pi cursor
// protocol" for the CAS commit flow and 409 handling.
import Foundation

/// Plan §R1.  Daemon-private envelope persisted inside the Pi-side
/// opaque cursor.
public struct MessagesCursor: Codable, Equatable, Sendable {
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
    /// should drain. Persisted (see plan §R9) so `scan` subcommand
    /// survives a daemon restart. Capped at `pendingScansCap`; older
    /// entries dropped on overflow.
    public var pendingScans: [PendingScan]

    /// SHA-256 (lowercase hex) of the sorted canonical
    /// known-identifiers set as of the last successful heartbeat-diff.
    /// Used after restart to detect changes (equal hash = no work;
    /// different hash = a contact was added/removed offline -> log
    /// info, do NOT auto-queue scans per plan §R9 trade-off). Fixed
    /// 64 hex chars when set. Nil before the first heartbeat.
    public var knownIdentifiersHash: String?

    /// Cap on pending scans persisted in the cursor JSON.
    public static let pendingScansCap: Int = 256

    public init(
        backfillCursor: Int64? = nil,
        liveCursor: Int64? = nil,
        installMaxRowID: Int64? = nil,
        backfillFloorSentAt: Date,
        backfillComplete: Bool = false,
        pendingScans: [PendingScan] = [],
        knownIdentifiersHash: String? = nil
    ) {
        self.backfillCursor = backfillCursor
        self.liveCursor = liveCursor
        self.installMaxRowID = installMaxRowID
        self.backfillFloorSentAt = backfillFloorSentAt
        self.backfillComplete = backfillComplete
        self.pendingScans = pendingScans
        self.knownIdentifiersHash = knownIdentifiersHash
    }

    enum CodingKeys: String, CodingKey {
        case backfillCursor       = "backfill_cursor"
        case liveCursor           = "live_cursor"
        case installMaxRowID      = "install_max_rowid"
        case backfillFloorSentAt  = "backfill_floor_sent_at"
        case backfillComplete     = "backfill_complete"
        case pendingScans         = "pending_scans"
        case knownIdentifiersHash = "known_identifiers_hash"
    }
}

/// One queued targeted scan.  The next messages tick reads & drains
/// the queue at the top, before live/backfill batches.
public struct PendingScan: Codable, Equatable, Sendable {
    /// Canonicalized phone/email handle the operator wants to scan
    /// for. Already passed through HandleNormalization.canonicalize
    /// at queue time.
    public let normalizedHandle: String

    /// Lower bound for the scan window (chat.db message.date >= since).
    public let since: Date

    public init(normalizedHandle: String, since: Date) {
        self.normalizedHandle = normalizedHandle
        self.since = since
    }

    enum CodingKeys: String, CodingKey {
        case normalizedHandle = "normalized_handle"
        case since
    }
}

public enum MessagesCursorCodec {
    /// JSON-encode a cursor for storage in the Pi-side opaque string.
    /// Uses ISO-8601 date encoding (without fractional seconds) to
    /// match the Pi-side JSON shape.
    public static func encode(_ cursor: MessagesCursor) throws -> String {
        let encoder = JSONEncoder()
        encoder.dateEncodingStrategy = .iso8601
        encoder.outputFormatting = [.sortedKeys, .withoutEscapingSlashes]
        let data = try encoder.encode(cursor)
        return String(decoding: data, as: UTF8.self)
    }

    /// JSON-decode a cursor from the Pi-side opaque string. An empty
    /// string (fresh-install case) returns nil — callers construct a
    /// fresh `MessagesCursor` in that case.
    public static func decode(_ raw: String) throws -> MessagesCursor? {
        if raw.isEmpty { return nil }
        let decoder = JSONDecoder()
        decoder.dateDecodingStrategy = .iso8601
        return try decoder.decode(MessagesCursor.self, from: Data(raw.utf8))
    }
}
