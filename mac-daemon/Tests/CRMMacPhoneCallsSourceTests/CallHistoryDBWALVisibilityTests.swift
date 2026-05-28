// Regression test for the bug where CallHistoryDB was opened with
// `?mode=ro&immutable=1`. SQLite's `immutable=1` flag tells the
// reader to skip the WAL entirely and serve only the main DB file —
// but macOS doesn't checkpoint CallHistoryDB on any predictable
// cadence (the WAL accumulates for days between checkpoints), so an
// immutable reader is blind to every call written since the last
// checkpoint. The daemon ran for weeks in this state and missed
// every phone call.
//
// This test simulates the production scenario:
//   1. Create a CallHistoryDB-shaped file with N rows committed +
//      checkpointed into the main DB file.
//   2. Open a SECOND writer connection, INSERT M more rows, close it
//      WITHOUT checkpointing. Those rows live in the -wal sidecar.
//   3. Open the DB via the production read path (`file://...?mode=ro`)
//      and assert all N+M rows are visible.
//
// The bug was that step 3 used `?mode=ro&immutable=1`, which returned
// only N rows. With the fix, step 3 returns N+M.
import XCTest
import GRDB
import CRMMacCore
@testable import CRMMacPhoneCallsSource

final class CallHistoryDBWALVisibilityTests: XCTestCase {
    private var tempDir: URL!

    override func setUpWithError() throws {
        tempDir = URL(fileURLWithPath: NSTemporaryDirectory())
            .appendingPathComponent("crm-mac-wal-test-\(UUID().uuidString)",
                                    isDirectory: true)
        try FileManager.default.createDirectory(at: tempDir,
                                                 withIntermediateDirectories: true)
    }

    override func tearDownWithError() throws {
        if let dir = tempDir {
            try? FileManager.default.removeItem(at: dir)
        }
    }

    /// Writer connections must be configured with WAL journal mode and
    /// no automatic checkpoint so the test deterministically leaves
    /// uncommitted-to-main rows in the WAL sidecar.
    private func makeWriterConfig() -> Configuration {
        var config = Configuration()
        config.readonly = false
        config.prepareDatabase { db in
            try db.execute(sql: "PRAGMA journal_mode = WAL")
            try db.execute(sql: "PRAGMA wal_autocheckpoint = 0")
        }
        return config
    }

    private func createSchema(at path: String) throws {
        let queue = try DatabaseQueue(path: path, configuration: makeWriterConfig())
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
                    ZCALLTYPE INTEGER,
                    ZHASMESSAGE INTEGER
                );
                """)
        }
    }

    private func insertRow(
        into queue: DatabaseQueue, uniqueID: String, zdate: Double
    ) throws {
        try queue.write { db in
            try db.execute(sql: """
                INSERT INTO ZCALLRECORD (
                    ZUNIQUE_ID, ZDATE, ZADDRESS,
                    ZORIGINATED, ZANSWERED, ZDURATION,
                    ZSERVICE_PROVIDER, ZCALLTYPE, ZHASMESSAGE)
                VALUES (?, ?, '+15551234567', 0, 1, 30,
                        'com.apple.Telephony', 0, 0);
                """, arguments: [uniqueID, zdate])
        }
    }

    /// 2025-01-01T00:00:00Z in Apple-epoch seconds.
    private let baseZDate: Double = 1_735_689_600 - 978_307_200

    /// Production-path open: reuses `SQLiteSnapshotReader.readOnlyURI`
    /// to build the exact URI production uses, so a future change to the
    /// URI shape lands in both call sites simultaneously.
    private func openProductionReader(at path: String) throws -> DatabasePool {
        var config = Configuration()
        config.readonly = true
        let uri = SQLiteSnapshotReader.readOnlyURI(for: URL(fileURLWithPath: path))
        return try DatabasePool(path: uri, configuration: config)
    }

    /// Core regression assertion: rows inserted via a separate writer
    /// connection that did NOT checkpoint must still be visible to the
    /// production-path reader. With `?mode=ro&immutable=1` (the bug),
    /// this test fails — only the pre-checkpoint rows are returned.
    func testReaderSeesWALResidentWrites() throws {
        let dbPath = tempDir.appendingPathComponent("CallHistory.storedata").path

        // Step 1: create schema + insert 3 rows + force a checkpoint
        // so they live in the main DB file.
        try createSchema(at: dbPath)
        do {
            let queue = try DatabaseQueue(path: dbPath, configuration: makeWriterConfig())
            for i in 0..<3 {
                try insertRow(into: queue,
                              uniqueID: "checkpoint-\(i)",
                              zdate: baseZDate + Double(i))
            }
            // Force the WAL to fold into the main DB file. After this
            // returns, the main file contains all 3 rows and the WAL is
            // empty.
            try queue.writeWithoutTransaction { db in
                try db.execute(sql: "PRAGMA wal_checkpoint(TRUNCATE)")
            }
            // queue goes out of scope and is released.
        }

        // Step 2: open a second writer, insert 4 more rows, close it
        // WITHOUT checkpointing. Those rows now live in the WAL
        // sidecar and would be invisible to an immutable reader.
        do {
            let queue = try DatabaseQueue(path: dbPath, configuration: makeWriterConfig())
            for i in 0..<4 {
                try insertRow(into: queue,
                              uniqueID: "wal-only-\(i)",
                              zdate: baseZDate + 100 + Double(i))
            }
            // No explicit checkpoint. queue is released; WAL persists.
        }

        // Sanity: confirm the WAL file actually exists on disk. If
        // SQLite folded the WAL on close for some reason, this test
        // wouldn't be exercising the regression.
        let walPath = dbPath + "-wal"
        XCTAssertTrue(FileManager.default.fileExists(atPath: walPath),
                      "expected -wal sidecar to exist; otherwise the test " +
                      "isn't exercising the WAL-visibility regression")

        // Step 3: open via the production read path and assert all 7
        // rows are visible.
        let pool = try openProductionReader(at: dbPath)
        let page = try pool.read { db in
            try CallHistoryDBReader.fetchPage(
                db: db,
                direction: .forwardFromExclusive(zdate: 0, zPK: 0),
                limit: 100)
        }
        XCTAssertEqual(page.rows.count, 7,
                       "reader must see both checkpointed (3) and WAL-only " +
                       "(4) rows — total 7. Got \(page.rows.count). If this " +
                       "fails with 3, `?mode=ro&immutable=1` has been " +
                       "re-introduced.")
        let uniqueIDs = Set(page.rows.map { $0.uniqueID })
        XCTAssertTrue(uniqueIDs.contains("checkpoint-0"))
        XCTAssertTrue(uniqueIDs.contains("wal-only-0"))
        XCTAssertTrue(uniqueIDs.contains("wal-only-3"))
    }

    /// Companion test: live cursor advancement also sees WAL-only
    /// writes. Mirrors the daemon's per-tick "fetch rows past the
    /// committed live cursor" pattern.
    func testReaderSeesNewWALRowsAfterCursorAdvance() throws {
        let dbPath = tempDir.appendingPathComponent("CallHistory.storedata").path
        try createSchema(at: dbPath)

        // Initial committed row.
        do {
            let queue = try DatabaseQueue(path: dbPath, configuration: makeWriterConfig())
            try insertRow(into: queue, uniqueID: "initial",
                          zdate: baseZDate + 10)
            try queue.writeWithoutTransaction { db in
                try db.execute(sql: "PRAGMA wal_checkpoint(TRUNCATE)")
            }
        }

        // First reader open: sees the one initial row.
        do {
            let pool = try openProductionReader(at: dbPath)
            let page = try pool.read { db in
                try CallHistoryDBReader.fetchPage(
                    db: db,
                    direction: .forwardFromExclusive(zdate: 0, zPK: 0),
                    limit: 100)
            }
            XCTAssertEqual(page.rows.count, 1)
            XCTAssertEqual(page.rows[0].uniqueID, "initial")
        }

        // Writer adds a new row WITHOUT checkpointing.
        do {
            let queue = try DatabaseQueue(path: dbPath, configuration: makeWriterConfig())
            try insertRow(into: queue, uniqueID: "wal-new",
                          zdate: baseZDate + 20)
        }

        // Second reader open: must see BOTH rows. With `immutable=1`,
        // the second open would return only the initial row.
        do {
            let pool = try openProductionReader(at: dbPath)
            let page = try pool.read { db in
                try CallHistoryDBReader.fetchPage(
                    db: db,
                    direction: .forwardFromExclusive(zdate: baseZDate + 10, zPK: 1),
                    limit: 100)
            }
            XCTAssertEqual(page.rows.count, 1,
                           "reader must observe the WAL-only row past the " +
                           "advanced cursor; got \(page.rows.count) rows")
            XCTAssertEqual(page.rows[0].uniqueID, "wal-new")
        }
    }
}
