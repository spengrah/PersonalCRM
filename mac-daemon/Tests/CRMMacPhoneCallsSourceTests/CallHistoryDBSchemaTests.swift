import XCTest
import GRDB
@testable import CRMMacPhoneCallsSource

final class CallHistoryDBSchemaTests: XCTestCase {
    func testValidatorReportsOkOnCompleteSchema() throws {
        let queue = try InMemoryCallHistoryDB.makeQueue()
        let result = try queue.read { try CallHistoryDBSchemaValidator.validate(db: $0) }
        XCTAssertEqual(result, .ok)
        XCTAssertEqual(result.label, "call_history_db_v1")
    }

    func testValidatorDetectsMissingZHasMessage() throws {
        // Build a CallHistoryDB without ZHASMESSAGE and confirm drift
        // is reported.
        let queue = try DatabaseQueue()
        try queue.write { db in
            try db.execute(sql: """
                CREATE TABLE ZCALLRECORD (
                    Z_PK INTEGER PRIMARY KEY AUTOINCREMENT,
                    ZUNIQUE_ID TEXT NOT NULL,
                    ZDATE REAL NOT NULL,
                    ZADDRESS TEXT,
                    ZORIGINATED INTEGER,
                    ZANSWERED INTEGER,
                    ZDURATION REAL,
                    ZSERVICE_PROVIDER TEXT,
                    ZCALLTYPE INTEGER
                    -- ZHASMESSAGE deliberately missing
                );
                """)
        }
        let result = try queue.read { try CallHistoryDBSchemaValidator.validate(db: $0) }
        guard case .drift(let table, let missing) = result else {
            return XCTFail("expected drift, got \(result)")
        }
        XCTAssertEqual(table, "ZCALLRECORD")
        XCTAssertTrue(missing.contains("ZHASMESSAGE"))
        XCTAssertEqual(result.label, "call_history_db_drift:ZCALLRECORD.ZHASMESSAGE")
    }

    func testValidatorDetectsMissingZUniqueID() throws {
        let queue = try DatabaseQueue()
        try queue.write { db in
            try db.execute(sql: """
                CREATE TABLE ZCALLRECORD (
                    Z_PK INTEGER PRIMARY KEY AUTOINCREMENT,
                    ZDATE REAL NOT NULL,
                    ZADDRESS TEXT,
                    ZORIGINATED INTEGER,
                    ZANSWERED INTEGER,
                    ZDURATION REAL,
                    ZSERVICE_PROVIDER TEXT,
                    ZCALLTYPE INTEGER,
                    ZHASMESSAGE INTEGER
                    -- ZUNIQUE_ID deliberately missing
                );
                """)
        }
        let result = try queue.read { try CallHistoryDBSchemaValidator.validate(db: $0) }
        guard case .drift(_, let missing) = result else {
            return XCTFail("expected drift")
        }
        XCTAssertTrue(missing.contains("ZUNIQUE_ID"))
    }

    func testValidatorIsCaseInsensitive() throws {
        // SQLite identifiers are case-insensitive; the validator
        // mirrors this. Build the table with lower-case columns and
        // verify .ok.
        let queue = try DatabaseQueue()
        try queue.write { db in
            try db.execute(sql: """
                CREATE TABLE ZCALLRECORD (
                    z_pk INTEGER PRIMARY KEY AUTOINCREMENT,
                    zunique_id TEXT NOT NULL,
                    zdate REAL NOT NULL,
                    zaddress TEXT,
                    zoriginated INTEGER,
                    zanswered INTEGER,
                    zduration REAL,
                    zservice_provider TEXT,
                    zcalltype INTEGER,
                    zhasmessage INTEGER
                );
                """)
        }
        let result = try queue.read { try CallHistoryDBSchemaValidator.validate(db: $0) }
        XCTAssertEqual(result, .ok)
    }
}
