// CallHistoryDBReader — GRDB-backed read-only iterator over
// CallHistoryDB ZCALLRECORD rows.
//
// Opens CallHistoryDB with `?mode=ro&immutable=1`.
// Despite Phone/FaceTime occasionally writing to the DB, immutable=1 is
// acceptable because (a) the daemon re-opens the DB each tick (~60-90s
// cadence) so no long-lived handle exists to see stale state, and (b)
// SQLITE_BUSY retries via GRDB's default DatabasePool busy-retry
// policy handle transient writer-lock contention.
//
// Cursor primitive: (ZDATE, Z_PK) lexicographic pair. ZDATE is
// Apple-epoch SECONDS since 2001-01-01 (Core Data's CFAbsoluteTime
// convention, NOT chat.db's nanoseconds). Z_PK is the row's monotonic
// primary key; together they make a unique tie-broken cursor.
//
// Live iteration:    WHERE (ZDATE > $1) OR (ZDATE = $1 AND Z_PK > $2)
// Backfill descent:  WHERE (ZDATE < $1) OR (ZDATE = $1 AND Z_PK < $2)
import Foundation
import GRDB

/// One CallHistoryDB row, shaped for downstream payload construction.
public struct CallHistoryRow: Equatable, Sendable {
    public let zPK: Int64
    public let uniqueID: String
    public let address: String?  // raw ZADDRESS; nil/empty rows are skipped
    public let originated: Bool  // ZORIGINATED == 1 -> outbound
    public let answered: Bool?   // ZANSWERED nullable; valid only for inbound
    public let duration: Int32   // ZDURATION (seconds; 0 for missed)
    public let serviceProvider: String?
    public let callType: Int64?
    public let hasMessage: Bool  // ZHASMESSAGE == 1 -> voicemail (inbound only)
    public let startedAt: Date   // ZDATE converted to UTC

    public init(
        zPK: Int64,
        uniqueID: String,
        address: String?,
        originated: Bool,
        answered: Bool?,
        duration: Int32,
        serviceProvider: String?,
        callType: Int64?,
        hasMessage: Bool,
        startedAt: Date
    ) {
        self.zPK = zPK
        self.uniqueID = uniqueID
        self.address = address
        self.originated = originated
        self.answered = answered
        self.duration = duration
        self.serviceProvider = serviceProvider
        self.callType = callType
        self.hasMessage = hasMessage
        self.startedAt = startedAt
    }
}

/// Direction of an iteration query.
public enum CallReadDirection: Sendable {
    /// (ZDATE > floor.zdate) OR (ZDATE = floor.zdate AND Z_PK > floor.z_pk)
    case forwardFromExclusive(zdate: Double, zPK: Int64)
    /// (ZDATE < ceil.zdate) OR (ZDATE = ceil.zdate AND Z_PK < ceil.z_pk)
    case backwardFromExclusive(zdate: Double, zPK: Int64)
}

/// One row's (ZDATE, Z_PK) coordinate. Used for cursor advance bounds.
public struct CallCursorPoint: Equatable, Sendable {
    public let zdate: Double
    public let zPK: Int64
    public init(zdate: Double, zPK: Int64) {
        self.zdate = zdate
        self.zPK = zPK
    }
}

/// Result of a fetch() call.
///
/// `rows`: only the rows we kept after skip filters (corrupt date,
/// empty address, schema-unmappable service).
///
/// `scannedBounds`: min and max of EVERY row inspected, including
/// skipped ones. Used by the caller to advance cursors past skipped
/// rows so a page of all-rejected rows doesn't stall the iterator.
///
/// `serviceUnknownCount`: tally of rows whose service couldn't be
/// resolved via ServiceDerivation. Surfaces as a telemetry counter on
/// the plugin (T-Swift-8).
///
/// `exhausted`: true if SQL returned fewer than `limit` rows.
public struct CallHistoryReadPage: Sendable {
    public let rows: [CallHistoryRow]
    public let scannedBounds: (min: CallCursorPoint, max: CallCursorPoint)?
    public let serviceUnknownCount: Int
    public let exhausted: Bool

    public init(
        rows: [CallHistoryRow],
        scannedBounds: (min: CallCursorPoint, max: CallCursorPoint)?,
        serviceUnknownCount: Int,
        exhausted: Bool
    ) {
        self.rows = rows
        self.scannedBounds = scannedBounds
        self.serviceUnknownCount = serviceUnknownCount
        self.exhausted = exhausted
    }
}

public enum CallHistoryDBReader {
    /// Apple-epoch offset in seconds (1970-01-01 -> 2001-01-01).
    public static let appleEpochOffset: TimeInterval = 978_307_200

    /// Sentinel below which ZDATE values are treated as corrupt and
    /// skipped (with a warning). ~500_000_000 seconds since the Apple
    /// epoch puts the floor at roughly 2016-11; CallHistoryDB rows
    /// before that horizon are very rare in real data and almost always
    /// corruption.
    public static let dateSentinelFloor: Double = 500_000_000

    /// max ZDATE (and the corresponding Z_PK at that ZDATE) in the
    /// table.  Returns nil if the table is empty.
    public static func maxZDate(db: Database) throws -> CallCursorPoint? {
        // Resolve in two steps so the tie-breaker is deterministic when
        // multiple rows share the same MAX(ZDATE).
        let row = try Row.fetchOne(
            db,
            sql: "SELECT ZDATE AS zd, MAX(Z_PK) AS pk FROM ZCALLRECORD WHERE ZDATE = (SELECT MAX(ZDATE) FROM ZCALLRECORD)")
        guard let zd = row?["zd"] as Double?,
              let pk = row?["pk"] as Int64? else {
            return nil
        }
        return CallCursorPoint(zdate: zd, zPK: pk)
    }

    /// Fetch up to `limit` rows in `direction` order. The reader applies
    /// every per-row skip filter (empty ZADDRESS, corrupt ZDATE,
    /// unmappable service) and reports a service_unknown counter.
    public static func fetchPage(
        db: Database,
        direction: CallReadDirection,
        limit: Int
    ) throws -> CallHistoryReadPage {
        precondition(limit > 0, "limit must be > 0")

        let whereClause: String
        let order: String
        let zdateBound: Double
        let zpkBound: Int64
        switch direction {
        case .forwardFromExclusive(let zdate, let zPK):
            whereClause = "(ZDATE > ?) OR (ZDATE = ? AND Z_PK > ?)"
            order = "ZDATE ASC, Z_PK ASC"
            zdateBound = zdate
            zpkBound = zPK
        case .backwardFromExclusive(let zdate, let zPK):
            whereClause = "(ZDATE < ?) OR (ZDATE = ? AND Z_PK < ?)"
            order = "ZDATE DESC, Z_PK DESC"
            zdateBound = zdate
            zpkBound = zPK
        }

        let sql = """
            SELECT
                Z_PK              AS z_pk,
                ZUNIQUE_ID        AS unique_id,
                ZADDRESS          AS address,
                ZORIGINATED       AS originated,
                ZANSWERED         AS answered,
                ZDURATION         AS duration,
                ZSERVICE_PROVIDER AS service_provider,
                ZCALLTYPE         AS call_type,
                ZHASMESSAGE       AS has_message,
                ZDATE             AS zdate
            FROM ZCALLRECORD
            WHERE \(whereClause)
            ORDER BY \(order)
            LIMIT ?
            """

        let rawRows = try Row.fetchAll(db, sql: sql,
                                       arguments: [zdateBound, zdateBound, zpkBound, limit])

        var kept: [CallHistoryRow] = []
        kept.reserveCapacity(rawRows.count)
        var serviceUnknown = 0
        var scannedMin: CallCursorPoint?
        var scannedMax: CallCursorPoint?

        for row in rawRows {
            guard let zPK: Int64 = row["z_pk"],
                  let zdate: Double = row["zdate"] else {
                continue
            }
            // Track every (ZDATE, Z_PK) we inspected, even rows we
            // ultimately skip. The caller advances the cursor past
            // skipped rows so all-rejected pages don't stall.
            let point = CallCursorPoint(zdate: zdate, zPK: zPK)
            if scannedMin == nil { scannedMin = point }
            if scannedMax == nil { scannedMax = point }
            scannedMin = lex(scannedMin!, point, choose: .min)
            scannedMax = lex(scannedMax!, point, choose: .max)

            // Corrupt date sentinel: zdate <= 0 or below the
            // ~2016-Nov floor. These rows are very rare in real data
            // and almost always indicate corruption.
            if zdate <= 0 || zdate < Self.dateSentinelFloor {
                continue
            }

            guard let uniqueID: String = row["unique_id"], !uniqueID.isEmpty else {
                continue
            }

            let address: String? = row["address"]
            let trimmedAddress = address?.trimmingCharacters(in: .whitespacesAndNewlines)
            if trimmedAddress == nil || trimmedAddress!.isEmpty {
                // Missing/empty address: skip — no peer to attribute to.
                continue
            }

            let originatedRaw: Int64 = (row["originated"] as Int64?) ?? 0
            let originated = originatedRaw != 0

            let answeredColumn: Int64? = row["answered"]
            let answered: Bool? = {
                if originated {
                    // Outbound: ZANSWERED is unreliable; force NULL.
                    return nil
                }
                guard let v = answeredColumn else { return false }
                return v != 0
            }()

            let durationRaw: Double = (row["duration"] as Double?) ?? 0
            let duration = Int32(max(0, durationRaw.rounded()))

            let provider: String? = row["service_provider"]
            let callType: Int64? = row["call_type"]
            // Service derivation is purely a row-level concern; we run
            // it here so the reader can count unknown-service rows and
            // skip them before payload shaping touches them.
            if ServiceDerivation.resolve(provider: provider, callType: callType) == nil {
                serviceUnknown += 1
                continue
            }

            let hasMessageRaw: Int64 = (row["has_message"] as Int64?) ?? 0
            // ZHASMESSAGE is inbound-only on iOS/macOS (the column is
            // reused for outbound greetings, which we don't surface).
            // Force false outbound regardless of source data.
            let hasMessage = !originated && hasMessageRaw != 0

            let startedAt = Date(timeIntervalSince1970: Self.appleEpochOffset + zdate)

            kept.append(CallHistoryRow(
                zPK: zPK,
                uniqueID: uniqueID,
                address: trimmedAddress,
                originated: originated,
                answered: answered,
                duration: duration,
                serviceProvider: provider,
                callType: callType,
                hasMessage: hasMessage,
                startedAt: startedAt))
        }

        let bounds: (min: CallCursorPoint, max: CallCursorPoint)?
        if let lo = scannedMin, let hi = scannedMax {
            bounds = (min: lo, max: hi)
        } else {
            bounds = nil
        }
        return CallHistoryReadPage(
            rows: kept,
            scannedBounds: bounds,
            serviceUnknownCount: serviceUnknown,
            exhausted: rawRows.count < limit)
    }

    // MARK: - lex compare helpers

    private enum LexChoice { case min, max }
    private static func lex(_ a: CallCursorPoint, _ b: CallCursorPoint,
                             choose: LexChoice) -> CallCursorPoint {
        let cmp: Bool
        if a.zdate != b.zdate {
            cmp = a.zdate < b.zdate
        } else {
            cmp = a.zPK < b.zPK
        }
        switch choose {
        case .min: return cmp ? a : b
        case .max: return cmp ? b : a
        }
    }
}
