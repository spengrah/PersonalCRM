// CallHistoryScanReaderTests — the identifier-scoped, date-bounded,
// resumable ZADDRESS scan reader over CallHistoryDB.
//
// Synthetic handles only (+15550000001, test@example.com); no real PII.
import XCTest
import GRDB
import CRMMacCore
@testable import CRMMacPhoneCallsSource

final class CallHistoryScanReaderTests: XCTestCase {
    // 2026-04-01 UTC — inside the 30-day window for the 2026-05 `since`
    // tests, and after the backfill floor.
    private let unixApr2026: TimeInterval = 1_775_001_600 // 2026-04-01
    private let unixMay2026: TimeInterval = 1_777_680_000 // 2026-05-02
    // 2026-01-01 floor in UNIX seconds.
    private let unixFloor2026: TimeInterval = 1_767_225_600

    /// Production-parity canonicalizer (phone -> E.164, email ->
    /// lowercased). Mirrors the closure the plugin injects.
    private let canonicalize: (String) -> String = { raw in
        let trimmed = raw.trimmingCharacters(in: .whitespacesAndNewlines)
        if trimmed.isEmpty { return "" }
        if trimmed.contains("@") {
            return NormalizationParity.normalizeEmail(trimmed)
        }
        return NormalizationParity.normalizePhoneE164(trimmed)
    }

    private func insertCall(
        db: Database,
        zPK: Int64,
        uniqueID: String,
        address: String?,
        unixDate: TimeInterval,
        originated: Bool = false,
        serviceProvider: String? = "com.apple.Telephony",
        callType: Int64? = 0
    ) throws {
        let zdate = InMemoryCallHistoryDB.appleEpochSeconds(unix: unixDate)
        try db.execute(
            sql: """
                INSERT INTO ZCALLRECORD (
                    Z_PK, ZUNIQUE_ID, ZDATE, ZADDRESS,
                    ZORIGINATED, ZANSWERED, ZDURATION,
                    ZSERVICE_PROVIDER, ZCALLTYPE, ZHASMESSAGE)
                VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
                """,
            arguments: [
                zPK, uniqueID, zdate, address,
                originated ? 1 : 0, originated ? nil : 1, 30,
                serviceProvider, callType, 0,
            ])
    }

    private func scan(
        _ queue: DatabaseQueue,
        handle: String,
        sinceUnix: TimeInterval,
        progressBelowZDate: Double? = nil,
        progressBelowZPK: Int64? = nil,
        limit: Int = 100
    ) throws -> CallHistoryScanPage {
        try queue.read { db in
            try CallHistoryScanReader.scanPage(
                db: db,
                canonicalHandle: handle,
                canonicalizer: canonicalize,
                since: Date(timeIntervalSince1970: sinceUnix),
                progressBelowZDate: progressBelowZDate,
                progressBelowZPK: progressBelowZPK,
                limit: limit)
        }
    }

    // MARK: - address resolution (alternate spellings)

    func testResolvesMultipleRawAddressesForOneCanonicalHandle() throws {
        let queue = try InMemoryCallHistoryDB.makeQueue()
        try queue.write { db in
            // Two raw spellings of the SAME canonical number + an
            // unrelated address.
            try insertCall(db: db, zPK: 1, uniqueID: "u1", address: "+15550000001",
                           unixDate: unixApr2026)
            try insertCall(db: db, zPK: 2, uniqueID: "u2", address: "(555) 000-0001",
                           unixDate: unixApr2026)
            try insertCall(db: db, zPK: 3, uniqueID: "u3", address: "+15559999999",
                           unixDate: unixApr2026)
        }
        let resolved = try queue.read { db in
            try CallHistoryScanReader.resolveAddresses(
                db: db, canonicalHandle: "+15550000001", canonicalizer: canonicalize)
        }
        XCTAssertEqual(Set(resolved), ["+15550000001", "(555) 000-0001"])
    }

    func testScanFindsRowsUnderAllSpellings() throws {
        let queue = try InMemoryCallHistoryDB.makeQueue()
        try queue.write { db in
            try insertCall(db: db, zPK: 1, uniqueID: "u1", address: "+15550000001",
                           unixDate: unixApr2026)
            try insertCall(db: db, zPK: 2, uniqueID: "u2", address: "(555) 000-0001",
                           unixDate: unixApr2026 + 10)
        }
        let page = try scan(queue, handle: "+15550000001", sinceUnix: unixFloor2026)
        XCTAssertEqual(Set(page.rows.map(\.uniqueID)), ["u1", "u2"])
        XCTAssertTrue(page.exhausted)
    }

    // MARK: - date window boundary

    func testDateWindowBoundary() throws {
        let queue = try InMemoryCallHistoryDB.makeQueue()
        let sinceUnix = unixMay2026
        try queue.write { db in
            // Exactly at `since` → included.
            try insertCall(db: db, zPK: 1, uniqueID: "at", address: "+15550000001",
                           unixDate: sinceUnix)
            // Just below `since` → excluded.
            try insertCall(db: db, zPK: 2, uniqueID: "below", address: "+15550000001",
                           unixDate: sinceUnix - 60)
            // Well above `since` → included.
            try insertCall(db: db, zPK: 3, uniqueID: "above", address: "+15550000001",
                           unixDate: sinceUnix + 86_400)
        }
        let page = try scan(queue, handle: "+15550000001", sinceUnix: sinceUnix)
        XCTAssertEqual(Set(page.rows.map(\.uniqueID)), ["at", "above"])
    }

    // MARK: - no match

    func testNoMatchHandleReturnsEmptyExhausted() throws {
        let queue = try InMemoryCallHistoryDB.makeQueue()
        try queue.write { db in
            try insertCall(db: db, zPK: 1, uniqueID: "u1", address: "+15550000001",
                           unixDate: unixApr2026)
        }
        let page = try scan(queue, handle: "+15558888888", sinceUnix: unixFloor2026)
        XCTAssertTrue(page.rows.isEmpty)
        XCTAssertTrue(page.exhausted)
        XCTAssertNil(page.lowestPoint)
    }

    // MARK: - email handle

    func testEmailHandleScan() throws {
        let queue = try InMemoryCallHistoryDB.makeQueue()
        try queue.write { db in
            // FaceTime call keyed on an email address (mixed case).
            try insertCall(db: db, zPK: 1, uniqueID: "e1", address: "Test@Example.com",
                           unixDate: unixApr2026,
                           serviceProvider: "com.apple.FaceTime", callType: 8)
        }
        let page = try scan(queue, handle: "test@example.com", sinceUnix: unixFloor2026)
        XCTAssertEqual(page.rows.map(\.uniqueID), ["e1"])
    }

    // MARK: - budget / resumability via (ZDATE, Z_PK)

    func testBudgetLimitAndResumeBelowProgress() throws {
        let queue = try InMemoryCallHistoryDB.makeQueue()
        // Five calls at strictly increasing ZDATE so the (ZDATE, Z_PK)
        // descending walk is deterministic.
        try queue.write { db in
            for i in 0..<5 {
                try insertCall(db: db, zPK: Int64(200 + i), uniqueID: "u\(i)",
                               address: "+15550000001",
                               unixDate: unixApr2026 + TimeInterval(i * 10))
            }
        }
        // First page: budget 2 → two highest (ZDATE, Z_PK), not
        // exhausted.
        let first = try scan(queue, handle: "+15550000001",
                             sinceUnix: unixFloor2026, limit: 2)
        XCTAssertEqual(first.rows.map(\.uniqueID), ["u4", "u3"])
        XCTAssertFalse(first.exhausted)
        XCTAssertEqual(first.lowestPoint?.zPK, 203)

        // Resume strictly below the lowest of the first page.
        let second = try scan(queue, handle: "+15550000001",
                              sinceUnix: unixFloor2026,
                              progressBelowZDate: first.lowestPoint?.zdate,
                              progressBelowZPK: first.lowestPoint?.zPK,
                              limit: 2)
        XCTAssertEqual(second.rows.map(\.uniqueID), ["u2", "u1"])
        XCTAssertFalse(second.exhausted)

        // Final page returns the last row and reports exhausted.
        let third = try scan(queue, handle: "+15550000001",
                             sinceUnix: unixFloor2026,
                             progressBelowZDate: second.lowestPoint?.zdate,
                             progressBelowZPK: second.lowestPoint?.zPK,
                             limit: 2)
        XCTAssertEqual(third.rows.map(\.uniqueID), ["u0"])
        XCTAssertTrue(third.exhausted)
    }

    // MARK: - (ZDATE, Z_PK) tie-break within the same second

    func testProgressTieBreaksOnZPKWithinSameZDate() throws {
        let queue = try InMemoryCallHistoryDB.makeQueue()
        // Three calls sharing one ZDATE; the Z_PK tie-break must page
        // through them without skipping or repeating.
        try queue.write { db in
            try insertCall(db: db, zPK: 10, uniqueID: "a", address: "+15550000001",
                           unixDate: unixApr2026)
            try insertCall(db: db, zPK: 11, uniqueID: "b", address: "+15550000001",
                           unixDate: unixApr2026)
            try insertCall(db: db, zPK: 12, uniqueID: "c", address: "+15550000001",
                           unixDate: unixApr2026)
        }
        let first = try scan(queue, handle: "+15550000001",
                             sinceUnix: unixFloor2026, limit: 2)
        XCTAssertEqual(first.rows.map(\.uniqueID), ["c", "b"])
        XCTAssertFalse(first.exhausted)
        XCTAssertEqual(first.lowestPoint?.zPK, 11)

        let second = try scan(queue, handle: "+15550000001",
                              sinceUnix: unixFloor2026,
                              progressBelowZDate: first.lowestPoint?.zdate,
                              progressBelowZPK: first.lowestPoint?.zPK,
                              limit: 2)
        XCTAssertEqual(second.rows.map(\.uniqueID), ["a"])
        XCTAssertTrue(second.exhausted)
    }

    // MARK: - skipped rows still advance lowestPoint

    func testServiceUnknownRowAdvancesLowestPointButIsNotReturned() throws {
        let queue = try InMemoryCallHistoryDB.makeQueue()
        try queue.write { db in
            // Higher ZDATE: unmappable service → skipped, but its point
            // must advance the resume coordinate so the page doesn't
            // stall on it.
            try insertCall(db: db, zPK: 30, uniqueID: "unknown", address: "+15550000001",
                           unixDate: unixApr2026 + 100,
                           serviceProvider: "com.unknown.thing", callType: 999)
            try insertCall(db: db, zPK: 31, uniqueID: "good", address: "+15550000001",
                           unixDate: unixApr2026)
        }
        let first = try scan(queue, handle: "+15550000001",
                             sinceUnix: unixFloor2026, limit: 1)
        // The first (highest) row is the unknown-service one: skipped,
        // not returned, but inspected — so lowestPoint advances to it.
        XCTAssertTrue(first.rows.isEmpty)
        XCTAssertFalse(first.exhausted)
        XCTAssertEqual(first.lowestPoint?.zPK, 30)

        // limit 2 so the single remaining row returns 1 < 2 → exhausted
        // (a limit-1 page returning exactly 1 row would NOT yet report
        // exhaustion — it takes a following empty page to confirm).
        let second = try scan(queue, handle: "+15550000001",
                              sinceUnix: unixFloor2026,
                              progressBelowZDate: first.lowestPoint?.zdate,
                              progressBelowZPK: first.lowestPoint?.zPK,
                              limit: 2)
        XCTAssertEqual(second.rows.map(\.uniqueID), ["good"])
        XCTAssertTrue(second.exhausted)
    }
}
