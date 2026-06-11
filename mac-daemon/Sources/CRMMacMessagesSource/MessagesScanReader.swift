// MessagesScanReader — identifier-scoped, date-bounded, resumable scan
// over chat.db `message` rows for a single canonical handle.
//
// chat.db has NO index on `handle.id` or `message.date`, so a naive
// `WHERE handle.id = ?` scan would full-table-scan AND miss alternate
// spellings of the same canonical handle (one canonical phone number can
// map to several `handle.ROWID`s: "+15550000001", "(555) 000-0001", …).
//
// The scan therefore runs in two passes:
//
//   1. Resolve handle ROWIDs. `SELECT ROWID, id FROM handle` is small
//      (one row per distinct raw handle the mailbox has ever seen),
//      canonicalize each `id` in Swift, and keep the ROWIDs whose
//      canonical form equals the target. This canonicalization CANNOT
//      happen in SQL — it is the same `HandleNormalization` the sender
//      filter uses.
//
//   2. Scan messages. `message.handle_id IN (resolvedROWIDs) AND
//      message.date >= sinceNanos [AND message.ROWID < progressBelowRowID]`
//      ordered `ROWID DESC`, budget-limited. The `message` table has a
//      PRIMARY KEY on ROWID; the `handle_id IN (...)` predicate over a
//      tiny ROWID set plus the bounded 30-day `date` range, paged by
//      budget, keeps each tick's work bounded.
//
// The scan is RESUMABLE: passing `progressBelowRowID` walks the next
// page strictly below the lowest ROWID already confirmed-published, so a
// scan whose result set exceeds one tick's budget completes across ticks
// without dropping rows.
import Foundation
import GRDB

/// Result of one scan page.
public struct MessagesScanPage: Equatable, Sendable {
    /// The kept rows (skip filters already applied), ROWID descending.
    public let rows: [ChatDBMessage]
    /// True if SQL returned fewer than `limit` rows — i.e. the scan has
    /// no more matching rows below this page (exhausted).
    public let exhausted: Bool
    /// Lowest `message.ROWID` inspected in this page (including skipped
    /// rows), or nil if SQL returned zero rows. The caller advances the
    /// scan's progress to this so the next page resumes strictly below
    /// it. Tracking EVERY inspected ROWID (not just kept) means a page
    /// of all-skipped rows still advances and doesn't stall.
    public let lowestRowID: Int64?
    /// Count of SQL rows returned for this page, before skip filters.
    /// The plugin consumes its scan budget on this so a page of
    /// all-skipped rows still costs budget for the SQL work it did.
    public let inspected: Int

    public init(rows: [ChatDBMessage], exhausted: Bool, lowestRowID: Int64?, inspected: Int) {
        self.rows = rows
        self.exhausted = exhausted
        self.lowestRowID = lowestRowID
        self.inspected = inspected
    }
}

public enum MessagesScanReader {
    /// Resolve the set of `handle.ROWID`s whose canonical form equals
    /// `canonicalHandle`. One canonical handle can map to several raw
    /// `handle.id` spellings, so all matching ROWIDs are returned.
    public static func resolveHandleROWIDs(
        db: Database,
        canonicalHandle: String
    ) throws -> [Int64] {
        let rows = try Row.fetchAll(db, sql: "SELECT ROWID AS rowid, id AS hid FROM handle")
        var matches: [Int64] = []
        for row in rows {
            guard let rowID: Int64 = row["rowid"],
                  let rawID: String = row["hid"] else {
                continue
            }
            if HandleNormalization.canonicalize(rawID) == canonicalHandle {
                matches.append(rowID)
            }
        }
        return matches
    }

    /// Fetch one budget-limited page of messages for `canonicalHandle`
    /// at-or-after `since`, descending by ROWID, resuming strictly below
    /// `progressBelowRowID` when set.
    ///
    /// Returns an EMPTY exhausted page (no rows, lowestRowID nil) when
    /// the handle resolves to zero ROWIDs (no chat.db history for it).
    public static func scanPage(
        db: Database,
        canonicalHandle: String,
        since: Date,
        progressBelowRowID: Int64?,
        limit: Int
    ) throws -> MessagesScanPage {
        precondition(limit > 0, "limit must be > 0")

        let handleROWIDs = try resolveHandleROWIDs(db: db, canonicalHandle: canonicalHandle)
        if handleROWIDs.isEmpty {
            return MessagesScanPage(rows: [], exhausted: true, lowestRowID: nil, inspected: 0)
        }

        // Convert the Date lower bound to chat.db's Apple-epoch
        // NANOSECONDS unit.
        let sinceNanos = Int64(
            (since.timeIntervalSince1970 - ChatDBReader.appleEpochOffset) * 1e9)

        // Build the `handle_id IN (?, ?, …)` placeholder list.
        let placeholders = Array(repeating: "?", count: handleROWIDs.count).joined(separator: ", ")
        var conditions = [
            "message.handle_id IN (\(placeholders))",
            "message.date >= ?",
        ]
        var arguments: [DatabaseValueConvertible] = handleROWIDs
        arguments.append(sinceNanos)
        if let progress = progressBelowRowID {
            conditions.append("message.ROWID < ?")
            arguments.append(progress)
        }
        arguments.append(limit)

        let sql = """
            \(ChatDBReader.selectColumnsAndJoins)
            WHERE \(conditions.joined(separator: " AND "))
            ORDER BY message.ROWID DESC
            LIMIT ?
            """

        let rows = try Row.fetchAll(db, sql: sql, arguments: StatementArguments(arguments))

        var kept: [ChatDBMessage] = []
        kept.reserveCapacity(rows.count)
        var lowest: Int64?
        for row in rows {
            if let rowID: Int64 = row["msg_rowid"] {
                lowest = min(lowest ?? rowID, rowID)
            }
            if let mapped = ChatDBReader.mapMessageRow(row) {
                kept.append(mapped)
            }
        }
        return MessagesScanPage(
            rows: kept,
            exhausted: rows.count < limit,
            lowestRowID: lowest,
            inspected: rows.count)
    }
}
