import XCTest
@testable import CRMMacCore

final class KnownIdentifiersCacheTests: XCTestCase {
    func testEmptyCacheStartsUnpopulated() async {
        let cache = KnownIdentifiersCache()
        let populated = await cache.isPopulated
        XCTAssertFalse(populated)
        let fetched = await cache.hasFetched
        XCTAssertFalse(fetched)
        let contains = await cache.contains("+15550000001")
        XCTAssertFalse(contains)
    }

    func testReplacePopulatesAndMarksFetched() async {
        let cache = KnownIdentifiersCache(consumers: [.messages])
        await cache.replace(with: ["+15550000001", "foo@example.com"])
        let populated = await cache.isPopulated
        XCTAssertTrue(populated)
        let fetched = await cache.hasFetched
        XCTAssertTrue(fetched)
        let hasPhone = await cache.contains("+15550000001")
        XCTAssertTrue(hasPhone)
    }

    func testFirstFetchSeedsBaselineWithoutDraining() async {
        // A consumer with no prior baseline (noBaseline) seeds on the
        // first fetch and enqueues nothing — no replay storm.
        let cache = KnownIdentifiersCache(consumers: [.messages])
        await cache.replace(with: ["a@example.com", "b@example.com"])
        let drained = await cache.drainNewlyAdded(for: .messages)
        XCTAssertTrue(drained.isEmpty)
        let base = await cache.baseline(for: .messages)
        XCTAssertEqual(base, ["a@example.com", "b@example.com"])
    }

    func testDiffAdditionsOnlyAfterSeed() async {
        let cache = KnownIdentifiersCache(
            baselines: [.messages: ["a@example.com"]],
            consumers: [.messages])
        await cache.replace(with: ["a@example.com", "b@example.com"])
        let drained = await cache.drainNewlyAdded(for: .messages)
        XCTAssertEqual(drained, ["b@example.com"])
    }

    func testRemovalsAreNotDrained() async {
        // Pi deleted a contact; cache shrinks. The diff is one-way
        // (additions only); the removed identifier is not drained, and
        // the canonical set + diff baseline both shrink.
        let cache = KnownIdentifiersCache(
            baselines: [.messages: ["a@example.com", "b@example.com"]],
            consumers: [.messages])
        await cache.replace(with: ["a@example.com"])
        let drained = await cache.drainNewlyAdded(for: .messages)
        XCTAssertTrue(drained.isEmpty)
        let snapshot = await cache.snapshot()
        XCTAssertEqual(snapshot, ["a@example.com"])
        let base = await cache.baseline(for: .messages)
        XCTAssertEqual(base, ["a@example.com"])
    }

    func testKnownIdentifiersHashDeterministic() {
        let set1: Set<String> = ["+15550000001", "foo@example.com", "bar@example.com"]
        let set2: Set<String> = ["bar@example.com", "+15550000001", "foo@example.com"]
        // Set is unordered but hash sorts before digesting -> same hash.
        XCTAssertEqual(
            KnownIdentifiersHash.sha256Hex(of: set1),
            KnownIdentifiersHash.sha256Hex(of: set2))
    }

    func testKnownIdentifiersHashLength() {
        let hash = KnownIdentifiersHash.sha256Hex(of: ["x", "y"])
        XCTAssertEqual(hash.count, 64)
        XCTAssertEqual(hash, hash.lowercased(), "hash must be lowercase hex")
    }

    func testKnownIdentifiersHashEmptySet() {
        // SHA-256 of empty input is a known constant.
        let hash = KnownIdentifiersHash.sha256Hex(of: [])
        XCTAssertEqual(hash, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855")
    }

    func testKnownIdentifiersHashChangesOnAddition() {
        let a = KnownIdentifiersHash.sha256Hex(of: ["a"])
        let ab = KnownIdentifiersHash.sha256Hex(of: ["a", "b"])
        XCTAssertNotEqual(a, ab)
    }
}
