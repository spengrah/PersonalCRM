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
// Unlike MessagesCursorWire, this struct does NOT carry a pendingScans
// queue. The 30-day identifier-scoped scan is deferred to a follow-up;
// the natural-backfill-descent + sender-filter posture matches the
// merged messages plugin's behavior. The knownIdentifiersHash IS
// carried for restart-time change detection.
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
    /// Used after restart to detect changes; v1.5 does not consume
    /// this on diff (recorded for future use).
    public var knownIdentifiersHash: String?

    /// 2026-01-01T00:00:00Z — the spec-defined backfill floor.
    /// Matches MessagesCursorWire.defaultBackfillFloor by design;
    /// CallHistoryDB and chat.db share the same chronological floor.
    public static let defaultBackfillFloor = Date(timeIntervalSince1970: 1_767_225_600)

    public init(
        backfillCursorZDate: Double? = nil,
        backfillCursorZPK: Int64? = nil,
        liveCursorZDate: Double? = nil,
        liveCursorZPK: Int64? = nil,
        installMaxZDate: Double? = nil,
        installMaxZPK: Int64? = nil,
        backfillFloorSentAt: Date,
        backfillComplete: Bool = false,
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
        case knownIdentifiersHash = "known_identifiers_hash"
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
