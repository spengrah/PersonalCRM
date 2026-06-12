// ChatDBReader — GRDB-backed read-only iterator over message rows.
//
// Opens chat.db in read-only mode (mode=ro; NOT immutable=1 — Messages.app
// is actively writing while we read). Honors WAL via GRDB's default
// behavior. SQLITE_BUSY is mapped through GRDB's DatabasePool retry
// policy; on continued failure the caller logs + treats the page as
// empty for this tick (next tick re-reads).
//
// Per-row JOINs:
//   - message.handle_id -> handle.ROWID  (inbound sender + outbound 1:1
//     peer on macOS/SMS variants that populate it)
//   - chat_message_join.message_id -> chat.guid
//   - message_attachment_join (first by ROWID) -> attachment.uti +
//     attachment_id (join-existence ground truth for the content guard)
//
// Both inbound and outbound (is_from_me=1) rows are surfaced. Outbound
// rows usually have a NULL handle_id, so their peer is resolved in the
// plugin layer via outboundPeer (the row's chat_handle_join entry).
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
///     a page of all-skipped rows doesn't stall the iterator.
///   - inspected: count of SQL rows returned (pre-filter). The plugin
///     consumes its row budget on this so a page of all-skipped rows
///     still costs budget for the SQL work it did.
///   - exhausted: true if SQL returned fewer than `limit` rows (so
///     there are no more rows in the requested direction).
public struct ChatDBReadPage: Equatable, Sendable {
    public let rows: [ChatDBMessage]
    /// Min and max of EVERY row's ROWID inspected (including skipped).
    /// Nil if SQL returned zero rows.
    public let scannedROWIDBounds: (min: Int64, max: Int64)?
    /// Count of SQL rows returned for this page, before skip filters.
    public let inspected: Int
    public let exhausted: Bool

    public init(
        rows: [ChatDBMessage],
        scannedROWIDBounds: (min: Int64, max: Int64)?,
        inspected: Int,
        exhausted: Bool
    ) {
        self.rows = rows
        self.scannedROWIDBounds = scannedROWIDBounds
        self.inspected = inspected
        self.exhausted = exhausted
    }

    public static func == (lhs: ChatDBReadPage, rhs: ChatDBReadPage) -> Bool {
        guard lhs.rows == rhs.rows
            && lhs.exhausted == rhs.exhausted
            && lhs.inspected == rhs.inspected else { return false }
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
            message.item_type                     AS msg_item_type,
            message.date                          AS msg_date,
            message.associated_message_guid       AS msg_reply_to_guid,
            message.handle_id                     AS msg_handle_id,
            handle.id                             AS hnd_id,
            chat.guid                             AS chat_guid,
            chat.style                            AS chat_style,
            maj_primary.attachment_id             AS att_join_id,
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
    /// missing rowid/guid/date, a corrupt/sentinel date, a contentless
    /// system row (item_type != 0 with no text and no attachment), an
    /// inbound row with no joined handle (system messages with no peer),
    /// or an outbound row with neither a joined handle nor a chat.guid
    /// (nothing downstream could attribute it). The caller still tracks
    /// every inspected ROWID (including skipped) for cursor advance.
    ///
    /// Outbound rows that survive carry `peerHandleRaw` = the joined
    /// handle if present, else "" — the plugin resolves the empty case
    /// via the row's chat_handle_join entry before shaping.
    static func mapMessageRow(_ row: Row) -> ChatDBMessage? {
        guard let rowID: Int64 = row["msg_rowid"],
              let rawDate: Int64 = row["msg_date"],
              let guid: String = row["msg_guid"] else {
            return nil
        }
        // Skip corrupt/sentinel date rows (date == 0 or < ~2017).
        if rawDate < Self.dateSentinelFloor { return nil }

        let isFromMeRaw: Int64 = (row["msg_is_from_me"] as Int64?) ?? 0
        let isFromMe = isFromMeRaw != 0
        let handleRaw: String = (row["hnd_id"] as String?) ?? ""
        let chatGUID: String? = row["chat_guid"]

        // Contentless system row guard (CONJUNCTIVE): drop a row only
        // when item_type marks it as a non-message AND it carries no
        // text AND no attachment. Any content-bearing row is kept
        // regardless of item_type, so this can never drop a real
        // message even if Apple's private item_type semantics drift.
        // Attachment presence is the join-existence ground truth
        // (att_join_id non-NULL), NOT the optional metadata columns,
        // which can legitimately be NULL on a real attachment row.
        let itemType: Int64 = (row["msg_item_type"] as Int64?) ?? 0
        let text: String? = row["msg_text"]
        let hasText = (text?.isEmpty == false)
        let hasAttachment = (row["att_join_id"] as Int64?) != nil
        if itemType != 0 && !hasText && !hasAttachment { return nil }

        if isFromMe {
            // Outbound: keep with the joined handle if present (some
            // macOS/SMS variants populate it), else "" for the plugin to
            // resolve. Skip only when there is also no chat.guid — then
            // nothing downstream could attribute it and the Pi requires
            // a non-empty chat_id.
            if handleRaw.isEmpty && (chatGUID?.isEmpty ?? true) { return nil }
        } else {
            // Inbound: a missing joined handle means a system message
            // with no peer to attribute to.
            if handleRaw.isEmpty { return nil }
        }

        let sentAt = Date(timeIntervalSince1970:
            Self.appleEpochOffset + Double(rawDate) / 1e9)
        let chatStyle: Int64 = (row["chat_style"] as Int64?) ?? 0

        return ChatDBMessage(
            rowID: rowID,
            guid: guid,
            chatGUID: chatGUID,
            peerHandleRaw: handleRaw,
            text: text,
            isFromMe: isFromMe,
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
            // page where every row got filtered out. Any row WITH a
            // ROWID counts toward the bounds even if other columns
            // (e.g. date) are NULL/corrupt: the backfill runner treats
            // nil bounds as "iterator exhausted" and flips
            // backfillComplete, so excluding such rows from the bounds
            // could end the walk while older rows remain unread.
            guard let rowID: Int64 = row["msg_rowid"] else {
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
            inspected: rows.count,
            exhausted: rows.count < limit)
    }

    /// Resolve an outbound row's peer: the `handle.id` of the FIRST
    /// `chat_handle_join` entry (by join ROWID) for `chatGUID`. Returns
    /// the raw handle.id string, or nil if the chat has no
    /// `chat_handle_join` rows.
    ///
    /// This covers both 1:1 (the chat's single peer handle) and group
    /// chats (the first member by join ROWID — the v1 simplification, so
    /// every outbound group message attributes to the same peer). There
    /// is NO self-handle exclusion: the query returns the first joined
    /// handle, full stop. It behaves as "the peer" only because macOS
    /// does not insert the account owner's own handle into
    /// chat_handle_join.
    public static func outboundPeer(
        db: Database,
        chatGUID: String
    ) throws -> String? {
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

    /// Resolve outbound peers for a page of rows in ONE DB pass-set.
    /// Collects the distinct `chat.guid`s of outbound rows whose
    /// `peerHandleRaw` is empty (no joined handle), runs `outboundPeer`
    /// once per distinct chat (memoized — most rows in a page share a
    /// few chats), and returns a `chatGUID → raw handle` map. Chats that
    /// resolve to nothing are simply absent from the map.
    ///
    /// Runs inside the same `pool.read` snapshot as the page fetch so a
    /// transient resolution error lands in the caller's catch path
    /// (cursor not advanced) rather than silently dropping rows.
    public static func resolveOutboundPeers(
        db: Database,
        rows: [ChatDBMessage]
    ) throws -> [String: String] {
        var resolved: [String: String] = [:]
        var attempted: Set<String> = []
        for row in rows where row.isFromMe && row.peerHandleRaw.isEmpty {
            guard let chatGUID = row.chatGUID, !chatGUID.isEmpty else { continue }
            if attempted.contains(chatGUID) { continue }
            attempted.insert(chatGUID)
            if let peer = try outboundPeer(db: db, chatGUID: chatGUID) {
                resolved[chatGUID] = peer
            }
        }
        return resolved
    }
}
