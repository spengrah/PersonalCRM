import XCTest
import GRDB
@testable import CRMMacMessagesSource

final class ChatDBReaderTests: XCTestCase {
    private let unix2026: TimeInterval = 1_767_225_600 // 2026-01-01 UTC

    private func makeFixture() throws -> DatabaseQueue {
        let queue = try InMemoryChatDB.makeQueue()
        try queue.write { db in
            // Handle 1: +15551234567 (1:1 peer)
            try db.execute(sql:
                "INSERT INTO handle (ROWID, id, service) VALUES (1, '+15551234567', 'iMessage')")
            // Handle 2: foo@example.com (group member)
            try db.execute(sql:
                "INSERT INTO handle (ROWID, id, service) VALUES (2, 'foo@example.com', 'iMessage')")
            // Handle 3: bar@example.com (group member)
            try db.execute(sql:
                "INSERT INTO handle (ROWID, id, service) VALUES (3, 'bar@example.com', 'iMessage')")

            // 1:1 chat with handle 1.
            try db.execute(sql:
                "INSERT INTO chat (ROWID, guid, style, chat_identifier) " +
                "VALUES (10, 'iMessage;-;+15551234567', 45, '+15551234567')")
            try db.execute(sql:
                "INSERT INTO chat_handle_join (chat_id, handle_id) VALUES (10, 1)")

            // Group chat with handles 2 + 3.
            try db.execute(sql:
                "INSERT INTO chat (ROWID, guid, style, chat_identifier) " +
                "VALUES (20, 'iMessage;+;groupX', 43, 'groupX')")
            try db.execute(sql:
                "INSERT INTO chat_handle_join (chat_id, handle_id) VALUES (20, 2)")
            try db.execute(sql:
                "INSERT INTO chat_handle_join (chat_id, handle_id) VALUES (20, 3)")
        }
        return queue
    }

    /// Insert a message row + its chat_message_join link. `chatRowID`
    /// nil skips the join (for the unresolvable-outbound case).
    private func insertMessage(
        db: Database,
        rowID: Int64,
        guid: String,
        text: String?,
        handleID: Int64?,
        isFromMe: Bool,
        unixDate: TimeInterval,
        chatRowID: Int64?,
        replyToGUID: String? = nil,
        itemType: Int64 = 0
    ) throws {
        let appleDate = InMemoryChatDB.appleEpochNanos(unix: unixDate)
        let sql = """
            INSERT INTO message (ROWID, guid, text, handle_id, date,
                                 is_from_me, item_type, cache_has_attachments,
                                 associated_message_guid)
            VALUES (?, ?, ?, ?, ?, ?, ?, 0, ?)
            """
        try db.execute(sql: sql, arguments: [
            rowID, guid, text,
            handleID as DatabaseValueConvertible? ?? DatabaseValue.null,
            appleDate, isFromMe ? 1 : 0, itemType,
            replyToGUID as DatabaseValueConvertible? ?? DatabaseValue.null,
        ])
        if let chatRowID {
            try db.execute(
                sql: "INSERT INTO chat_message_join (chat_id, message_id) VALUES (?, ?)",
                arguments: [chatRowID, rowID])
        }
    }

    /// Insert an attachment join with NO attachment-metadata row, so the
    /// join exists (att_join_id non-NULL) but every metadata column is
    /// NULL. Exercises the "attachment presence via join, not metadata"
    /// path.
    private func insertBareAttachmentJoin(
        db: Database,
        joinRowID: Int64,
        attachmentID: Int64,
        messageRowID: Int64
    ) throws {
        try db.execute(sql:
            "INSERT INTO message_attachment_join (ROWID, message_id, attachment_id) VALUES (?, ?, ?)",
            arguments: [joinRowID, messageRowID, attachmentID])
    }

    private func insertAttachment(
        db: Database,
        rowID: Int64,
        guid: String,
        uti: String?,
        mimeType: String?,
        transferName: String?,
        totalBytes: Int64?,
        messageRowID: Int64
    ) throws {
        try db.execute(sql:
            "INSERT INTO attachment (ROWID, guid, uti, mime_type, transfer_name, total_bytes) " +
            "VALUES (?, ?, ?, ?, ?, ?)",
            arguments: [rowID, guid, uti, mimeType, transferName, totalBytes])
        try db.execute(sql:
            "INSERT INTO message_attachment_join (message_id, attachment_id) VALUES (?, ?)",
            arguments: [messageRowID, rowID])
    }

    // MARK: - empty DB

    func testEmptyDBYieldsNothing() throws {
        let queue = try InMemoryChatDB.makeQueue()
        let rows = try queue.read { db in
            try ChatDBReader.fetch(db: db,
                                   direction: .forwardFromExclusive(0),
                                   limit: 10)
        }
        XCTAssertTrue(rows.isEmpty)
        let maxRow = try queue.read { try ChatDBReader.maxROWID(db: $0) }
        XCTAssertNil(maxRow)
    }

    // MARK: - inbound 1:1

    func testInbound1to1PeerHandleFromMessageHandle() throws {
        let queue = try makeFixture()
        try queue.write { db in
            // Inbound: is_from_me=0, message.handle_id = 1 (+15551234567)
            try insertMessage(db: db, rowID: 100, guid: "g1", text: "hi",
                               handleID: 1, isFromMe: false,
                               unixDate: unix2026, chatRowID: 10)
        }
        let rows = try queue.read { db in
            try ChatDBReader.fetch(db: db,
                                   direction: .forwardFromExclusive(0),
                                   limit: 10)
        }
        XCTAssertEqual(rows.count, 1)
        let row = rows[0]
        XCTAssertEqual(row.rowID, 100)
        XCTAssertEqual(row.guid, "g1")
        XCTAssertEqual(row.peerHandleRaw, "+15551234567",
                       "inbound peer = message.handle_id, NOT chat_handle_join")
        XCTAssertFalse(row.isFromMe)
        XCTAssertFalse(row.isGroup)
        XCTAssertEqual(row.text, "hi")
    }

    // MARK: - inbound group: still uses message.handle_id, not chat_handle_join roster

    func testInboundGroupPeerFromMessageHandle() throws {
        let queue = try makeFixture()
        try queue.write { db in
            // Inbound from handle 3 (bar@example.com) in group 20.
            try insertMessage(db: db, rowID: 200, guid: "g2", text: "yo",
                               handleID: 3, isFromMe: false,
                               unixDate: unix2026, chatRowID: 20)
        }
        let rows = try queue.read { db in
            try ChatDBReader.fetch(db: db,
                                   direction: .forwardFromExclusive(0),
                                   limit: 10)
        }
        XCTAssertEqual(rows.count, 1)
        let row = rows[0]
        XCTAssertEqual(row.peerHandleRaw, "bar@example.com",
                       "inbound group peer = the actual sender (message.handle_id), " +
                       "NOT a participant picked from chat_handle_join")
        XCTAssertTrue(row.isGroup, "chat.style=43 -> group")
    }

    // MARK: - outbound 1:1

    func testOutbound1to1() throws {
        let queue = try makeFixture()
        try queue.write { db in
            try insertMessage(db: db, rowID: 300, guid: "g3", text: "out",
                               handleID: nil, isFromMe: true,
                               unixDate: unix2026, chatRowID: 10)
        }
        let rows = try queue.read { db in
            try ChatDBReader.fetch(db: db,
                                   direction: .forwardFromExclusive(0),
                                   limit: 10)
        }
        // Outbound rows have handle_id=NULL in chat.db. They ARE now
        // surfaced with an empty peerHandleRaw; the plugin resolves the
        // peer via the chat's chat_handle_join entry before shaping.
        XCTAssertEqual(rows.count, 1)
        let row = rows[0]
        XCTAssertEqual(row.guid, "g3")
        XCTAssertTrue(row.isFromMe)
        XCTAssertEqual(row.peerHandleRaw, "",
                       "outbound NULL-handle row surfaces with empty peer for plugin resolution")
        XCTAssertEqual(row.chatGUID, "iMessage;-;+15551234567")
    }

    func testOutboundRowWithJoinedHandleKeepsIt() throws {
        let queue = try makeFixture()
        try queue.write { db in
            // Some macOS/SMS variants populate message.handle_id on
            // outbound rows; the reader uses it directly.
            try insertMessage(db: db, rowID: 301, guid: "g3b", text: "out",
                               handleID: 1, isFromMe: true,
                               unixDate: unix2026, chatRowID: 10)
        }
        let rows = try queue.read { db in
            try ChatDBReader.fetch(db: db,
                                   direction: .forwardFromExclusive(0),
                                   limit: 10)
        }
        XCTAssertEqual(rows.count, 1)
        XCTAssertTrue(rows[0].isFromMe)
        XCTAssertEqual(rows[0].peerHandleRaw, "+15551234567",
                       "outbound row with a joined handle keeps it")
    }

    func testOutboundRowNoChatNoHandleSkippedButBoundsAdvance() throws {
        let queue = try makeFixture()
        try queue.write { db in
            // Outbound row with NO handle AND no chat_message_join link:
            // unresolvable, skipped — but the scanned bounds + inspected
            // count still cover it so the cursor advances past it.
            try insertMessage(db: db, rowID: 302, guid: "g3c", text: "out",
                               handleID: nil, isFromMe: true,
                               unixDate: unix2026, chatRowID: nil)
        }
        let page = try queue.read { db in
            try ChatDBReader.fetchPage(db: db,
                                       direction: .forwardFromExclusive(0),
                                       limit: 10)
        }
        XCTAssertEqual(page.rows.count, 0, "unresolvable outbound row skipped")
        XCTAssertEqual(page.inspected, 1, "the skipped row still counts as inspected")
        XCTAssertEqual(page.scannedROWIDBounds?.min, 302)
        XCTAssertEqual(page.scannedROWIDBounds?.max, 302)
    }

    func testOutbound1to1PeerLookup() throws {
        let queue = try makeFixture()
        let peer = try queue.read { db in
            try ChatDBReader.outboundPeer(db: db, chatGUID: "iMessage;-;+15551234567")
        }
        XCTAssertEqual(peer, "+15551234567")
    }

    func testOutboundGroupPeerLookupFirstByJoinROWID() throws {
        let queue = try makeFixture()
        let peer = try queue.read { db in
            try ChatDBReader.outboundPeer(db: db, chatGUID: "iMessage;+;groupX")
        }
        XCTAssertEqual(peer, "foo@example.com",
                       "first chat_handle_join entry by join ROWID")
    }

    func testResolveOutboundPeersMemoizesPerChat() throws {
        let queue = try makeFixture()
        try queue.write { db in
            // Two outbound rows in the 1:1 chat, one in the group chat,
            // and one in a chat with no chat_handle_join membership.
            try insertMessage(db: db, rowID: 310, guid: "o1", text: "a",
                               handleID: nil, isFromMe: true,
                               unixDate: unix2026, chatRowID: 10)
            try insertMessage(db: db, rowID: 311, guid: "o2", text: "b",
                               handleID: nil, isFromMe: true,
                               unixDate: unix2026, chatRowID: 10)
            try insertMessage(db: db, rowID: 312, guid: "o3", text: "c",
                               handleID: nil, isFromMe: true,
                               unixDate: unix2026, chatRowID: 20)
            // A chat with a guid but zero chat_handle_join rows.
            try db.execute(sql:
                "INSERT INTO chat (ROWID, guid, style, chat_identifier) " +
                "VALUES (30, 'iMessage;-;orphan', 45, 'orphan')")
            try insertMessage(db: db, rowID: 313, guid: "o4", text: "d",
                               handleID: nil, isFromMe: true,
                               unixDate: unix2026, chatRowID: 30)
        }
        let result = try queue.read { db -> [String: String] in
            let page = try ChatDBReader.fetchPage(
                db: db, direction: .forwardFromExclusive(0), limit: 10)
            return try ChatDBReader.resolveOutboundPeers(db: db, rows: page.rows)
        }
        XCTAssertEqual(result["iMessage;-;+15551234567"], "+15551234567")
        XCTAssertEqual(result["iMessage;+;groupX"], "foo@example.com")
        XCTAssertNil(result["iMessage;-;orphan"],
                     "chat without chat_handle_join rows is absent from the map")
    }

    // MARK: - attachment

    func testPrimaryAttachmentMapping() throws {
        let queue = try makeFixture()
        try queue.write { db in
            try insertMessage(db: db, rowID: 400, guid: "g4", text: nil,
                               handleID: 1, isFromMe: false,
                               unixDate: unix2026, chatRowID: 10)
            try insertAttachment(db: db, rowID: 50, guid: "a1",
                                  uti: "public.jpeg",
                                  mimeType: "image/jpeg",
                                  transferName: "photo.jpg",
                                  totalBytes: 12345,
                                  messageRowID: 400)
        }
        let rows = try queue.read { db in
            try ChatDBReader.fetch(db: db,
                                   direction: .forwardFromExclusive(0),
                                   limit: 10)
        }
        XCTAssertEqual(rows.count, 1)
        let row = rows[0]
        XCTAssertEqual(row.primaryAttachmentUTI, "public.jpeg")
        XCTAssertEqual(row.primaryAttachmentMimeType, "image/jpeg")
        XCTAssertEqual(row.primaryAttachmentTransferName, "photo.jpg")
        XCTAssertEqual(row.primaryAttachmentTotalBytes, 12345)
    }

    // MARK: - reply chain

    func testReplyToGUID() throws {
        let queue = try makeFixture()
        try queue.write { db in
            try insertMessage(db: db, rowID: 500, guid: "g5", text: "reply",
                               handleID: 1, isFromMe: false,
                               unixDate: unix2026, chatRowID: 10,
                               replyToGUID: "parent-guid")
        }
        let rows = try queue.read { db in
            try ChatDBReader.fetch(db: db,
                                   direction: .forwardFromExclusive(0),
                                   limit: 10)
        }
        XCTAssertEqual(rows.count, 1)
        XCTAssertEqual(rows[0].replyToGUID, "parent-guid")
    }

    // MARK: - cursor advancement

    func testForwardCursorAdvancement() throws {
        let queue = try makeFixture()
        try queue.write { db in
            for i in 1...5 {
                try insertMessage(db: db, rowID: Int64(600 + i),
                                   guid: "fwd\(i)", text: "msg\(i)",
                                   handleID: 1, isFromMe: false,
                                   unixDate: unix2026 + Double(i),
                                   chatRowID: 10)
            }
        }
        // First batch from 0.
        let firstBatch = try queue.read { db in
            try ChatDBReader.fetch(db: db,
                                   direction: .forwardFromExclusive(0),
                                   limit: 3)
        }
        XCTAssertEqual(firstBatch.map(\.rowID), [601, 602, 603])
        // Second batch from 603.
        let secondBatch = try queue.read { db in
            try ChatDBReader.fetch(db: db,
                                   direction: .forwardFromExclusive(603),
                                   limit: 3)
        }
        XCTAssertEqual(secondBatch.map(\.rowID), [604, 605])
    }

    func testBackwardCursorDescend() throws {
        let queue = try makeFixture()
        try queue.write { db in
            for i in 1...5 {
                try insertMessage(db: db, rowID: Int64(700 + i),
                                   guid: "bwd\(i)", text: "msg\(i)",
                                   handleID: 1, isFromMe: false,
                                   unixDate: unix2026 + Double(i),
                                   chatRowID: 10)
            }
        }
        let batch = try queue.read { db in
            try ChatDBReader.fetch(db: db,
                                   direction: .backwardFromExclusive(706),
                                   limit: 3)
        }
        XCTAssertEqual(batch.map(\.rowID), [705, 704, 703])
    }

    // MARK: - date sentinel skip

    func testDateZeroSkipped() throws {
        let queue = try makeFixture()
        try queue.write { db in
            try db.execute(sql:
                "INSERT INTO message (ROWID, guid, text, handle_id, date, is_from_me, " +
                "cache_has_attachments, associated_message_guid) " +
                "VALUES (800, 'g-zero', 'corrupt', 1, 0, 0, 0, NULL)")
            try db.execute(sql:
                "INSERT INTO chat_message_join (chat_id, message_id) VALUES (10, 800)")
        }
        let rows = try queue.read { db in
            try ChatDBReader.fetch(db: db,
                                   direction: .forwardFromExclusive(0),
                                   limit: 10)
        }
        XCTAssertEqual(rows.count, 0, "date=0 must be skipped")
    }

    func testDateSubThresholdSkipped() throws {
        let queue = try makeFixture()
        try queue.write { db in
            // Date below sentinel floor (< 5e17 ns since Apple epoch).
            try db.execute(sql:
                "INSERT INTO message (ROWID, guid, text, handle_id, date, is_from_me, " +
                "cache_has_attachments, associated_message_guid) " +
                "VALUES (801, 'g-sub', 'ancient', 1, 1, 0, 0, NULL)")
            try db.execute(sql:
                "INSERT INTO chat_message_join (chat_id, message_id) VALUES (10, 801)")
        }
        let rows = try queue.read { db in
            try ChatDBReader.fetch(db: db,
                                   direction: .forwardFromExclusive(0),
                                   limit: 10)
        }
        XCTAssertEqual(rows.count, 0, "date below sentinel must be skipped")
    }

    func testValidDateRoundTrip() throws {
        // Apple-epoch nanoseconds for 2026-01-01 UTC.
        // unix2026 - 978307200 = 788918400 seconds = 7.89e17 nanoseconds.
        let queue = try makeFixture()
        try queue.write { db in
            try insertMessage(db: db, rowID: 802, guid: "g-ok",
                               text: "ok", handleID: 1, isFromMe: false,
                               unixDate: unix2026, chatRowID: 10)
        }
        let rows = try queue.read { db in
            try ChatDBReader.fetch(db: db,
                                   direction: .forwardFromExclusive(0),
                                   limit: 10)
        }
        XCTAssertEqual(rows.count, 1)
        XCTAssertEqual(rows[0].sentAt.timeIntervalSince1970, unix2026, accuracy: 0.001)
    }

    // MARK: - empty handle skipped

    func testEmptyHandleSkipped() throws {
        let queue = try makeFixture()
        try queue.write { db in
            try db.execute(sql:
                "INSERT INTO handle (ROWID, id, service) VALUES (99, '', 'iMessage')")
            try insertMessage(db: db, rowID: 900, guid: "g-eh",
                               text: "x", handleID: 99, isFromMe: false,
                               unixDate: unix2026, chatRowID: 10)
        }
        let rows = try queue.read { db in
            try ChatDBReader.fetch(db: db,
                                   direction: .forwardFromExclusive(0),
                                   limit: 10)
        }
        XCTAssertEqual(rows.count, 0, "empty handle.id must be skipped")
    }

    // MARK: - system / group-action rows (item_type guard)

    func testSystemRowsSkippedBothDirections() throws {
        let queue = try makeFixture()
        try queue.write { db in
            // Inbound contentless system row (e.g. group rename, item_type=2)
            // WITH a handle — currently passes the handle check; the guard
            // must still drop it.
            try insertMessage(db: db, rowID: 320, guid: "sys-in", text: nil,
                               handleID: 3, isFromMe: false,
                               unixDate: unix2026, chatRowID: 20, itemType: 2)
            // Outbound contentless system row.
            try insertMessage(db: db, rowID: 321, guid: "sys-out", text: nil,
                               handleID: nil, isFromMe: true,
                               unixDate: unix2026, chatRowID: 20, itemType: 2)
            // item_type != 0 but WITH text → kept (conjunctive guard).
            try insertMessage(db: db, rowID: 322, guid: "sys-text", text: "still a message",
                               handleID: 3, isFromMe: false,
                               unixDate: unix2026, chatRowID: 20, itemType: 2)
        }
        let rows = try queue.read { db in
            try ChatDBReader.fetch(db: db,
                                   direction: .forwardFromExclusive(0),
                                   limit: 10)
        }
        XCTAssertEqual(Set(rows.map(\.guid)), ["sys-text"],
                       "contentless system rows skipped both directions; content-bearing kept")
    }

    func testSystemRowWithSparseAttachmentKept() throws {
        let queue = try makeFixture()
        try queue.write { db in
            // item_type != 0, no text, but an attachment join exists with
            // NO metadata row → att_join_id non-NULL while every metadata
            // column is NULL. Attachment presence comes from the join, so
            // the row is KEPT.
            try insertMessage(db: db, rowID: 330, guid: "att-sparse", text: nil,
                               handleID: 1, isFromMe: false,
                               unixDate: unix2026, chatRowID: 10, itemType: 2)
            try insertBareAttachmentJoin(db: db, joinRowID: 70,
                                         attachmentID: 999, messageRowID: 330)
        }
        let rows = try queue.read { db in
            try ChatDBReader.fetch(db: db,
                                   direction: .forwardFromExclusive(0),
                                   limit: 10)
        }
        XCTAssertEqual(rows.map(\.guid), ["att-sparse"],
                       "attachment present via join (not metadata) keeps the row")
        XCTAssertNil(rows[0].primaryAttachmentUTI,
                     "metadata columns are still NULL")
    }

    // MARK: - inspected count

    func testInspectedCountsAllSQLRows() throws {
        let queue = try makeFixture()
        try queue.write { db in
            // One kept inbound row, one skipped contentless system row.
            try insertMessage(db: db, rowID: 340, guid: "keep", text: "hi",
                               handleID: 1, isFromMe: false,
                               unixDate: unix2026, chatRowID: 10)
            try insertMessage(db: db, rowID: 341, guid: "drop", text: nil,
                               handleID: 1, isFromMe: false,
                               unixDate: unix2026, chatRowID: 10, itemType: 2)
        }
        let page = try queue.read { db in
            try ChatDBReader.fetchPage(db: db,
                                       direction: .forwardFromExclusive(0),
                                       limit: 10)
        }
        XCTAssertEqual(page.rows.count, 1, "one row kept")
        XCTAssertEqual(page.inspected, 2, "inspected counts every SQL row, kept or skipped")
    }

    // MARK: - max ROWID

    func testMaxROWIDReturnsHighestMessageROWID() throws {
        let queue = try makeFixture()
        try queue.write { db in
            try insertMessage(db: db, rowID: 1000, guid: "m1",
                               text: nil, handleID: 1, isFromMe: false,
                               unixDate: unix2026, chatRowID: 10)
            try insertMessage(db: db, rowID: 2000, guid: "m2",
                               text: nil, handleID: 1, isFromMe: false,
                               unixDate: unix2026, chatRowID: 10)
        }
        let maxRow = try queue.read { try ChatDBReader.maxROWID(db: $0) }
        XCTAssertEqual(maxRow, 2000)
    }
}
