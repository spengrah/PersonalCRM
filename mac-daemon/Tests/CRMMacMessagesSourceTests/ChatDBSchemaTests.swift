import XCTest
import GRDB
@testable import CRMMacMessagesSource

final class ChatDBSchemaTests: XCTestCase {
    func testOKOnFullFixture() throws {
        let queue = try InMemoryChatDB.makeQueue()
        let health = try queue.read { db in
            try ChatDBSchemaValidator.validate(db: db)
        }
        XCTAssertEqual(health, .ok)
        XCTAssertEqual(health.label, "chat_db_v2")
    }

    func testDriftOnMissingItemType() throws {
        let queue = try DatabaseQueue()
        try queue.write { db in
            // Full schema EXCEPT message.item_type is missing.
            try db.execute(sql: """
                CREATE TABLE handle (
                    ROWID INTEGER PRIMARY KEY, id TEXT, service TEXT
                );
                CREATE TABLE chat (
                    ROWID INTEGER PRIMARY KEY, guid TEXT, style INTEGER,
                    chat_identifier TEXT
                );
                CREATE TABLE chat_handle_join (chat_id INTEGER, handle_id INTEGER);
                CREATE TABLE message (
                    ROWID INTEGER PRIMARY KEY, guid TEXT, text TEXT,
                    handle_id INTEGER, date INTEGER, is_from_me INTEGER,
                    cache_has_attachments INTEGER, associated_message_guid TEXT
                );
                CREATE TABLE chat_message_join (chat_id INTEGER, message_id INTEGER);
                CREATE TABLE attachment (
                    ROWID INTEGER PRIMARY KEY, guid TEXT, uti TEXT,
                    mime_type TEXT, transfer_name TEXT, total_bytes INTEGER
                );
                CREATE TABLE message_attachment_join (
                    message_id INTEGER, attachment_id INTEGER
                );
                """)
        }
        let health = try queue.read { db in
            try ChatDBSchemaValidator.validate(db: db)
        }
        switch health {
        case .drift(let table, let missing):
            XCTAssertEqual(table, "message")
            XCTAssertTrue(missing.contains("item_type"),
                          "expected item_type in missing set, got \(missing)")
        case .ok:
            XCTFail("expected drift on missing item_type, got ok")
        }
    }

    func testDriftOnMissingColumn() throws {
        let queue = try DatabaseQueue()
        try queue.write { db in
            // Build a chat.db schema with `message.guid` deliberately
            // missing. Other tables are full.
            try db.execute(sql: """
                CREATE TABLE handle (
                    ROWID INTEGER PRIMARY KEY,
                    id TEXT,
                    service TEXT
                );
                CREATE TABLE chat (
                    ROWID INTEGER PRIMARY KEY,
                    guid TEXT,
                    style INTEGER,
                    chat_identifier TEXT
                );
                CREATE TABLE chat_handle_join (
                    chat_id INTEGER,
                    handle_id INTEGER
                );
                CREATE TABLE message (
                    ROWID INTEGER PRIMARY KEY,
                    text TEXT,
                    handle_id INTEGER,
                    date INTEGER,
                    is_from_me INTEGER,
                    cache_has_attachments INTEGER,
                    associated_message_guid TEXT
                );
                CREATE TABLE chat_message_join (
                    chat_id INTEGER,
                    message_id INTEGER
                );
                CREATE TABLE attachment (
                    ROWID INTEGER PRIMARY KEY,
                    guid TEXT,
                    uti TEXT,
                    mime_type TEXT,
                    transfer_name TEXT,
                    total_bytes INTEGER
                );
                CREATE TABLE message_attachment_join (
                    message_id INTEGER,
                    attachment_id INTEGER
                );
                """)
        }

        let health = try queue.read { db in
            try ChatDBSchemaValidator.validate(db: db)
        }
        switch health {
        case .drift(let table, let missing):
            XCTAssertEqual(table, "message")
            XCTAssertTrue(missing.contains("guid"),
                          "expected guid in missing set, got \(missing)")
        case .ok:
            XCTFail("expected drift, got ok")
        }
        XCTAssertTrue(health.label.hasPrefix("chat_db_drift:message."))
    }

    func testDriftOnMissingTable() throws {
        // Empty database — every table is missing.
        let queue = try DatabaseQueue()
        let health = try queue.read { db in
            try ChatDBSchemaValidator.validate(db: db)
        }
        if case .drift = health {
            // Good.
        } else {
            XCTFail("expected drift on empty DB")
        }
    }
}
