// MessagesScanReaderTests — the identifier-scoped, date-bounded,
// resumable scan reader over chat.db.
//
// Synthetic handles only (+15550000001, test@example.com); no real PII.
import XCTest
import GRDB
@testable import CRMMacMessagesSource

final class MessagesScanReaderTests: XCTestCase {
    // 2026-04-01 UTC — comfortably inside the 30-day window for the
    // tests that use a 2026-05 `since`, and after the backfill floor.
    private let unixApr2026: TimeInterval = 1_775_001_600 // 2026-04-01
    private let unixMay2026: TimeInterval = 1_777_680_000 // 2026-05-02

    private func insertHandle(db: Database, rowID: Int64, id: String) throws {
        try db.execute(
            sql: "INSERT INTO handle (ROWID, id, service) VALUES (?, ?, 'iMessage')",
            arguments: [rowID, id])
    }

    private func insertMessage(
        db: Database,
        rowID: Int64,
        guid: String,
        handleID: Int64,
        unixDate: TimeInterval,
        chatRowID: Int64,
        chatGUID: String,
        chatStyle: Int64 = 45
    ) throws {
        // Create the chat row once per chatRowID (ignore duplicates).
        try db.execute(
            sql: "INSERT OR IGNORE INTO chat (ROWID, guid, style, chat_identifier) VALUES (?, ?, ?, ?)",
            arguments: [chatRowID, chatGUID, chatStyle, chatGUID])
        let appleNanos = InMemoryChatDB.appleEpochNanos(unix: unixDate)
        try db.execute(
            sql: """
                INSERT INTO message (ROWID, guid, text, handle_id, date,
                                     is_from_me, cache_has_attachments,
                                     associated_message_guid)
                VALUES (?, ?, 'hi', ?, ?, 0, 0, NULL)
                """,
            arguments: [rowID, guid, handleID, appleNanos])
        try db.execute(
            sql: "INSERT INTO chat_message_join (chat_id, message_id) VALUES (?, ?)",
            arguments: [chatRowID, rowID])
    }

    // MARK: - handle ROWID resolution (alternate spellings)

    func testResolvesMultipleROWIDsForOneCanonicalHandle() throws {
        let queue = try InMemoryChatDB.makeQueue()
        try queue.write { db in
            // Two raw spellings of the SAME canonical number.
            try insertHandle(db: db, rowID: 1, id: "+15550000001")
            try insertHandle(db: db, rowID: 2, id: "(555) 000-0001")
            // An unrelated handle.
            try insertHandle(db: db, rowID: 3, id: "+15559999999")
        }
        let resolved = try queue.read { db in
            try MessagesScanReader.resolveHandleROWIDs(db: db, canonicalHandle: "+15550000001")
        }
        XCTAssertEqual(Set(resolved), [1, 2])
    }

    func testScanFindsRowsUnderAllSpellings() throws {
        let queue = try InMemoryChatDB.makeQueue()
        try queue.write { db in
            try insertHandle(db: db, rowID: 1, id: "+15550000001")
            try insertHandle(db: db, rowID: 2, id: "(555) 000-0001")
            try insertMessage(db: db, rowID: 100, guid: "g1", handleID: 1,
                              unixDate: unixApr2026, chatRowID: 10,
                              chatGUID: "chatA")
            try insertMessage(db: db, rowID: 101, guid: "g2", handleID: 2,
                              unixDate: unixApr2026, chatRowID: 11,
                              chatGUID: "chatB")
        }
        let page = try queue.read { db in
            try MessagesScanReader.scanPage(
                db: db, canonicalHandle: "+15550000001",
                since: Date(timeIntervalSince1970: 1_767_225_600),
                progressBelowRowID: nil, limit: 100)
        }
        XCTAssertEqual(Set(page.rows.map(\.guid)), ["g1", "g2"])
        XCTAssertTrue(page.exhausted)
    }

    // MARK: - date window boundary

    func testDateWindowBoundary() throws {
        let queue = try InMemoryChatDB.makeQueue()
        let sinceUnix = unixMay2026
        try queue.write { db in
            try insertHandle(db: db, rowID: 1, id: "+15550000001")
            // Exactly at `since` → included.
            try insertMessage(db: db, rowID: 100, guid: "at", handleID: 1,
                              unixDate: sinceUnix, chatRowID: 10, chatGUID: "c")
            // Just below `since` → excluded.
            try insertMessage(db: db, rowID: 101, guid: "below", handleID: 1,
                              unixDate: sinceUnix - 60, chatRowID: 10, chatGUID: "c")
            // Well above `since` → included.
            try insertMessage(db: db, rowID: 102, guid: "above", handleID: 1,
                              unixDate: sinceUnix + 86_400, chatRowID: 10, chatGUID: "c")
        }
        let page = try queue.read { db in
            try MessagesScanReader.scanPage(
                db: db, canonicalHandle: "+15550000001",
                since: Date(timeIntervalSince1970: sinceUnix),
                progressBelowRowID: nil, limit: 100)
        }
        XCTAssertEqual(Set(page.rows.map(\.guid)), ["at", "above"])
    }

    // MARK: - no match

    func testNoMatchHandleReturnsEmptyExhausted() throws {
        let queue = try InMemoryChatDB.makeQueue()
        try queue.write { db in
            try insertHandle(db: db, rowID: 1, id: "+15550000001")
            try insertMessage(db: db, rowID: 100, guid: "g1", handleID: 1,
                              unixDate: unixApr2026, chatRowID: 10, chatGUID: "c")
        }
        let page = try queue.read { db in
            try MessagesScanReader.scanPage(
                db: db, canonicalHandle: "+15558888888",
                since: Date(timeIntervalSince1970: 1_767_225_600),
                progressBelowRowID: nil, limit: 100)
        }
        XCTAssertTrue(page.rows.isEmpty)
        XCTAssertTrue(page.exhausted)
        XCTAssertNil(page.lowestRowID)
    }

    // MARK: - group rows

    func testGroupChatRowUnderScannedHandleIsReturned() throws {
        let queue = try InMemoryChatDB.makeQueue()
        try queue.write { db in
            try insertHandle(db: db, rowID: 1, id: "+15550000001")
            // style 43 = group chat.
            try insertMessage(db: db, rowID: 100, guid: "grp", handleID: 1,
                              unixDate: unixApr2026, chatRowID: 20,
                              chatGUID: "groupX", chatStyle: 43)
        }
        let page = try queue.read { db in
            try MessagesScanReader.scanPage(
                db: db, canonicalHandle: "+15550000001",
                since: Date(timeIntervalSince1970: 1_767_225_600),
                progressBelowRowID: nil, limit: 100)
        }
        let row = try XCTUnwrap(page.rows.first)
        XCTAssertEqual(row.guid, "grp")
        XCTAssertTrue(row.isGroup)
    }

    // MARK: - budget / resumability

    func testBudgetLimitAndResumeBelowProgress() throws {
        let queue = try InMemoryChatDB.makeQueue()
        try queue.write { db in
            try insertHandle(db: db, rowID: 1, id: "+15550000001")
            for i in 0..<5 {
                try insertMessage(db: db, rowID: Int64(200 + i), guid: "g\(i)",
                                  handleID: 1, unixDate: unixApr2026,
                                  chatRowID: 10, chatGUID: "c")
            }
        }
        // First page: budget 2 → highest two ROWIDs (descending), not
        // exhausted.
        let first = try queue.read { db in
            try MessagesScanReader.scanPage(
                db: db, canonicalHandle: "+15550000001",
                since: Date(timeIntervalSince1970: 1_767_225_600),
                progressBelowRowID: nil, limit: 2)
        }
        XCTAssertEqual(first.rows.map(\.rowID), [204, 203])
        XCTAssertFalse(first.exhausted)
        XCTAssertEqual(first.lowestRowID, 203)

        // Resume strictly below the lowest of the first page.
        let second = try queue.read { db in
            try MessagesScanReader.scanPage(
                db: db, canonicalHandle: "+15550000001",
                since: Date(timeIntervalSince1970: 1_767_225_600),
                progressBelowRowID: first.lowestRowID, limit: 2)
        }
        XCTAssertEqual(second.rows.map(\.rowID), [202, 201])
        XCTAssertFalse(second.exhausted)

        // Final page returns the last row and reports exhausted.
        let third = try queue.read { db in
            try MessagesScanReader.scanPage(
                db: db, canonicalHandle: "+15550000001",
                since: Date(timeIntervalSince1970: 1_767_225_600),
                progressBelowRowID: second.lowestRowID, limit: 2)
        }
        XCTAssertEqual(third.rows.map(\.rowID), [200])
        XCTAssertTrue(third.exhausted)
    }

    func testEmailHandleScan() throws {
        let queue = try InMemoryChatDB.makeQueue()
        try queue.write { db in
            try insertHandle(db: db, rowID: 1, id: "Test@Example.com")
            try insertMessage(db: db, rowID: 100, guid: "e1", handleID: 1,
                              unixDate: unixApr2026, chatRowID: 10, chatGUID: "c")
        }
        let page = try queue.read { db in
            try MessagesScanReader.scanPage(
                db: db, canonicalHandle: "test@example.com",
                since: Date(timeIntervalSince1970: 1_767_225_600),
                progressBelowRowID: nil, limit: 100)
        }
        XCTAssertEqual(page.rows.map(\.guid), ["e1"])
    }
}
