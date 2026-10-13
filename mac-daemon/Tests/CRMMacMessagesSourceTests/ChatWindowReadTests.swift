// ChatWindowReadTests observe ChatDBReader.fetchChatWindow: the
// time-bounded read of one chat's messages that the group re-scan uses.
import XCTest
import GRDB
@testable import CRMMacMessagesSource

final class ChatWindowReadTests: XCTestCase {
    private let unix2026: TimeInterval = 1_767_225_600 // 2026-01-01 UTC

    private func date(_ offset: TimeInterval) -> Date {
        Date(timeIntervalSince1970: unix2026 + offset)
    }

    private func makeFixture() throws -> DatabaseQueue {
        let queue = try InMemoryChatDB.makeQueue()
        try queue.write { db in
            try db.execute(sql:
                "INSERT INTO handle (ROWID, id, service) VALUES (1, 'foo@example.com', 'iMessage')")
            try db.execute(sql:
                "INSERT INTO handle (ROWID, id, service) VALUES (2, 'bar@example.com', 'iMessage')")
            try db.execute(sql:
                "INSERT INTO chat (ROWID, guid, style, chat_identifier) " +
                "VALUES (20, 'iMessage;+;groupX', 43, 'groupX')")
            try db.execute(sql:
                "INSERT INTO chat (ROWID, guid, style, chat_identifier) " +
                "VALUES (21, 'iMessage;+;groupY', 43, 'groupY')")
            try db.execute(sql:
                "INSERT INTO chat_handle_join (chat_id, handle_id) VALUES (20, 1)")
            try db.execute(sql:
                "INSERT INTO chat_handle_join (chat_id, handle_id) VALUES (20, 2)")
        }
        return queue
    }

    private func insertMessage(
        db: Database,
        rowID: Int64,
        guid: String,
        text: String? = "hi",
        attributedBody: Data? = nil,
        handleID: Int64?,
        isFromMe: Bool,
        offset: TimeInterval,
        chatRowID: Int64 = 20,
        itemType: Int64 = 0
    ) throws {
        try db.execute(sql: """
            INSERT INTO message (ROWID, guid, text, attributedBody, handle_id, date,
                                 is_from_me, item_type, cache_has_attachments,
                                 associated_message_guid)
            VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, NULL)
            """, arguments: [
                rowID, guid, text, attributedBody,
                handleID as DatabaseValueConvertible? ?? DatabaseValue.null,
                InMemoryChatDB.appleEpochNanos(unix: unix2026 + offset),
                isFromMe ? 1 : 0, itemType,
            ])
        try db.execute(
            sql: "INSERT INTO chat_message_join (chat_id, message_id) VALUES (?, ?)",
            arguments: [chatRowID, rowID])
    }

    private func window(
        _ queue: DatabaseQueue, chat: String = "iMessage;+;groupX",
        from: TimeInterval, through: TimeInterval
    ) throws -> [ChatDBMessage] {
        try queue.read {
            try ChatDBReader.fetchChatWindow(
                db: $0, chatGUID: chat, from: date(from), through: date(through))
        }
    }

    func testBoundsAreInclusive() throws {
        let queue = try makeFixture()
        try queue.write { db in
            try insertMessage(db: db, rowID: 1, guid: "before", handleID: 1, isFromMe: false, offset: 99)
            try insertMessage(db: db, rowID: 2, guid: "at-from", handleID: 1, isFromMe: false, offset: 100)
            try insertMessage(db: db, rowID: 3, guid: "inside", handleID: 1, isFromMe: false, offset: 150)
            try insertMessage(db: db, rowID: 4, guid: "at-through", handleID: 1, isFromMe: false, offset: 200)
            try insertMessage(db: db, rowID: 5, guid: "after", handleID: 1, isFromMe: false, offset: 201)
        }
        let rows = try window(queue, from: 100, through: 200)
        XCTAssertEqual(rows.map(\.guid), ["at-from", "inside", "at-through"])
    }

    func testOtherChatRowsAreNotReturned() throws {
        let queue = try makeFixture()
        try queue.write { db in
            try insertMessage(db: db, rowID: 1, guid: "mine", handleID: 1, isFromMe: false, offset: 150)
            try insertMessage(db: db, rowID: 2, guid: "other", handleID: 1, isFromMe: false,
                              offset: 150, chatRowID: 21)
        }
        let rows = try window(queue, from: 100, through: 200)
        XCTAssertEqual(rows.map(\.guid), ["mine"])
    }

    func testOrderedBySentTimeThenROWID() throws {
        let queue = try makeFixture()
        try queue.write { db in
            // ROWID 1 sent LATER than ROWIDs 2 and 3; 2 and 3 share a time.
            try insertMessage(db: db, rowID: 1, guid: "late-low-rowid", handleID: 1, isFromMe: false, offset: 180)
            try insertMessage(db: db, rowID: 3, guid: "tie-b", handleID: 1, isFromMe: false, offset: 150)
            try insertMessage(db: db, rowID: 2, guid: "tie-a", handleID: 1, isFromMe: false, offset: 150)
        }
        let rows = try window(queue, from: 100, through: 200)
        XCTAssertEqual(rows.map(\.guid), ["tie-a", "tie-b", "late-low-rowid"])
    }

    func testSystemRowSkippedThatOtherReadsSkip() throws {
        let queue = try makeFixture()
        try queue.write { db in
            try insertMessage(db: db, rowID: 1, guid: "real", handleID: 1, isFromMe: false, offset: 150)
            try insertMessage(db: db, rowID: 2, guid: "rename", text: nil, handleID: 1,
                              isFromMe: false, offset: 160, itemType: 2)
        }
        let rows = try window(queue, from: 100, through: 200)
        XCTAssertEqual(rows.map(\.guid), ["real"])
    }

    func testRichTextBodyDecodedAndAttachmentOnlyRowHasNoBody() throws {
        let queue = try makeFixture()
        try queue.write { db in
            try insertMessage(db: db, rowID: 1, guid: "rich", text: nil,
                              attributedBody: typedstream("rich hello"),
                              handleID: 1, isFromMe: false, offset: 150)
            try insertMessage(db: db, rowID: 2, guid: "photo", text: nil, handleID: 1,
                              isFromMe: false, offset: 160)
            try db.execute(sql:
                "INSERT INTO attachment (ROWID, guid, uti) VALUES (1, 'att-1', 'public.jpeg')")
            try db.execute(sql:
                "INSERT INTO message_attachment_join (message_id, attachment_id) VALUES (2, 1)")
        }
        let rows = try window(queue, from: 100, through: 200)
        XCTAssertEqual(rows.map(\.guid), ["rich", "photo"])
        XCTAssertEqual(rows[0].text, "rich hello")
        XCTAssertNil(rows[1].text)
    }

    func testInboundCarriesSenderHandleAndOutboundIsFromMe() throws {
        let queue = try makeFixture()
        try queue.write { db in
            try insertMessage(db: db, rowID: 1, guid: "in", handleID: 2, isFromMe: false, offset: 150)
            try insertMessage(db: db, rowID: 2, guid: "out", handleID: nil, isFromMe: true, offset: 160)
        }
        let rows = try window(queue, from: 100, through: 200)
        XCTAssertEqual(rows.map(\.guid), ["in", "out"])
        XCTAssertEqual(rows[0].peerHandleRaw, "bar@example.com")
        XCTAssertFalse(rows[0].isFromMe)
        XCTAssertTrue(rows[0].isGroup)
        XCTAssertTrue(rows[1].isFromMe)
        XCTAssertTrue(rows[1].isGroup)
        XCTAssertEqual(rows[1].chatGUID, "iMessage;+;groupX")
    }

    /// Typedstream bytes shaped like the ones Messages.app stores in
    /// `message.attributedBody` for an NSAttributedString holding `body`
    /// (same shape the body-capture tests use).
    private func typedstream(_ body: String) -> Data {
        let utf8 = Array(body.utf8)
        var bytes: [UInt8] = [0x04, 0x0B]
        bytes += Array("streamtyped".utf8)
        bytes += [0x81, 0xE8, 0x03, 0x84, 0x01, 0x40, 0x84, 0x84, 0x84, 0x12]
        bytes += Array("NSAttributedString".utf8)
        bytes += [0x00, 0x84, 0x84, 0x08]
        bytes += Array("NSObject".utf8)
        bytes += [0x00, 0x85, 0x92, 0x84, 0x84, 0x84, 0x08]
        bytes += Array("NSString".utf8)
        bytes += [0x01, 0x94, 0x84, 0x01, 0x2B]
        bytes.append(UInt8(utf8.count))
        bytes += utf8
        bytes += [0x86, 0x84, 0x02]
        bytes += Array("iI".utf8)
        bytes += [0x01, UInt8(body.utf16.count), 0x92, 0x84, 0x84, 0x84, 0x0C]
        bytes += Array("NSDictionary".utf8)
        bytes += [0x00, 0x94, 0x84, 0x01, 0x69, 0x01, 0x92, 0x84, 0x96, 0x96, 0x1D]
        bytes += Array("__kIMMessagePartAttributeName".utf8)
        bytes += [0x86, 0x86, 0x86]
        return Data(bytes)
    }
}
