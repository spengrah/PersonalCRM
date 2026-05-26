// PhoneCallsCursorWire — JSON-packed envelope of the Pi-side opaque
// cursor for the phone_calls source (phase 1.5).
//
// Lives in CRMMacCore so both CRMMacPhoneCallsSource (which produces +
// consumes the cursor in the source plugin) and any future CLI ops
// support can share a single definition.
//
// The cursor primitive differs from MessagesCursorWire: CallHistoryDB's
// ZCALLRECORD table is keyed on a (ZDATE, Z_PK) pair, not a single
// monotonic ROWID. ZDATE is Apple-epoch SECONDS since 2001-01-01 (Core
// Data's CFAbsoluteTime convention). Two calls can share a wall-clock
// second; the Z_PK tie-breaker prevents skipping a boundary row.
//
// Live iteration uses: WHERE (ZDATE > $1) OR (ZDATE = $1 AND Z_PK > $2)
// Backfill uses: WHERE (ZDATE < $1) OR (ZDATE = $1 AND Z_PK < $2)
//
// Unlike MessagesCursorWire, this struct does NOT carry a pendingScans
// queue. v1.5 defers identifier-scoped scans to a follow-up (D-DEVIATION-1
// in the plan); the natural-backfill-descent + sender-filter posture
// matches the merged messages plugin's v1 simplification. The
// knownIdentifiersHash IS carried for restart-time change detection.
import Foundation

public struct PhoneCallsCursorWire: Codable, Equatable, Sendable {
    /// ZDATE floor for backfill (Apple-epoch seconds, encoded as Date).
    /// Backfill walks DOWN from this paired with `backfillCursorZPK`.
    /// Nil before the install-time floor has been captured.
    public var backfillCursorZDate: Date?

    /// Z_PK tie-breaker paired with `backfillCursorZDate`. Nil iff
    /// `backfillCursorZDate` is nil.
    public var backfillCursorZPK: Int64?

    /// ZDATE at-or-below which live events have been emitted. Walks UP
    /// from `installMaxZDate`.
    public var liveCursorZDate: Date?

    /// Z_PK tie-breaker paired with `liveCursorZDate`.
    public var liveCursorZPK: Int64?

    /// Install-time MAX(ZDATE), captured lazily on the first tick.
    public var installMaxZDate: Date?

    /// Install-time Z_PK paired with `installMaxZDate`.
    public var installMaxZPK: Int64?

    /// 2026-01-01 floor (same constant as messages). Older
    /// CallHistoryDB rows are not emitted.
    public var backfillFloorSentAt: Date

    /// True once `backfillCursor*` has reached `backfillFloorSentAt`.
    public var backfillComplete: Bool

    /// SHA-256 (lowercase hex) of the sorted canonical known-
    /// identifiers set as of the last successful heartbeat-diff.
    /// Used after restart to detect changes — equal hash means no
    /// work, different hash means a contact was added/removed
    /// offline. v1.5 does NOT auto-queue scans on diff
    /// (D-DEVIATION-1); the hash is recorded for future use.
    public var knownIdentifiersHash: String?

    /// 2026-01-01T00:00:00Z — the spec-defined backfill floor.
    /// Matches MessagesCursorWire.defaultBackfillFloor by design;
    /// CallHistoryDB and chat.db share the same chronological floor.
    public static let defaultBackfillFloor = Date(timeIntervalSince1970: 1_767_225_600)

    public init(
        backfillCursorZDate: Date? = nil,
        backfillCursorZPK: Int64? = nil,
        liveCursorZDate: Date? = nil,
        liveCursorZPK: Int64? = nil,
        installMaxZDate: Date? = nil,
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
    /// Uses ISO-8601 date encoding (without fractional seconds) to
    /// match the Pi-side JSON shape.
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
