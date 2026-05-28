// Regression test for chat.db WAL visibility through the production
// read path (`SQLiteSnapshotReader.readOnlyURI`). Mirrors
// CallHistoryDBWALVisibilityTests on the phone_calls side.
//
// Messages.app writes chat.db in WAL mode continuously. Even though
// Messages.app checkpoints more aggressively than CallHistoryDB, any
// write made since the last checkpoint lives in the -wal sidecar. If
// the reader were ever opened with `?...&immutable=1`, SQLite would
// serve only the main DB file and ignore the -wal sidecar — blinding
// the daemon to every such uncheckpointed message. This locks in that
// the bare-path -> SQLiteSnapshotReader URI conversion did not regress
// WAL visibility, and guards against a future immutable=1 slip.
//
// This test exercises the production scenario end-to-end and is
// deliberately FILE-BACKED (not InMemoryChatDB): an in-memory DB has
// no -wal sidecar and cannot exercise WAL-resident-write visibility.
//   1. Build a chat.db-shaped file from the committed schema fixture,
//      insert N rows, force a checkpoint so they fold into the main
//      file, close.
//   2. Open a second writer, insert M more rows, close WITHOUT
//      checkpointing — those rows live in the -wal sidecar.
//   3. Open via the production URI builder and assert all N+M rows are
//      visible through ChatDBReader.fetchPage.
import XCTest
import GRDB
import CRMMacCore
@testable import CRMMacMessagesSource

final class ChatDBWALVisibilityTests: XCTestCase {
    private var tempDir: URL!

    override func setUpWithError() throws {
        tempDir = URL(fileURLWithPath: NSTemporaryDirectory())
            .appendingPathComponent("crm-mac-chatdb-wal-test-\(UUID().uuidString)",
                                    isDirectory: true)
        try FileManager.default.createDirectory(at: tempDir,
                                                withIntermediateDirectories: true)
    }

    override func tearDownWithError() throws {
        if let dir = tempDir {
            try? FileManager.default.removeItem(at: dir)
        }
    }

    /// Writer connections use WAL journal mode and no automatic
    /// checkpoint so the test deterministically leaves
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

    /// Load the committed chat.db schema fixture into a fresh
    /// file-backed DB + seed the one handle + chat the reader joins to.
    private func createSchema(at path: String) throws {
        let bundle = Bundle.module
        guard let scriptURL = bundle.url(forResource: "chat_db_schema",
                                         withExtension: "sql",
                                         subdirectory: "Fixtures") else {
            throw NSError(domain: "ChatDBWALVisibilityTests", code: 1,
                          userInfo: [NSLocalizedDescriptionKey:
                                     "chat_db_schema.sql not found in Bundle.module/Fixtures"])
        }
        let script = try String(contentsOf: scriptURL, encoding: .utf8)
        let queue = try DatabaseQueue(path: path, configuration: makeWriterConfig())
        try queue.write { db in
            try db.execute(sql: script)
            // One inbound peer handle + one 1:1 chat the reader joins.
            try db.execute(sql:
                "INSERT INTO handle (ROWID, id, service) VALUES (1, '+15551234567', 'iMessage')")
            try db.execute(sql:
                "INSERT INTO chat (ROWID, guid, style) VALUES (1, 'chat-guid-1', 45)")
        }
    }

    /// Insert one inbound message row + its chat_message_join. `date`
    /// is Apple-epoch nanoseconds for a 2025 timestamp, comfortably
    /// above the reader's corrupt-date sentinel floor.
    private func insertMessage(
        into queue: DatabaseQueue, rowID: Int64, guid: String, unixDate: TimeInterval
    ) throws {
        let appleDate = InMemoryChatDB.appleEpochNanos(unix: unixDate)
        try queue.write { db in
            try db.execute(sql: """
                INSERT INTO message (ROWID, guid, text, handle_id, date,
                                     is_from_me, cache_has_attachments)
                VALUES (?, ?, 'hi', 1, ?, 0, 0)
                """, arguments: [rowID, guid, appleDate])
            try db.execute(sql:
                "INSERT INTO chat_message_join (chat_id, message_id) VALUES (1, ?)",
                arguments: [rowID])
        }
    }

    /// 2025-01-01T00:00:00Z in UNIX seconds.
    private let baseUnix: TimeInterval = 1_735_689_600

    /// Production-path open: reuses `SQLiteSnapshotReader.readOnlyURI`
    /// to build the exact URI production uses, so a future change to
    /// the URI shape lands in both call sites simultaneously.
    private func openProductionReader(at path: String) throws -> DatabasePool {
        var config = Configuration()
        config.readonly = true
        let uri = SQLiteSnapshotReader.readOnlyURI(for: URL(fileURLWithPath: path))
        return try DatabasePool(path: uri, configuration: config)
    }

    /// Core regression assertion: rows inserted via a separate writer
    /// connection that did NOT checkpoint must still be visible to the
    /// production-path reader. With `immutable=1` (the regression),
    /// this fails — only the pre-checkpoint rows are returned.
    func testReaderSeesWALResidentWrites() throws {
        let dbPath = tempDir.appendingPathComponent("chat.db").path

        // Step 1: schema + 3 rows + force a checkpoint into the main file.
        try createSchema(at: dbPath)
        do {
            let queue = try DatabaseQueue(path: dbPath, configuration: makeWriterConfig())
            for i in 0..<3 {
                try insertMessage(into: queue, rowID: Int64(100 + i),
                                  guid: "checkpoint-\(i)", unixDate: baseUnix + Double(i))
            }
            try queue.writeWithoutTransaction { db in
                try db.execute(sql: "PRAGMA wal_checkpoint(TRUNCATE)")
            }
        }

        // Step 2: second writer, 4 more rows, close WITHOUT checkpoint.
        do {
            let queue = try DatabaseQueue(path: dbPath, configuration: makeWriterConfig())
            for i in 0..<4 {
                try insertMessage(into: queue, rowID: Int64(200 + i),
                                  guid: "wal-only-\(i)", unixDate: baseUnix + 100 + Double(i))
            }
        }

        // Sanity: the -wal sidecar must actually exist, otherwise the
        // test isn't exercising the regression.
        let walPath = dbPath + "-wal"
        XCTAssertTrue(FileManager.default.fileExists(atPath: walPath),
                      "expected -wal sidecar to exist; otherwise the test " +
                      "isn't exercising the WAL-visibility regression")

        // Step 3: open via the production read path; assert all 7 rows.
        let pool = try openProductionReader(at: dbPath)
        let page = try pool.read { db in
            try ChatDBReader.fetchPage(
                db: db,
                direction: .forwardFromExclusive(0),
                limit: 100)
        }
        XCTAssertEqual(page.rows.count, 7,
                       "reader must see both checkpointed (3) and WAL-only " +
                       "(4) rows — total 7. Got \(page.rows.count). If this " +
                       "fails with 3, `immutable=1` has been re-introduced " +
                       "on chat.db.")
        let guids = Set(page.rows.map { $0.guid })
        XCTAssertTrue(guids.contains("checkpoint-0"))
        XCTAssertTrue(guids.contains("wal-only-0"))
        XCTAssertTrue(guids.contains("wal-only-3"))
    }

    /// Companion: a reader opened AFTER a writer adds an uncheckpointed
    /// row past the prior live cursor still observes it. Mirrors the
    /// daemon's per-tick "fetch rows past the committed live cursor".
    func testReaderSeesNewWALRowsAfterCursorAdvance() throws {
        let dbPath = tempDir.appendingPathComponent("chat.db").path
        try createSchema(at: dbPath)

        do {
            let queue = try DatabaseQueue(path: dbPath, configuration: makeWriterConfig())
            try insertMessage(into: queue, rowID: 100, guid: "initial",
                              unixDate: baseUnix + 10)
            try queue.writeWithoutTransaction { db in
                try db.execute(sql: "PRAGMA wal_checkpoint(TRUNCATE)")
            }
        }

        do {
            let pool = try openProductionReader(at: dbPath)
            let page = try pool.read { db in
                try ChatDBReader.fetchPage(
                    db: db, direction: .forwardFromExclusive(0), limit: 100)
            }
            XCTAssertEqual(page.rows.count, 1)
            XCTAssertEqual(page.rows[0].guid, "initial")
        }

        // Writer adds a new row WITHOUT checkpointing.
        do {
            let queue = try DatabaseQueue(path: dbPath, configuration: makeWriterConfig())
            try insertMessage(into: queue, rowID: 200, guid: "wal-new",
                              unixDate: baseUnix + 20)
        }

        // Second reader open: must see the WAL-only row past the cursor.
        do {
            let pool = try openProductionReader(at: dbPath)
            let page = try pool.read { db in
                try ChatDBReader.fetchPage(
                    db: db, direction: .forwardFromExclusive(100), limit: 100)
            }
            XCTAssertEqual(page.rows.count, 1,
                           "reader must observe the WAL-only row past the " +
                           "advanced cursor; got \(page.rows.count) rows")
            XCTAssertEqual(page.rows[0].guid, "wal-new")
        }
    }
}
