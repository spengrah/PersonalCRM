// SQLiteSnapshotReaderTests — verifies the read-only WAL-aware URI the
// helper produces opens the SAME file as a direct bare-path open, for
// every path-byte class the helper's doc comment claims to support.
//
// The contract is load-bearing in production: the real macOS DBs live
// under `~/Library/Application Support/...`, whose path contains a
// SPACE — encoded to `%20` and round-tripped back by SQLite's `file:`
// URI parser. The `%` case is the sharpest (a raw `%` is an invalid URI
// escape; encoding it to `%25` is what makes it round-trip).
//
// Each test writes a row through a bare-path DatabaseQueue at the EXACT
// special-character path, then opens via
// `SQLiteSnapshotReader.readOnlyURI(for:)` and asserts the row is
// visible — proving both opens resolve to the same file. (This test
// lives in a test target, which the SQLiteURILiteralGuardTests grep
// does NOT scan, so the bare-path writer open is allowed here.)
//
// If any case ever fails to round-trip via GRDB/SQLite, NARROW the
// helper's documented contract to the characters that do work rather
// than claiming broader support than this proves.
import XCTest
import GRDB
import CRMMacCore

final class SQLiteSnapshotReaderTests: XCTestCase {
    private var tempDir: URL!

    override func setUpWithError() throws {
        tempDir = URL(fileURLWithPath: NSTemporaryDirectory())
            .appendingPathComponent("crm-mac-uri-test-\(UUID().uuidString)", isDirectory: true)
        try FileManager.default.createDirectory(at: tempDir, withIntermediateDirectories: true)
    }

    override func tearDownWithError() throws {
        if let dir = tempDir { try? FileManager.default.removeItem(at: dir) }
    }

    /// Create a one-row DB at `path` via a bare-path writer, then open
    /// the SAME path through the helper URI and assert the row reads
    /// back. Round-trip success proves the encoded URI resolves to the
    /// exact same file as the bare path.
    private func assertRoundTrips(path: String, file: StaticString = #filePath, line: UInt = #line) throws {
        // Bare-path writer at the exact (unencoded) path.
        do {
            let queue = try DatabaseQueue(path: path)
            try queue.write { db in
                try db.execute(sql: "CREATE TABLE t (v TEXT NOT NULL)")
                try db.execute(sql: "INSERT INTO t (v) VALUES ('sentinel')")
            }
        }

        // Reader via the production URI builder.
        var config = Configuration()
        config.readonly = true
        let uri = SQLiteSnapshotReader.readOnlyURI(for: path)
        let pool = try DatabasePool(path: uri, configuration: config)
        let value = try pool.read { db in
            try String.fetchOne(db, sql: "SELECT v FROM t LIMIT 1")
        }
        XCTAssertEqual(value, "sentinel",
                       "URI \(uri) must open the SAME file as bare path \(path)",
                       file: file, line: line)
    }

    func testSpacePathRoundTrips() throws {
        let dir = tempDir.appendingPathComponent("a b", isDirectory: true)
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        try assertRoundTrips(path: dir.appendingPathComponent("db.sqlite").path)
    }

    func testQuestionMarkPathRoundTrips() throws {
        let dir = tempDir.appendingPathComponent("a?b", isDirectory: true)
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        try assertRoundTrips(path: dir.appendingPathComponent("db.sqlite").path)
    }

    func testHashPathRoundTrips() throws {
        let dir = tempDir.appendingPathComponent("a#b", isDirectory: true)
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        try assertRoundTrips(path: dir.appendingPathComponent("db.sqlite").path)
    }

    func testPercentPathRoundTrips() throws {
        // The sharpest case: a raw `%` is an invalid URI escape; the
        // helper must encode it to `%25` to round-trip.
        let dir = tempDir.appendingPathComponent("a%b", isDirectory: true)
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        try assertRoundTrips(path: dir.appendingPathComponent("db.sqlite").path)
    }

    func testApplicationSupportStylePathRoundTrips() throws {
        // Mirrors the real production path shape (contains a space).
        let dir = tempDir
            .appendingPathComponent("Application Support", isDirectory: true)
            .appendingPathComponent("CallHistoryDB", isDirectory: true)
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        try assertRoundTrips(path: dir.appendingPathComponent("CallHistory.storedata").path)
    }

    func testPlainAsciiPathIsTextuallyUnchangedAfterPrefix() {
        // For an ASCII path free of URI-significant bytes, encoding is a
        // no-op: the URI is just `file://<path>?mode=ro`.
        let uri = SQLiteSnapshotReader.readOnlyURI(for: "/tmp/plain/db.sqlite")
        XCTAssertEqual(uri, "file:///tmp/plain/db.sqlite?mode=ro")
    }

    func testURLOverloadMatchesStringOverload() {
        let path = "/tmp/Application Support/x.sqlite"
        XCTAssertEqual(
            SQLiteSnapshotReader.readOnlyURI(for: URL(fileURLWithPath: path)),
            SQLiteSnapshotReader.readOnlyURI(for: path))
    }
}
