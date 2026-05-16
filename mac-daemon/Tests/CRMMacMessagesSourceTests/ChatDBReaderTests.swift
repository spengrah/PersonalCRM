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

    /// Insert a message row + its chat_message_join link.
    private func insertMessage(
        db: Database,
        rowID: Int64,
        guid: String,
        text: String?,
        handleID: Int64?,
        isFromMe: Bool,
        unixDate: TimeInterval,
        chatRowID: Int64,
        replyToGUID: String? = nil
    ) throws {
        let appleDate = InMemoryChatDB.appleEpochNanos(unix: unixDate)
        let sql = """
            INSERT INTO message (ROWID, guid, text, handle_id, date,
                                 is_from_me, cache_has_attachments,
                                 associated_message_guid)
            VALUES (?, ?, ?, ?, ?, ?, 0, ?)
            """
        try db.execute(sql: sql, arguments: [
            rowID, guid, text,
            handleID as DatabaseValueConvertible? ?? DatabaseValue.null,
            appleDate, isFromMe ? 1 : 0,
            replyToGUID as DatabaseValueConvertible? ?? DatabaseValue.null,
        ])
        try db.execute(
            sql: "INSERT INTO chat_message_join (chat_id, message_id) VALUES (?, ?)",
            arguments: [chatRowID, rowID])
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
        // Outbound rows have handle_id=NULL in chat.db -> no JOIN
        // result -> the reader skips them (we filter empty
        // peerHandleRaw). Outbound peer selection happens via the
        // outboundGroupPeer/outbound 1:1 lookup, done by the caller
        // (PayloadShaping) when isFromMe=true.
        XCTAssertEqual(rows.count, 0,
                       "outbound rows are not surfaced by fetch() " +
                       "(no peer); caller resolves outbound peers explicitly")
    }

    func testOutbound1to1PeerLookup() throws {
        let queue = try makeFixture()
        let peer = try queue.read { db in
            try ChatDBReader.outboundGroupPeer(db: db, chatGUID: "iMessage;-;+15551234567")
        }
        XCTAssertEqual(peer, "+15551234567")
    }

    func testOutboundGroupPeerLookupFirstByJoinROWID() throws {
        let queue = try makeFixture()
        let peer = try queue.read { db in
            try ChatDBReader.outboundGroupPeer(db: db, chatGUID: "iMessage;+;groupX")
        }
        XCTAssertEqual(peer, "foo@example.com",
                       "first non-self handle by chat_handle_join.ROWID order")
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
