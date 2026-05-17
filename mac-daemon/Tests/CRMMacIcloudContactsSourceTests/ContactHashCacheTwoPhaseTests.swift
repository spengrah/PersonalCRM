// ContactHashCacheTwoPhaseTests anchor the staging / commit /
// discard semantics that protect the daemon from losing prior hashes
// during partial-tick failures.
import XCTest
@testable import CRMMacIcloudContactsSource

final class ContactHashCacheTwoPhaseTests: XCTestCase {
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

    func testStagingDoesNotMutateLiveMapOrFile() async throws {
        let cache = ContactHashCache(fileURL: fileURL)
        try await cache.load()
        try await cache.applyUpdates(["A": "h1", "B": "h2"])
        await cache.stagePendingRemovals(["A"])
        // Live map: prior hash still visible.
        let aHash = await cache.get("A")
        XCTAssertEqual(aHash, "h1")
        // File: still contains both entries.
        let reload = ContactHashCache(fileURL: fileURL)
        try await reload.load()
        let aPersisted = await reload.get("A")
        XCTAssertEqual(aPersisted, "h1")
        let pendingCount = await cache.pendingRemovalCount()
        XCTAssertEqual(pendingCount, 1)
    }

    func testCommitFinalizesRemovals() async throws {
        let cache = ContactHashCache(fileURL: fileURL)
        try await cache.load()
        try await cache.applyUpdates(["A": "h1", "B": "h2"])
        await cache.stagePendingRemovals(["A"])
        try await cache.commitPendingRemovals()
        let aHash = await cache.get("A")
        XCTAssertNil(aHash)
        let pendingCount = await cache.pendingRemovalCount()
        XCTAssertEqual(pendingCount, 0)
        // File reflects the commit.
        let reload = ContactHashCache(fileURL: fileURL)
        try await reload.load()
        let aPersisted = await reload.get("A")
        let bPersisted = await reload.get("B")
        XCTAssertNil(aPersisted)
        XCTAssertEqual(bPersisted, "h2")
    }

    func testDiscardLeavesLiveMapIntact() async throws {
        let cache = ContactHashCache(fileURL: fileURL)
        try await cache.load()
        try await cache.applyUpdates(["A": "h1", "B": "h2"])
        await cache.stagePendingRemovals(["A", "B"])
        await cache.discardPendingRemovals()
        let aHash = await cache.get("A")
        let bHash = await cache.get("B")
        XCTAssertEqual(aHash, "h1")
        XCTAssertEqual(bHash, "h2")
        let pendingCount = await cache.pendingRemovalCount()
        XCTAssertEqual(pendingCount, 0)
        // File too.
        let reload = ContactHashCache(fileURL: fileURL)
        try await reload.load()
        let aPersisted = await reload.get("A")
        let bPersisted = await reload.get("B")
        XCTAssertEqual(aPersisted, "h1")
        XCTAssertEqual(bPersisted, "h2")
    }

    func testCrashBetweenStageAndCommitLosesPendingButPreservesLive() async throws {
        // Simulate a process crash by abandoning the cache instance
        // after stagePendingRemovals + before commitPendingRemovals.
        // A fresh instance reloads from disk; pendingRemovals is
        // in-memory only.
        do {
            let cache = ContactHashCache(fileURL: fileURL)
            try await cache.load()
            try await cache.applyUpdates(["A": "h1"])
            await cache.stagePendingRemovals(["A"])
        }
        let cache2 = ContactHashCache(fileURL: fileURL)
        try await cache2.load()
        let aHash = await cache2.get("A")
        XCTAssertEqual(aHash, "h1", "live map survives crash mid-stage")
        let pendingCount = await cache2.pendingRemovalCount()
        XCTAssertEqual(pendingCount, 0, "pendingRemovals is in-memory only")
    }

    func testSameTickDeleteThenAddCancelsRemoval() async throws {
        // Per Codex r3 P2-4: a delete event followed by a re-add in
        // the same tick must finalize as "upserted", not "deleted".
        let cache = ContactHashCache(fileURL: fileURL)
        try await cache.load()
        try await cache.applyUpdates(["A": "h1"])
        await cache.stagePendingRemovals(["A"])
        try await cache.applyUpdates(["A": "h2"]) // re-add in same tick
        let pendingCount = await cache.pendingRemovalCount()
        XCTAssertEqual(pendingCount, 0,
                       "applyUpdates must cancel any staged removal for the same identifier")
        let aHash = await cache.get("A")
        XCTAssertEqual(aHash, "h2", "new hash wins")
        // Commit pending should be a no-op.
        try await cache.commitPendingRemovals()
        let finalHash = await cache.get("A")
        XCTAssertEqual(finalHash, "h2",
                       "commitPendingRemovals must NOT remove the re-added identifier")
    }

    func testEmptyCommitIsNoOp() async throws {
        let cache = ContactHashCache(fileURL: fileURL)
        try await cache.load()
        try await cache.commitPendingRemovals()
        let size = await cache.size()
        XCTAssertEqual(size, 0)
    }
}
