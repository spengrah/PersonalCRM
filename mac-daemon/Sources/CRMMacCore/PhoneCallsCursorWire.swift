// PhoneCallsCursorWire — JSON-packed envelope of the Pi-side opaque
// cursor for the phone_calls source.
//
// Lives in CRMMacCore so both CRMMacPhoneCallsSource (which produces +
// consumes the cursor in the source plugin) and any future CLI ops
// support can share a single definition.
//
// The cursor primitive differs from MessagesCursorWire: CallHistoryDB's
// ZCALLRECORD table is keyed on a (ZDATE, Z_PK) pair, not a single
// monotonic ROWID. ZDATE is Apple-epoch SECONDS since 2001-01-01 (Core
// Data's CFAbsoluteTime convention), stored as a SQLite REAL — so
// fractional sub-second precision IS used by CallHistoryDB and must be
// preserved across cursor round-trip. Two calls can share a wall-clock
// second; the Z_PK tie-breaker prevents skipping a boundary row.
//
// Live iteration uses: WHERE (ZDATE > $1) OR (ZDATE = $1 AND Z_PK > $2)
// Backfill uses: WHERE (ZDATE < $1) OR (ZDATE = $1 AND Z_PK < $2)
//
// Encoding: ZDATE is encoded as a raw Double (seconds since the Apple
// epoch) rather than an ISO-8601 string so the sub-second part is
// preserved verbatim. Using ISO-8601 without fractional-seconds would
// truncate to whole seconds and cause backfill descent to skip rows
// and live descent to duplicate them across restart.
//
// Like MessagesCursorWire, this struct carries a pendingScans queue —
// operator-queued and auto-queued 30-day identifier-scoped backwards
// scans the next tick drains. The field is additive: cursor JSON
// written before it existed decodes the array as empty. The
// knownIdentifiersHash field is retained (dead) for restart-time change
// detection; the persisted per-source baseline in DaemonState
// supersedes its intended role.
import Foundation

public struct PhoneCallsCursorWire: Codable, Equatable, Sendable {
    /// ZDATE floor for backfill (Apple-epoch seconds, Double).
    /// Backfill walks DOWN from this paired with `backfillCursorZPK`.
    /// Nil before the install-time floor has been captured.
    public var backfillCursorZDate: Double?

    /// Z_PK tie-breaker paired with `backfillCursorZDate`. Nil iff
    /// `backfillCursorZDate` is nil.
    public var backfillCursorZPK: Int64?

    /// ZDATE at-or-below which live events have been emitted (Apple-
    /// epoch seconds, Double). Walks UP from `installMaxZDate`.
    public var liveCursorZDate: Double?

    /// Z_PK tie-breaker paired with `liveCursorZDate`.
    public var liveCursorZPK: Int64?

    /// Install-time MAX(ZDATE), captured lazily on the first tick.
    /// Apple-epoch seconds (Double).
    public var installMaxZDate: Double?

    /// Install-time Z_PK paired with `installMaxZDate`.
    public var installMaxZPK: Int64?

    /// 2026-01-01 floor (same constant as messages). Older
    /// CallHistoryDB rows are not emitted. Kept as a Date for parity
    /// with the messages cursor's wire shape (this column is
    /// human-debug-readable; sub-second precision is not meaningful
    /// for a chronological floor).
    public var backfillFloorSentAt: Date

    /// True once `backfillCursor*` has reached `backfillFloorSentAt`.
    public var backfillComplete: Bool

    /// SHA-256 (lowercase hex) of the sorted canonical known-
    /// identifiers set as of the last successful heartbeat-diff.
    /// Retained (dead) for the dead-field compatibility; the persisted
    /// per-source baseline in DaemonState supersedes its intended role.
    public var knownIdentifiersHash: String?

    /// Operator-queued AND auto-queued one-shot 30-day backwards scans
    /// the next tick should drain. Persisted so a `scan` subcommand or
    /// a newly-known-identifier scan survives a daemon restart. Capped
    /// at `pendingScansCap`; older entries dropped on overflow.
    public var pendingScans: [PhoneCallsCursorPendingScan]

    /// 2026-01-01T00:00:00Z — the spec-defined backfill floor.
    /// Matches MessagesCursorWire.defaultBackfillFloor by design;
    /// CallHistoryDB and chat.db share the same chronological floor.
    public static let defaultBackfillFloor = Date(timeIntervalSince1970: 1_767_225_600)

    /// Cap on pending scans persisted in the cursor JSON. Matches
    /// MessagesCursorWire.pendingScansCap.
    public static let pendingScansCap: Int = 256

    public init(
        backfillCursorZDate: Double? = nil,
        backfillCursorZPK: Int64? = nil,
        liveCursorZDate: Double? = nil,
        liveCursorZPK: Int64? = nil,
        installMaxZDate: Double? = nil,
        installMaxZPK: Int64? = nil,
        backfillFloorSentAt: Date,
        backfillComplete: Bool = false,
        pendingScans: [PhoneCallsCursorPendingScan] = [],
        knownIdentifiersHash: String? = nil
    ) {
        self.backfillCursorZDate = backfillCursorZDate
        self.backfillCursorZPK = backfillCursorZPK
        self.liveCursorZDate = liveCursorZDate
        self.liveCursorZPK = liveCursorZPK
        self.installMaxZDate = installMaxZDate
        self.installMaxZPK = installMaxZPK
        self.backfillFloorSentAt = backfillFloorSentAt
        self.backfillComplete = backfillComplete
        self.pendingScans = pendingScans
        self.knownIdentifiersHash = knownIdentifiersHash
    }

    enum CodingKeys: String, CodingKey {
        case backfillCursorZDate  = "backfill_cursor_zdate"
        case backfillCursorZPK    = "backfill_cursor_z_pk"
        case liveCursorZDate      = "live_cursor_zdate"
        case liveCursorZPK        = "live_cursor_z_pk"
        case installMaxZDate      = "install_max_zdate"
        case installMaxZPK        = "install_max_z_pk"
        case backfillFloorSentAt  = "backfill_floor_sent_at"
        case backfillComplete     = "backfill_complete"
        case pendingScans         = "pending_scans"
        case knownIdentifiersHash = "known_identifiers_hash"
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        self.backfillCursorZDate = try c.decodeIfPresent(Double.self, forKey: .backfillCursorZDate)
        self.backfillCursorZPK = try c.decodeIfPresent(Int64.self, forKey: .backfillCursorZPK)
        self.liveCursorZDate = try c.decodeIfPresent(Double.self, forKey: .liveCursorZDate)
        self.liveCursorZPK = try c.decodeIfPresent(Int64.self, forKey: .liveCursorZPK)
        self.installMaxZDate = try c.decodeIfPresent(Double.self, forKey: .installMaxZDate)
        self.installMaxZPK = try c.decodeIfPresent(Int64.self, forKey: .installMaxZPK)
        self.backfillFloorSentAt = try c.decode(Date.self, forKey: .backfillFloorSentAt)
        self.backfillComplete = try c.decodeIfPresent(Bool.self, forKey: .backfillComplete) ?? false
        // Additive: cursor JSON written before pendingScans existed
        // decodes the array as empty.
        self.pendingScans = try c.decodeIfPresent(
            [PhoneCallsCursorPendingScan].self, forKey: .pendingScans) ?? []
        self.knownIdentifiersHash = try c.decodeIfPresent(
            String.self, forKey: .knownIdentifiersHash)
    }
}

/// One queued targeted scan for the phone_calls source. Mirrors
/// MessagesCursorPendingScan but with the CallHistoryDB `(ZDATE, Z_PK)`
/// progress primitive instead of a single ROWID.
public struct PhoneCallsCursorPendingScan: Codable, Equatable, Sendable {
    /// Canonicalized phone/email handle to scan for. Already passed
    /// through HandleNormalization.canonicalize at queue time.
    public let normalizedHandle: String

    /// Lower bound for the scan window (CallHistoryDB ZDATE Apple-epoch
    /// seconds >= since).
    public let since: Date

    /// Resume coordinate (ZDATE component): the LOWEST `ZDATE` already
    /// confirmed-published for this scan. Paired with `progressBelowZPK`
    /// as a `(ZDATE, Z_PK)` lexicographic bound. Nil means "not
    /// started". Additive Codable field.
    public let progressBelowZDate: Double?

    /// Resume coordinate (Z_PK component) paired with
    /// `progressBelowZDate`. Nil iff `progressBelowZDate` is nil.
    public let progressBelowZPK: Int64?

    public init(
        normalizedHandle: String,
        since: Date,
        progressBelowZDate: Double? = nil,
        progressBelowZPK: Int64? = nil
    ) {
        self.normalizedHandle = normalizedHandle
        self.since = since
        self.progressBelowZDate = progressBelowZDate
        self.progressBelowZPK = progressBelowZPK
    }

    enum CodingKeys: String, CodingKey {
        case normalizedHandle  = "normalized_handle"
        case since
        case progressBelowZDate = "progress_below_zdate"
        case progressBelowZPK   = "progress_below_z_pk"
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        self.normalizedHandle = try c.decode(String.self, forKey: .normalizedHandle)
        self.since = try c.decode(Date.self, forKey: .since)
        self.progressBelowZDate = try c.decodeIfPresent(Double.self, forKey: .progressBelowZDate)
        self.progressBelowZPK = try c.decodeIfPresent(Int64.self, forKey: .progressBelowZPK)
    }
}

public enum PhoneCallsCursorWireCodec {
    /// JSON-encode a cursor for storage in the Pi-side opaque string.
    /// Uses ISO-8601 date encoding for the human-readable floor; ZDATE
    /// fields are raw Doubles so sub-second precision survives the
    /// round-trip.
    public static func encode(_ cursor: PhoneCallsCursorWire) throws -> String {
        let encoder = JSONEncoder()
        encoder.dateEncodingStrategy = .iso8601
        encoder.outputFormatting = [.sortedKeys, .withoutEscapingSlashes]
        let data = try encoder.encode(cursor)
        return String(decoding: data, as: UTF8.self)
    }

    /// JSON-decode a cursor from the Pi-side opaque string. An empty
    /// string returns nil — callers construct a fresh
    /// `PhoneCallsCursorWire` in that case.
    public static func decode(_ raw: String) throws -> PhoneCallsCursorWire? {
        if raw.isEmpty { return nil }
        let decoder = JSONDecoder()
        decoder.dateDecodingStrategy = .iso8601
        return try decoder.decode(PhoneCallsCursorWire.self, from: Data(raw.utf8))
    }
}
