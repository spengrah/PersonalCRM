// ChatDBReader — GRDB-backed read-only iterator over message rows.
//
// Opens chat.db in read-only mode (mode=ro; NOT immutable=1 — Messages.app
// is actively writing while we read). Honors WAL via GRDB's default
// behavior. On SQLITE_BUSY: one 200ms retry; on continued failure the
// caller marks the source unhealthy for this tick.
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

public final class ChatDBReader {
    /// Apple-epoch offset in seconds (1970-01-01 -> 2001-01-01).
    public static let appleEpochOffset: TimeInterval = 978_307_200

    /// Sentinel below which chat.db dates are treated as corrupt and
    /// skipped (with a warning). Equivalent to ~2017-09 in nanoseconds
    /// since the Apple epoch.
    private static let dateSentinelFloor: Int64 = 500_000_000_000_000_000

    /// max message.ROWID currently in the database.
    public static func maxROWID(db: Database) throws -> Int64? {
        let row = try Row.fetchOne(db, sql: "SELECT MAX(ROWID) AS m FROM message")
        return row?["m"] as Int64?
    }

    /// Fetch up to `limit` rows in `direction` order, with per-row
    /// JOINs to handle/chat/attachment.  Rows whose handle.id is empty
    /// or whose date is corrupt are skipped (the caller will not see
    /// them).
    ///
    /// Returns the rows; the caller is responsible for updating the
    /// cursor based on the highest/lowest ROWID seen.
    public static func fetch(
        db: Database,
        direction: ReadDirection,
        limit: Int
    ) throws -> [ChatDBMessage] {
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

        // GRDB row dictionary access uses column aliases; we use
        // explicit aliases to keep the per-row mapping deterministic
        // and resilient to JOIN column-name collisions on (e.g.) ROWID.
        let sql = """
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
            WHERE \(condition)
            ORDER BY \(order)
            LIMIT ?
            """

        var results: [ChatDBMessage] = []
        let rows = try Row.fetchAll(db, sql: sql, arguments: [bound, limit])
        results.reserveCapacity(rows.count)

        for row in rows {
            guard let rowID: Int64 = row["msg_rowid"],
                  let guid: String = row["msg_guid"],
                  let rawDate: Int64 = row["msg_date"] else {
                continue
            }
            // Skip rows with no handle (system messages, etc.) — they
            // have no peer to attribute to and the Pi would no-match
            // anyway.
            let handleRaw: String = (row["hnd_id"] as String?) ?? ""
            if handleRaw.isEmpty {
                continue
            }

            // Skip corrupt/sentinel date rows. The Apple-epoch
            // nanoseconds convention is hard-coded; rows with date == 0
            // or date < ~2017 are treated as corrupt.
            if rawDate < Self.dateSentinelFloor {
                continue
            }
            let sentAt = Date(timeIntervalSince1970:
                Self.appleEpochOffset + Double(rawDate) / 1e9)

            let isFromMeRaw: Int64 = (row["msg_is_from_me"] as Int64?) ?? 0
            let isFromMe = isFromMeRaw != 0

            let chatGUID: String? = row["chat_guid"]
            // style 43 is a group chat per Apple's internal convention.
            let chatStyle: Int64 = (row["chat_style"] as Int64?) ?? 0
            let isGroup = chatStyle == 43

            let text: String? = row["msg_text"]
            let replyTo: String? = row["msg_reply_to_guid"]

            let primaryUTI: String? = row["att_uti"]
            let primaryMime: String? = row["att_mime"]
            let primaryName: String? = row["att_name"]
            let primarySize: Int64? = row["att_size"]

            results.append(ChatDBMessage(
                rowID: rowID,
                guid: guid,
                chatGUID: chatGUID,
                peerHandleRaw: handleRaw,
                text: text,
                isFromMe: isFromMe,
                isGroup: isGroup,
                sentAt: sentAt,
                replyToGUID: replyTo,
                primaryAttachmentUTI: primaryUTI,
                primaryAttachmentMimeType: primaryMime,
                primaryAttachmentTransferName: primaryName,
                primaryAttachmentTotalBytes: primarySize))
        }
        return results
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
