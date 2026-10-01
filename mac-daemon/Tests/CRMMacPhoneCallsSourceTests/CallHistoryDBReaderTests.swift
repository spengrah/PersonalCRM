import XCTest
import GRDB
@testable import CRMMacPhoneCallsSource

final class CallHistoryDBReaderTests: XCTestCase {
    private struct InsertRow {
        let uniqueID: String
        let zdate: Double
        let address: String?
        let originated: Bool
        let answered: Int64?
        let duration: Double
        let serviceProvider: String?
        let callType: Int64?
        let hasMessage: Bool
    }

    private func seed(_ queue: DatabaseQueue, _ rows: [InsertRow]) throws {
        try queue.write { db in
            for row in rows {
                try db.execute(
                    sql: """
                        INSERT INTO ZCALLRECORD (
                            ZUNIQUE_ID, ZDATE, ZADDRESS,
                            ZORIGINATED, ZANSWERED, ZDURATION,
                            ZSERVICE_PROVIDER, ZCALLTYPE, ZHASMESSAGE)
                        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
                        """,
                    arguments: [
                        row.uniqueID,
                        row.zdate,
                        row.address,
                        row.originated ? 1 : 0,
                        row.answered,
                        row.duration,
                        row.serviceProvider,
                        row.callType,
                        row.hasMessage ? 1 : 0,
                    ])
            }
        }
    }

    /// 2025-01-01T00:00:00Z in Apple-epoch seconds.
    private let baseZDate: Double = 1_735_689_600 - 978_307_200

    func testForwardIterationWalksAllRows() throws {
        let queue = try InMemoryCallHistoryDB.makeQueue()
        try seed(queue, [
            InsertRow(uniqueID: "u1", zdate: baseZDate + 10, address: "+15551234567",
                      originated: false, answered: 1, duration: 30,
                      serviceProvider: "com.apple.Telephony", callType: 0,
                      hasMessage: false),
            InsertRow(uniqueID: "u2", zdate: baseZDate + 20, address: "+15559876543",
                      originated: true, answered: nil, duration: 60,
                      serviceProvider: "com.apple.Telephony", callType: 0,
                      hasMessage: false),
            InsertRow(uniqueID: "u3", zdate: baseZDate + 30, address: "alice@example.com",
                      originated: false, answered: 0, duration: 0,
                      serviceProvider: "com.apple.FaceTime", callType: 8,
                      hasMessage: true),
        ])

        let page = try queue.read { db in
            try CallHistoryDBReader.fetchPage(
                db: db,
                direction: .insertedAfter(zPK: 0),
                limit: 10)
        }
        XCTAssertEqual(page.rows.count, 3)
        XCTAssertEqual(page.rows[0].uniqueID, "u1")
        XCTAssertEqual(page.rows[1].uniqueID, "u2")
        XCTAssertEqual(page.rows[2].uniqueID, "u3")
        XCTAssertTrue(page.exhausted)
        XCTAssertEqual(page.serviceUnknownCount, 0)
    }

    func testBackwardIterationWalksReverseOrder() throws {
        let queue = try InMemoryCallHistoryDB.makeQueue()
        try seed(queue, [
            InsertRow(uniqueID: "u1", zdate: baseZDate + 10, address: "+15551234567",
                      originated: false, answered: 1, duration: 30,
                      serviceProvider: "com.apple.Telephony", callType: 0,
                      hasMessage: false),
            InsertRow(uniqueID: "u2", zdate: baseZDate + 20, address: "+15559876543",
                      originated: true, answered: nil, duration: 60,
                      serviceProvider: "com.apple.Telephony", callType: 0,
                      hasMessage: false),
        ])

        let page = try queue.read { db in
            try CallHistoryDBReader.fetchPage(
                db: db,
                direction: .backwardFromExclusive(zdate: baseZDate + 100, zPK: 999),
                limit: 10)
        }
        XCTAssertEqual(page.rows.count, 2)
        XCTAssertEqual(page.rows[0].uniqueID, "u2")
        XCTAssertEqual(page.rows[1].uniqueID, "u1")
    }

    /// Backfill advances to `lastScanned`, which on a descending page
    /// must be the page's lowest (ZDATE, Z_PK) point even when Z_PK
    /// order disagrees with ZDATE order.
    func testBackwardPageLastScannedIsLowestPoint() throws {
        let queue = try InMemoryCallHistoryDB.makeQueue()
        let offsets: [Double] = [40, 10, 30, 0, 20] // Z_PK 1...5
        try seed(queue, offsets.enumerated().map { i, offset in
            InsertRow(uniqueID: "u\(i + 1)", zdate: baseZDate + offset,
                      address: "+15551234567",
                      originated: false, answered: 1, duration: 30,
                      serviceProvider: "com.apple.Telephony", callType: 0,
                      hasMessage: false)
        })
        let page = try queue.read { db in
            try CallHistoryDBReader.fetchPage(
                db: db,
                direction: .backwardFromExclusive(zdate: baseZDate + 100, zPK: 0),
                limit: 3)
        }
        XCTAssertEqual(page.rows.map(\.uniqueID), ["u1", "u3", "u5"])
        XCTAssertEqual(page.lastScanned, CallCursorPoint(zdate: baseZDate + 20, zPK: 5))
    }

    func testMaxZDateReturnsTopRow() throws {
        let queue = try InMemoryCallHistoryDB.makeQueue()
        try seed(queue, [
            InsertRow(uniqueID: "u1", zdate: baseZDate + 10, address: "+15551234567",
                      originated: false, answered: 1, duration: 30,
                      serviceProvider: "com.apple.Telephony", callType: 0,
                      hasMessage: false),
            InsertRow(uniqueID: "u2", zdate: baseZDate + 200, address: "+15559876543",
                      originated: true, answered: nil, duration: 60,
                      serviceProvider: "com.apple.Telephony", callType: 0,
                      hasMessage: false),
        ])
        let p = try queue.read { try CallHistoryDBReader.maxZDate(db: $0) }
        XCTAssertEqual(p?.zdate, baseZDate + 200)
        XCTAssertEqual(p?.zPK, 2)
    }

    func testMaxZDateOnEmptyTableReturnsNil() throws {
        let queue = try InMemoryCallHistoryDB.makeQueue()
        let p = try queue.read { try CallHistoryDBReader.maxZDate(db: $0) }
        XCTAssertNil(p)
    }

    func testEmptyAddressRowsAreSkipped() throws {
        let queue = try InMemoryCallHistoryDB.makeQueue()
        try seed(queue, [
            InsertRow(uniqueID: "u-ok", zdate: baseZDate + 10, address: "+15551234567",
                      originated: false, answered: 1, duration: 30,
                      serviceProvider: "com.apple.Telephony", callType: 0,
                      hasMessage: false),
            InsertRow(uniqueID: "u-empty", zdate: baseZDate + 20, address: "",
                      originated: false, answered: 1, duration: 30,
                      serviceProvider: "com.apple.Telephony", callType: 0,
                      hasMessage: false),
            InsertRow(uniqueID: "u-nil", zdate: baseZDate + 30, address: nil,
                      originated: false, answered: 1, duration: 30,
                      serviceProvider: "com.apple.Telephony", callType: 0,
                      hasMessage: false),
        ])
        let page = try queue.read { db in
            try CallHistoryDBReader.fetchPage(
                db: db,
                direction: .insertedAfter(zPK: 0),
                limit: 10)
        }
        XCTAssertEqual(page.rows.count, 1)
        XCTAssertEqual(page.rows[0].uniqueID, "u-ok")
        // The page ends on skipped rows; lastScanned still reaches them
        // so the caller can advance past them.
        XCTAssertEqual(page.lastScanned?.zPK, 3)
    }

    func testCorruptDateRowsAreSkipped() throws {
        let queue = try InMemoryCallHistoryDB.makeQueue()
        try seed(queue, [
            // zdate <= 0 -> corrupt
            InsertRow(uniqueID: "u-zero", zdate: 0, address: "+15551234567",
                      originated: false, answered: 1, duration: 30,
                      serviceProvider: "com.apple.Telephony", callType: 0,
                      hasMessage: false),
            // zdate below the ~2016 sentinel -> corrupt
            InsertRow(uniqueID: "u-ancient", zdate: 1_000_000, address: "+15551234567",
                      originated: false, answered: 1, duration: 30,
                      serviceProvider: "com.apple.Telephony", callType: 0,
                      hasMessage: false),
            InsertRow(uniqueID: "u-ok", zdate: baseZDate + 30, address: "+15551234567",
                      originated: false, answered: 1, duration: 30,
                      serviceProvider: "com.apple.Telephony", callType: 0,
                      hasMessage: false),
        ])
        let page = try queue.read { db in
            try CallHistoryDBReader.fetchPage(
                db: db,
                direction: .insertedAfter(zPK: 0),
                limit: 10)
        }
        XCTAssertEqual(page.rows.count, 1)
        XCTAssertEqual(page.rows[0].uniqueID, "u-ok")
    }

    /// A NULL-ZDATE row has no cursor coordinate, so it can't advance
    /// the live cursor. A full page of them must not hide the rows
    /// inserted after.
    func testInsertedAfterExcludesNullZDateRows() throws {
        let queue = try InMemoryCallHistoryDB.makeQueue()
        try queue.write { db in
            for id in ["u-null-1", "u-null-2"] {
                try db.execute(
                    sql: "INSERT INTO ZCALLRECORD (ZUNIQUE_ID, ZDATE, ZADDRESS) VALUES (?, NULL, '+15551234567')",
                    arguments: [id])
            }
        }
        try seed(queue, [
            InsertRow(uniqueID: "u-ok", zdate: baseZDate + 10, address: "+15551234567",
                      originated: false, answered: 1, duration: 30,
                      serviceProvider: "com.apple.Telephony", callType: 0,
                      hasMessage: false),
        ])
        let page = try queue.read { db in
            try CallHistoryDBReader.fetchPage(
                db: db,
                direction: .insertedAfter(zPK: 0),
                limit: 2)
        }
        XCTAssertEqual(page.rows.map(\.uniqueID), ["u-ok"])
        XCTAssertEqual(page.lastScanned?.zPK, 3)
    }

    func testServiceUnknownRowsAreCountedAndSkipped() throws {
        let queue = try InMemoryCallHistoryDB.makeQueue()
        try seed(queue, [
            InsertRow(uniqueID: "u-unknown", zdate: baseZDate + 10, address: "+15551234567",
                      originated: false, answered: 1, duration: 30,
                      serviceProvider: "com.apple.UnknownService", callType: 8,
                      hasMessage: false),
            InsertRow(uniqueID: "u-ok", zdate: baseZDate + 20, address: "+15551234567",
                      originated: false, answered: 1, duration: 30,
                      serviceProvider: "com.apple.Telephony", callType: 0,
                      hasMessage: false),
        ])
        let page = try queue.read { db in
            try CallHistoryDBReader.fetchPage(
                db: db,
                direction: .insertedAfter(zPK: 0),
                limit: 10)
        }
        XCTAssertEqual(page.rows.count, 1)
        XCTAssertEqual(page.rows[0].uniqueID, "u-ok")
        XCTAssertEqual(page.serviceUnknownCount, 1)
    }

    func testOutboundForcesAnsweredNilAndHasVoicemailFalse() throws {
        let queue = try InMemoryCallHistoryDB.makeQueue()
        // Outbound row with weird source data: ZANSWERED=1 and
        // ZHASMESSAGE=1 (defensively-bogus values from macOS).
        try seed(queue, [
            InsertRow(uniqueID: "u-out", zdate: baseZDate + 10, address: "+15551234567",
                      originated: true, answered: 1, duration: 60,
                      serviceProvider: "com.apple.Telephony", callType: 0,
                      hasMessage: true),
        ])
        let page = try queue.read { db in
            try CallHistoryDBReader.fetchPage(
                db: db,
                direction: .insertedAfter(zPK: 0),
                limit: 10)
        }
        XCTAssertEqual(page.rows.count, 1)
        XCTAssertNil(page.rows[0].answered)
        XCTAssertFalse(page.rows[0].hasMessage)
        XCTAssertTrue(page.rows[0].originated)
    }

    func testInboundPreservesAnsweredAndHasVoicemail() throws {
        let queue = try InMemoryCallHistoryDB.makeQueue()
        try seed(queue, [
            InsertRow(uniqueID: "u-answered", zdate: baseZDate + 10, address: "+15551234567",
                      originated: false, answered: 1, duration: 30,
                      serviceProvider: "com.apple.Telephony", callType: 0,
                      hasMessage: false),
            InsertRow(uniqueID: "u-vm", zdate: baseZDate + 20, address: "+15551234567",
                      originated: false, answered: 0, duration: 25,
                      serviceProvider: "com.apple.Telephony", callType: 0,
                      hasMessage: true),
            InsertRow(uniqueID: "u-missed", zdate: baseZDate + 30, address: "+15551234567",
                      originated: false, answered: 0, duration: 0,
                      serviceProvider: "com.apple.Telephony", callType: 0,
                      hasMessage: false),
        ])
        let page = try queue.read { db in
            try CallHistoryDBReader.fetchPage(
                db: db,
                direction: .insertedAfter(zPK: 0),
                limit: 10)
        }
        XCTAssertEqual(page.rows.count, 3)
        XCTAssertEqual(page.rows[0].answered, true)
        XCTAssertEqual(page.rows[1].answered, false)
        XCTAssertEqual(page.rows[1].hasMessage, true)
        XCTAssertEqual(page.rows[2].answered, false)
        XCTAssertEqual(page.rows[2].hasMessage, false)
    }

    /// T-Swift-7: (ZDATE, Z_PK) tie-break test. Two rows at the same
    /// ZDATE must both be returned in Z_PK order; backfill descent past
    /// the second row of the pair must still return the first.
    func testTieBreakOnZDate() throws {
        let queue = try InMemoryCallHistoryDB.makeQueue()
        let tiedZDate = baseZDate + 10
        try seed(queue, [
            InsertRow(uniqueID: "u-a", zdate: tiedZDate, address: "+15551234567",
                      originated: false, answered: 1, duration: 30,
                      serviceProvider: "com.apple.Telephony", callType: 0,
                      hasMessage: false),
            InsertRow(uniqueID: "u-b", zdate: tiedZDate, address: "+15551234567",
                      originated: false, answered: 1, duration: 30,
                      serviceProvider: "com.apple.Telephony", callType: 0,
                      hasMessage: false),
        ])
        // Descent from a ceiling ABOVE the tied zdate: both rows
        // returned in Z_PK descending order.
        let firstPage = try queue.read { db in
            try CallHistoryDBReader.fetchPage(
                db: db,
                direction: .backwardFromExclusive(zdate: tiedZDate + 1, zPK: 0),
                limit: 10)
        }
        XCTAssertEqual(firstPage.rows.map(\.uniqueID), ["u-b", "u-a"])

        // Descent from (tiedZDate, 2): the ceiling is exclusive of
        // "u-b", so the row at the same zdate with Z_PK=1 must still
        // appear.
        let secondPage = try queue.read { db in
            try CallHistoryDBReader.fetchPage(
                db: db,
                direction: .backwardFromExclusive(zdate: tiedZDate, zPK: 2),
                limit: 10)
        }
        XCTAssertEqual(secondPage.rows.map(\.uniqueID), ["u-a"])

        // Descent from (tiedZDate, 1): no rows.
        let thirdPage = try queue.read { db in
            try CallHistoryDBReader.fetchPage(
                db: db,
                direction: .backwardFromExclusive(zdate: tiedZDate, zPK: 1),
                limit: 10)
        }
        XCTAssertEqual(thirdPage.rows.count, 0)
    }

    func testLimitIsRespected() throws {
        let queue = try InMemoryCallHistoryDB.makeQueue()
        var rows: [InsertRow] = []
        for i in 0..<10 {
            rows.append(InsertRow(
                uniqueID: "u\(i)", zdate: baseZDate + Double(i),
                address: "+15551234567",
                originated: false, answered: 1, duration: 30,
                serviceProvider: "com.apple.Telephony", callType: 0,
                hasMessage: false))
        }
        try seed(queue, rows)
        let page = try queue.read { db in
            try CallHistoryDBReader.fetchPage(
                db: db,
                direction: .insertedAfter(zPK: 0),
                limit: 3)
        }
        XCTAssertEqual(page.rows.count, 3)
        XCTAssertFalse(page.exhausted)
    }

    func testZDateConvertedToUTCDate() throws {
        let queue = try InMemoryCallHistoryDB.makeQueue()
        // 2025-06-15T12:00:00Z in Apple-epoch seconds.
        let target = TimeInterval(1_750_000_000) - 978_307_200
        try seed(queue, [
            InsertRow(uniqueID: "u1", zdate: target, address: "+15551234567",
                      originated: false, answered: 1, duration: 30,
                      serviceProvider: "com.apple.Telephony", callType: 0,
                      hasMessage: false),
        ])
        let page = try queue.read { db in
            try CallHistoryDBReader.fetchPage(
                db: db,
                direction: .insertedAfter(zPK: 0),
                limit: 10)
        }
        XCTAssertEqual(page.rows[0].startedAt.timeIntervalSince1970, 1_750_000_000, accuracy: 0.001)
    }
}
