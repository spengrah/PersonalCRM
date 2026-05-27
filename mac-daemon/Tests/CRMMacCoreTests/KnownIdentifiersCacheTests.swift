import XCTest
@testable import CRMMacCore

final class KnownIdentifiersCacheTests: XCTestCase {
    func testEmptyCacheStartsUnpopulated() async {
        let cache = KnownIdentifiersCache()
        let populated = await cache.isPopulated
        XCTAssertFalse(populated)
        let contains = await cache.contains("+15551234567")
        XCTAssertFalse(contains)
    }

    func testReplaceFromEmpty() async {
        let cache = KnownIdentifiersCache()
        let added = await cache.replace(with: ["+15551234567", "foo@example.com"])
        XCTAssertEqual(added, ["+15551234567", "foo@example.com"])
        let populated = await cache.isPopulated
        XCTAssertTrue(populated)
        let hasPhone = await cache.contains("+15551234567")
        XCTAssertTrue(hasPhone)
    }

    func testDiffEqualSetsIsEmpty() async {
        let cache = KnownIdentifiersCache(initial: ["a@example.com", "b@example.com"])
        let added = await cache.replace(with: ["a@example.com", "b@example.com"])
        XCTAssertTrue(added.isEmpty)
    }

    func testDiffAdditionsOnly() async {
        let cache = KnownIdentifiersCache(initial: ["a@example.com"])
        let added = await cache.replace(with: ["a@example.com", "b@example.com"])
        XCTAssertEqual(added, ["b@example.com"])
    }

    func testRemovalsAreNotInDiff() async {
        // Pi deleted a contact; cache shrinks. Diff is one-way (only
        // additions); the removed identifier is dropped silently.
        let cache = KnownIdentifiersCache(initial: ["a@example.com", "b@example.com"])
        let added = await cache.replace(with: ["a@example.com"])
        XCTAssertTrue(added.isEmpty)
        let snapshot = await cache.snapshot()
        XCTAssertEqual(snapshot, ["a@example.com"])
    }

    func testKnownIdentifiersHashDeterministic() {
        let set1: Set<String> = ["+15551234567", "foo@example.com", "bar@example.com"]
        let set2: Set<String> = ["bar@example.com", "+15551234567", "foo@example.com"]
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
