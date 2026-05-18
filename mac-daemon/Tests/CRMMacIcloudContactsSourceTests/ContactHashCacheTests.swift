// ContactHashCacheTests cover the basic file-I/O surface:
// load-from-missing, load-from-corrupt, atomic rewrite. The two-phase
// commit semantics live in ContactHashCacheTwoPhaseTests below in a
// separate file.
import XCTest
@testable import CRMMacIcloudContactsSource

final class ContactHashCacheTests: XCTestCase {
    private var tmpDir: URL!
    private var fileURL: URL!

    override func setUp() {
        super.setUp()
        tmpDir = URL(fileURLWithPath: NSTemporaryDirectory())
            .appendingPathComponent(UUID().uuidString)
        try? FileManager.default.createDirectory(
            at: tmpDir, withIntermediateDirectories: true)
        fileURL = tmpDir.appendingPathComponent("hashes.json")
    }

    override func tearDown() {
        try? FileManager.default.removeItem(at: tmpDir)
        super.tearDown()
    }

    func testLoadAbsentFileIsEmpty() async throws {
        let cache = ContactHashCache(fileURL: fileURL)
        try await cache.load()
        let size = await cache.size()
        XCTAssertEqual(size, 0)
    }

    func testLoadCorruptFileThrows() async throws {
        try Data("not json".utf8).write(to: fileURL)
        let cache = ContactHashCache(fileURL: fileURL)
        do {
            try await cache.load()
            XCTFail("expected throw")
        } catch {
            // expected
        }
    }

    func testApplyUpdatesPersists() async throws {
        let cache = ContactHashCache(fileURL: fileURL)
        try await cache.load()
        try await cache.applyUpdates(["A": "h1", "B": "h2"])
        let reload = ContactHashCache(fileURL: fileURL)
        try await reload.load()
        let aHash = await reload.get("A")
        let bHash = await reload.get("B")
        XCTAssertEqual(aHash, "h1")
        XCTAssertEqual(bHash, "h2")
    }

    func testApplyUpdatesReplacesExisting() async throws {
        let cache = ContactHashCache(fileURL: fileURL)
        try await cache.load()
        try await cache.applyUpdates(["A": "h1"])
        try await cache.applyUpdates(["A": "h2"])
        let aHash = await cache.get("A")
        XCTAssertEqual(aHash, "h2")
    }

    func testApplyUpdatesIsIdempotent() async throws {
        let cache = ContactHashCache(fileURL: fileURL)
        try await cache.load()
        try await cache.applyUpdates(["A": "h1"])
        try await cache.applyUpdates(["A": "h1"])
        let size = await cache.size()
        XCTAssertEqual(size, 1)
    }

    func testEmptyApplyUpdatesIsNoOp() async throws {
        let cache = ContactHashCache(fileURL: fileURL)
        try await cache.load()
        try await cache.applyUpdates([:])
        XCTAssertFalse(FileManager.default.fileExists(atPath: fileURL.path),
                       "no updates should not create the file")
    }

    func testApplyUpdatesFirstWriteWithAbsentDestination() async throws {
        // Regression: on first install the cache file does not exist.
        // FileManager.replaceItemAt throws when the destination is
        // absent, so writeFile must fall back to moveItem in that case.
        // Without the fallback every first-tick write fails, the plugin
        // marks cache_write_failed, and no iCloud events are ever
        // synced.
        XCTAssertFalse(FileManager.default.fileExists(atPath: fileURL.path),
                       "precondition: destination file should not exist")
        let cache = ContactHashCache(fileURL: fileURL)
        try await cache.load()
        try await cache.applyUpdates(["A": "h1"])
        XCTAssertTrue(FileManager.default.fileExists(atPath: fileURL.path),
                      "cache file should exist after first applyUpdates")
        let reload = ContactHashCache(fileURL: fileURL)
        try await reload.load()
        let aHash = await reload.get("A")
        XCTAssertEqual(aHash, "h1")
    }

    func testSchemaVersionAboveSupportedThrows() async throws {
        let body = """
        {"schema_version": 999, "hashes": {}}
        """
        try Data(body.utf8).write(to: fileURL)
        let cache = ContactHashCache(fileURL: fileURL)
        do {
            try await cache.load()
            XCTFail("expected schema_version reject")
        } catch ContactHashCacheError.malformedFile {
            // expected
        } catch {
            XCTFail("unexpected error: \(error)")
        }
    }
}
