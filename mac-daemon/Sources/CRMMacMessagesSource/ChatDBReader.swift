// ChatDBReader — GRDB-backed read-only iterator over message rows.
//
// Opens chat.db in read-only mode (mode=ro; NOT immutable=1 — Messages.app
// is actively writing while we read). Honors WAL via GRDB's default
// behavior. SQLITE_BUSY is mapped through GRDB's DatabasePool retry
// policy; on continued failure the caller logs + treats the page as
// empty for this tick (next tick re-reads).
//
// Per-row JOINs:
//   - message.handle_id -> handle.ROWID  (inbound sender + outbound 1:1 peer)
//   - chat_message_join.message_id -> chat.guid
//   - message_attachment_join (first by ROWID) -> attachment.uti
//
// Outbound group-chat peer (is_from_me=1, multi-handle chat): selected
// from chat_handle_join.ROWID order (first non-self).  This is a v1
// simplification — every outbound group message attributes to the same
// peer.
import Foundation
import GRDB

/// One chat.db row, shaped for downstream payload construction.
public struct ChatDBMessage: Equatable, Sendable {
    public let rowID: Int64
    public let guid: String
    public let chatGUID: String?
    /// Raw handle.id from chat.db — caller normalizes before lookup.
    public let peerHandleRaw: String
    public let text: String?
    public let isFromMe: Bool
    public let isGroup: Bool
    public let sentAt: Date
    public let replyToGUID: String?
    /// First attachment row joined via message_attachment_join (lowest
    /// join.ROWID). Nil if the message has no attachments.
    public let primaryAttachmentUTI: String?
    public let primaryAttachmentMimeType: String?
    public let primaryAttachmentTransferName: String?
    public let primaryAttachmentTotalBytes: Int64?

    public init(
        rowID: Int64, guid: String, chatGUID: String?,
        peerHandleRaw: String, text: String?, isFromMe: Bool,
        isGroup: Bool, sentAt: Date, replyToGUID: String?,
        primaryAttachmentUTI: String?,
        primaryAttachmentMimeType: String?,
        primaryAttachmentTransferName: String?,
        primaryAttachmentTotalBytes: Int64?
    ) {
        self.rowID = rowID
        self.guid = guid
        self.chatGUID = chatGUID
        self.peerHandleRaw = peerHandleRaw
        self.text = text
        self.isFromMe = isFromMe
        self.isGroup = isGroup
        self.sentAt = sentAt
        self.replyToGUID = replyToGUID
        self.primaryAttachmentUTI = primaryAttachmentUTI
        self.primaryAttachmentMimeType = primaryAttachmentMimeType
        self.primaryAttachmentTransferName = primaryAttachmentTransferName
        self.primaryAttachmentTotalBytes = primaryAttachmentTotalBytes
    }
}

/// Direction of an iteration query.
public enum ReadDirection: Sendable {
    /// message.ROWID > floor, ascending (live cursor advance).
    case forwardFromExclusive(Int64)
    /// message.ROWID < ceiling, descending (backfill cursor descend).
    case backwardFromExclusive(Int64)
}

public enum ChatDBReaderError: Error, Equatable, Sendable {
    case dateOutOfRange(rowID: Int64, rawDate: Int64)
}

/// Result of a fetch() call. Reports:
///   - rows: only the rows we kept (after skip filters).
///   - scannedROWIDBounds: (min, max) of EVERY row inspected, including
///     skipped ones. The caller advances cursors past skipped rows so
///     a page of all-empty-handle rows doesn't stall the iterator.
///   - exhausted: true if SQL returned fewer than `limit` rows (so
///     there are no more rows in the requested direction).
public struct ChatDBReadPage: Equatable, Sendable {
    public let rows: [ChatDBMessage]
    /// Min and max of EVERY row's ROWID inspected (including skipped).
    /// Nil if SQL returned zero rows.
    public let scannedROWIDBounds: (min: Int64, max: Int64)?
    public let exhausted: Bool

    public init(
        rows: [ChatDBMessage],
        scannedROWIDBounds: (min: Int64, max: Int64)?,
        exhausted: Bool
    ) {
        self.rows = rows
        self.scannedROWIDBounds = scannedROWIDBounds
        self.exhausted = exhausted
    }

    public static func == (lhs: ChatDBReadPage, rhs: ChatDBReadPage) -> Bool {
        guard lhs.rows == rhs.rows && lhs.exhausted == rhs.exhausted else { return false }
        switch (lhs.scannedROWIDBounds, rhs.scannedROWIDBounds) {
        case (nil, nil): return true
        case let (.some(l), .some(r)): return l == r
        default: return false
        }
    }
}

public final class ChatDBReader {
    /// Apple-epoch offset in seconds (1970-01-01 -> 2001-01-01).
    public static let appleEpochOffset: TimeInterval = 978_307_200

    /// Sentinel below which chat.db dates are treated as corrupt and
    /// skipped (with a warning). Equivalent to ~2017-09 in nanoseconds
    /// since the Apple epoch.
    static let dateSentinelFloor: Int64 = 500_000_000_000_000_000

    /// The shared SELECT column list + JOINs used by both the
    /// ROWID-ranged page reader and the identifier-scoped scan reader.
    /// Callers append their own WHERE / ORDER BY / LIMIT. Keeping the
    /// projection in one place means the two readers can never drift in
    /// which columns they shape a `ChatDBMessage` from.
    static let selectColumnsAndJoins = """
        SELECT
            message.ROWID                         AS msg_rowid,
            message.guid                          AS msg_guid,
            message.text                          AS msg_text,
            message.is_from_me                    AS msg_is_from_me,
            message.date                          AS msg_date,
            message.associated_message_guid       AS msg_reply_to_guid,
            message.handle_id                     AS msg_handle_id,
            handle.id                             AS hnd_id,
            chat.guid                             AS chat_guid,
            chat.style                            AS chat_style,
            att.uti                               AS att_uti,
            att.mime_type                         AS att_mime,
            att.transfer_name                     AS att_name,
            att.total_bytes                       AS att_size
        FROM message
        LEFT JOIN handle               ON handle.ROWID = message.handle_id
        LEFT JOIN chat_message_join    ON chat_message_join.message_id = message.ROWID
        LEFT JOIN chat                 ON chat.ROWID = chat_message_join.chat_id
        LEFT JOIN (
            SELECT message_id, attachment_id, MIN(ROWID) AS primary_join_rowid
            FROM message_attachment_join
            GROUP BY message_id
        ) AS maj_primary                ON maj_primary.message_id = message.ROWID
        LEFT JOIN attachment AS att     ON att.ROWID = maj_primary.attachment_id
        """

    /// Map one fetched GRDB row (aliased per `selectColumnsAndJoins`) to
    /// a `ChatDBMessage`. Returns nil for rows that should be SKIPPED:
    /// missing rowid/guid/date, NULL/empty handle (system messages,
    /// outbound), or a corrupt/sentinel date. The caller still tracks
    /// every inspected ROWID (including skipped) for cursor advance.
    static func mapMessageRow(_ row: Row) -> ChatDBMessage? {
        guard let rowID: Int64 = row["msg_rowid"],
              let rawDate: Int64 = row["msg_date"],
              let guid: String = row["msg_guid"] else {
            return nil
        }
        // Skip rows with no handle (system messages, outbound) — no peer
        // to attribute to and the Pi would no-match anyway.
        let handleRaw: String = (row["hnd_id"] as String?) ?? ""
        if handleRaw.isEmpty { return nil }
        // Skip corrupt/sentinel date rows (date == 0 or < ~2017).
        if rawDate < Self.dateSentinelFloor { return nil }
        let sentAt = Date(timeIntervalSince1970:
            Self.appleEpochOffset + Double(rawDate) / 1e9)

        let isFromMeRaw: Int64 = (row["msg_is_from_me"] as Int64?) ?? 0
        let chatStyle: Int64 = (row["chat_style"] as Int64?) ?? 0

        return ChatDBMessage(
            rowID: rowID,
            guid: guid,
            chatGUID: row["chat_guid"],
            peerHandleRaw: handleRaw,
            text: row["msg_text"],
            isFromMe: isFromMeRaw != 0,
            // style 43 is a group chat per Apple's internal convention.
            isGroup: chatStyle == 43,
            sentAt: sentAt,
            replyToGUID: row["msg_reply_to_guid"],
            primaryAttachmentUTI: row["att_uti"],
            primaryAttachmentMimeType: row["att_mime"],
            primaryAttachmentTransferName: row["att_name"],
            primaryAttachmentTotalBytes: row["att_size"])
    }

    /// max message.ROWID currently in the database.
    public static func maxROWID(db: Database) throws -> Int64? {
        let row = try Row.fetchOne(db, sql: "SELECT MAX(ROWID) AS m FROM message")
        return row?["m"] as Int64?
    }

    /// Fetch up to `limit` rows in `direction` order, with per-row
    /// JOINs to handle/chat/attachment.
    ///
    /// Returns only the rows we kept; this entry point is kept for
    /// backward compatibility with tests that don't need the bounds
    /// info.  New callers should use `fetchPage` which also reports
    /// the scanned-ROWID bounds (so the caller can advance the cursor
    /// past skipped rows).
    public static func fetch(
        db: Database,
        direction: ReadDirection,
        limit: Int
    ) throws -> [ChatDBMessage] {
        try fetchPage(db: db, direction: direction, limit: limit).rows
    }

    /// Like `fetch` but also reports the min+max of every ROWID
    /// inspected and whether SQL returned fewer rows than the limit
    /// (i.e. the iterator is exhausted in the requested direction).
    public static func fetchPage(
        db: Database,
        direction: ReadDirection,
        limit: Int
    ) throws -> ChatDBReadPage {
        precondition(limit > 0, "limit must be > 0")
        let condition: String
        let order: String
        let bound: Int64
        switch direction {
        case .forwardFromExclusive(let lower):
            condition = "message.ROWID > ?"
            order = "message.ROWID ASC"
            bound = lower
        case .backwardFromExclusive(let upper):
            condition = "message.ROWID < ?"
            order = "message.ROWID DESC"
            bound = upper
        }

        let sql = """
            \(Self.selectColumnsAndJoins)
            WHERE \(condition)
            ORDER BY \(order)
            LIMIT ?
            """

        var results: [ChatDBMessage] = []
        var scannedMin: Int64?
        var scannedMax: Int64?
        let rows = try Row.fetchAll(db, sql: sql, arguments: [bound, limit])
        results.reserveCapacity(rows.count)

        for row in rows {
            // Track every ROWID we saw, including skipped ones — the
            // caller needs this so cursor advance doesn't stall on a
            // page where every row got filtered out. A row missing the
            // ROWID or date is not a real message row and is not counted
            // toward the scanned bounds (matches the pre-extraction
            // behavior so backfillComplete still flips correctly).
            guard let rowID: Int64 = row["msg_rowid"],
                  row["msg_date"] as Int64? != nil else {
                continue
            }
            scannedMin = min(scannedMin ?? rowID, rowID)
            scannedMax = max(scannedMax ?? rowID, rowID)

            if let mapped = Self.mapMessageRow(row) {
                results.append(mapped)
            }
        }
        let bounds: (min: Int64, max: Int64)? = {
            if let lo = scannedMin, let hi = scannedMax {
                return (min: lo, max: hi)
            }
            return nil
        }()
        return ChatDBReadPage(
            rows: results,
            scannedROWIDBounds: bounds,
            exhausted: rows.count < limit)
    }

    /// Outbound group-chat peer selection: smallest `chat_handle_join.ROWID`
    /// in the chat that is NOT the self-handle.  Returns the raw
    /// handle.id string, or nil if the chat has no non-self handle.
    public static func outboundGroupPeer(
        db: Database,
        chatGUID: String
    ) throws -> String? {
        // SELECT handle.id FROM chat_handle_join chj
        //  JOIN chat ON chat.ROWID = chj.chat_id AND chat.guid = ?
        //  JOIN handle ON handle.ROWID = chj.handle_id
        //  ORDER BY chj.ROWID ASC LIMIT 1
        let sql = """
            SELECT handle.id AS hid
            FROM chat_handle_join AS chj
            JOIN chat   ON chat.ROWID = chj.chat_id
            JOIN handle ON handle.ROWID = chj.handle_id
            WHERE chat.guid = ?
            ORDER BY chj.ROWID ASC
            LIMIT 1
            """
        let row = try Row.fetchOne(db, sql: sql, arguments: [chatGUID])
        return row?["hid"] as String?
    }
}
