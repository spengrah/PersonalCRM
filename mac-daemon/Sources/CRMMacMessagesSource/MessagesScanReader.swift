// MessagesScanReader — identifier-scoped, date-bounded, resumable scan
// over chat.db `message` rows for a single canonical handle.
//
// chat.db has NO index on `handle.id` or `message.date`, so a naive
// `WHERE handle.id = ?` scan would full-table-scan AND miss alternate
// spellings of the same canonical handle (one canonical phone number can
// map to several `handle.ROWID`s: "+15550000001", "(555) 000-0001", …).
//
// The scan therefore runs in three passes:
//
//   1. Resolve handle ROWIDs. `SELECT ROWID, id FROM handle` is small
//      (one row per distinct raw handle the mailbox has ever seen),
//      canonicalize each `id` in Swift, and keep the ROWIDs whose
//      canonical form equals the target. This canonicalization CANNOT
//      happen in SQL — it is the same `HandleNormalization` the sender
//      filter uses.
//
//   2. Resolve the handle's chat memberships. `SELECT DISTINCT chat_id
//      FROM chat_handle_join WHERE handle_id IN (resolvedROWIDs)` — a
//      tiny table (one row per chat membership). The resulting chat-ROWID
//      set is what lets the scan reach OUTBOUND rows: an outbound row has
//      a NULL handle_id, so it can only be tied to the scanned handle via
//      the chats that handle belongs to.
//
//   3. Scan messages. Inbound rows match `message.handle_id IN
//      (resolvedROWIDs)`; outbound rows match `message.is_from_me = 1 AND
//      message.ROWID IN (chat_message_join rows of the resolved chats)`.
//      Bounded by `message.date >= sinceNanos [AND message.ROWID <
//      progressBelowRowID]`, ordered `ROWID DESC`, budget-limited. The
//      outbound branch is omitted entirely when the chat set is empty
//      (query identical to the inbound-only form). The outbound IN
//      subquery materializes the message-ID set of the handle's chats
//      once per page — same complexity class as the page's existing
//      ROWID-DESC walk, and budget-bounded per tick.
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

    /// Resolve the distinct `chat.ROWID`s that any of `handleROWIDs`
    /// belongs to, via `chat_handle_join`. Used to reach OUTBOUND rows
    /// (NULL handle_id) sent in the scanned handle's conversations.
    /// Returns an empty array if the handle has no chat memberships.
    public static func resolveChatROWIDs(
        db: Database,
        handleROWIDs: [Int64]
    ) throws -> [Int64] {
        if handleROWIDs.isEmpty { return [] }
        let placeholders = Array(repeating: "?", count: handleROWIDs.count).joined(separator: ", ")
        let sql = """
            SELECT DISTINCT chat_id AS cid
            FROM chat_handle_join
            WHERE handle_id IN (\(placeholders))
            """
        let rows = try Row.fetchAll(db, sql: sql,
                                    arguments: StatementArguments(handleROWIDs as [DatabaseValueConvertible]))
        return rows.compactMap { $0["cid"] as Int64? }
    }

    /// Fetch one budget-limited page of messages for `canonicalHandle`
    /// at-or-after `since`, descending by ROWID, resuming strictly below
    /// `progressBelowRowID` when set. Returns BOTH inbound rows (matched
    /// by handle) and outbound rows (matched by chat membership).
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
        let chatROWIDs = try resolveChatROWIDs(db: db, handleROWIDs: handleROWIDs)

        // Convert the Date lower bound to chat.db's Apple-epoch
        // NANOSECONDS unit.
        let sinceNanos = Int64(
            (since.timeIntervalSince1970 - ChatDBReader.appleEpochOffset) * 1e9)

        // Inbound: handle_id IN (...). Outbound (NULL handle_id): the row
        // is_from_me=1 and belongs to one of the handle's chats. The
        // outbound branch is omitted when the chat set is empty, leaving
        // the query identical to the inbound-only form.
        let handlePlaceholders = Array(repeating: "?", count: handleROWIDs.count).joined(separator: ", ")
        var arguments: [DatabaseValueConvertible] = handleROWIDs
        let matchClause: String
        if chatROWIDs.isEmpty {
            matchClause = "message.handle_id IN (\(handlePlaceholders))"
        } else {
            let chatPlaceholders = Array(repeating: "?", count: chatROWIDs.count).joined(separator: ", ")
            matchClause = """
                ( message.handle_id IN (\(handlePlaceholders))
                  OR (message.is_from_me = 1 AND message.ROWID IN (
                        SELECT cmj.message_id FROM chat_message_join AS cmj
                        WHERE cmj.chat_id IN (\(chatPlaceholders)))) )
                """
            arguments.append(contentsOf: chatROWIDs)
        }
        var conditions = [matchClause, "message.date >= ?"]
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
