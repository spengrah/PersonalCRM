// CallHistoryScanReader — identifier-scoped, date-bounded, resumable
// scan over CallHistoryDB ZCALLRECORD rows for a single canonical
// handle.
//
// Unlike chat.db, CallHistoryDB indexes ZADDRESS, so a handle-scoped
// scan is index-assisted. But ZADDRESS is raw/uncanonicalized — one
// canonical handle (e.g. "+15550000001") can be stored under several
// raw spellings ("+15550000001", "(555) 000-0001", …). A naive
// `WHERE ZADDRESS = ?` would miss alternate spellings AND can't
// canonicalize inside SQL.
//
// The scan therefore runs in two passes:
//
//   1. Resolve matching raw addresses. `SELECT DISTINCT ZADDRESS FROM
//      ZCALLRECORD` is bounded (one row per distinct raw address seen),
//      canonicalize each in Swift, and keep the raw values whose
//      canonical form equals the target. This is the same
//      canonicalizer the sender filter uses; it CANNOT run in SQL.
//
//   2. Scan calls. `ZADDRESS IN (resolved) AND ZDATE >= sinceSeconds
//      [AND (ZDATE, Z_PK) < progress]` ordered `(ZDATE, Z_PK) DESC`,
//      budget-limited. The ZADDRESS index keeps each page cheap.
//
// The scan is RESUMABLE: passing the `(progressBelowZDate,
// progressBelowZPK)` pair walks the next page strictly below the lowest
// `(ZDATE, Z_PK)` already confirmed-published, so a scan whose result
// set exceeds one tick's budget completes across ticks without dropping
// rows.
//
// ZDATE is Apple-epoch SECONDS (Double), not chat.db's nanoseconds —
// the `since` Date lower bound is converted to that unit here.
import Foundation
import GRDB

/// Result of one phone-scan page.
public struct CallHistoryScanPage: Equatable, Sendable {
    /// The kept rows (skip filters already applied), (ZDATE, Z_PK)
    /// descending.
    public let rows: [CallHistoryRow]
    /// True if SQL returned fewer than `limit` rows — i.e. the scan has
    /// no more matching rows below this page (exhausted).
    public let exhausted: Bool
    /// Lowest (ZDATE, Z_PK) inspected in this page (including skipped
    /// rows), or nil if SQL returned zero rows. The caller advances the
    /// scan's progress to this so the next page resumes strictly below
    /// it. Tracking EVERY inspected coordinate (not just kept) means a
    /// page of all-skipped rows still advances and doesn't stall.
    public let lowestPoint: CallCursorPoint?

    public init(rows: [CallHistoryRow], exhausted: Bool, lowestPoint: CallCursorPoint?) {
        self.rows = rows
        self.exhausted = exhausted
        self.lowestPoint = lowestPoint
    }
}

public enum CallHistoryScanReader {
    /// Resolve the set of distinct raw ZADDRESS values whose canonical
    /// form equals `canonicalHandle`. One canonical handle can map to
    /// several raw spellings, so all matching raw values are returned.
    /// `canonicalizer` is the same normalizer the sender filter uses.
    public static func resolveAddresses(
        db: Database,
        canonicalHandle: String,
        canonicalizer: (String) -> String
    ) throws -> [String] {
        let rows = try Row.fetchAll(
            db, sql: "SELECT DISTINCT ZADDRESS AS addr FROM ZCALLRECORD WHERE ZADDRESS IS NOT NULL")
        var matches: [String] = []
        for row in rows {
            guard let raw: String = row["addr"] else { continue }
            let trimmed = raw.trimmingCharacters(in: .whitespacesAndNewlines)
            if trimmed.isEmpty { continue }
            if canonicalizer(trimmed) == canonicalHandle {
                matches.append(raw)
            }
        }
        return matches
    }

    /// Fetch one budget-limited page of calls for `canonicalHandle`
    /// at-or-after `since`, descending by (ZDATE, Z_PK), resuming
    /// strictly below `(progressBelowZDate, progressBelowZPK)` when set.
    ///
    /// Returns an EMPTY exhausted page (no rows, lowestPoint nil) when
    /// the handle resolves to zero raw addresses (no call history for
    /// it).
    public static func scanPage(
        db: Database,
        canonicalHandle: String,
        canonicalizer: (String) -> String,
        since: Date,
        progressBelowZDate: Double?,
        progressBelowZPK: Int64?,
        limit: Int
    ) throws -> CallHistoryScanPage {
        precondition(limit > 0, "limit must be > 0")

        let addresses = try resolveAddresses(
            db: db, canonicalHandle: canonicalHandle, canonicalizer: canonicalizer)
        if addresses.isEmpty {
            return CallHistoryScanPage(rows: [], exhausted: true, lowestPoint: nil)
        }

        // Convert the Date lower bound to CallHistoryDB's Apple-epoch
        // SECONDS unit.
        let sinceSeconds = since.timeIntervalSince1970 - CallHistoryDBReader.appleEpochOffset

        let placeholders = Array(repeating: "?", count: addresses.count).joined(separator: ", ")
        var conditions = [
            "ZADDRESS IN (\(placeholders))",
            "ZDATE >= ?",
        ]
        var arguments: [DatabaseValueConvertible] = addresses
        arguments.append(sinceSeconds)
        // Resume bound: strictly below the lowest (ZDATE, Z_PK) already
        // published. The pair must be supplied together.
        if let pZDate = progressBelowZDate, let pZPK = progressBelowZPK {
            conditions.append("((ZDATE < ?) OR (ZDATE = ? AND Z_PK < ?))")
            arguments.append(pZDate)
            arguments.append(pZDate)
            arguments.append(pZPK)
        }
        arguments.append(Int64(limit))

        let sql = """
            \(CallHistoryDBReader.selectColumns)
            WHERE \(conditions.joined(separator: " AND "))
            ORDER BY ZDATE DESC, Z_PK DESC
            LIMIT ?
            """

        let rawRows = try Row.fetchAll(db, sql: sql, arguments: StatementArguments(arguments))

        var kept: [CallHistoryRow] = []
        kept.reserveCapacity(rawRows.count)
        var lowest: CallCursorPoint?
        for row in rawRows {
            switch CallHistoryDBReader.mapRow(row) {
            case .malformed:
                continue
            case .skipped(let point, _):
                lowest = CallHistoryDBReader.lexExtend(lowest, point, choose: .min)
            case .kept(let mapped, let point):
                lowest = CallHistoryDBReader.lexExtend(lowest, point, choose: .min)
                kept.append(mapped)
            }
        }
        return CallHistoryScanPage(
            rows: kept,
            exhausted: rawRows.count < limit,
            lowestPoint: lowest)
    }
}
