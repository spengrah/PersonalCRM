import XCTest
@testable import CRMMacMessagesSource

final class BackfillPacingTests: XCTestCase {
    private let t0 = Date(timeIntervalSince1970: 1_700_000_000)

    func testInitialBudgetUnexhausted() {
        let b = BackfillBudget(maxRows: 500, maxDuration: 5.0, now: t0)
        XCTAssertEqual(b.rowsRemaining, 500)
        XCTAssertFalse(b.wallclockExhausted(now: t0))
        XCTAssertFalse(b.exhausted(now: t0))
    }

    func testRowBudgetExhaustion() {
        var b = BackfillBudget(maxRows: 500, maxDuration: 5.0, now: t0)
        b.consume(rows: 300)
        XCTAssertEqual(b.rowsRemaining, 200)
        b.consume(rows: 200)
        XCTAssertEqual(b.rowsRemaining, 0)
        XCTAssertTrue(b.exhausted(now: t0))
    }

    func testRowBudgetCannotGoNegative() {
        var b = BackfillBudget(maxRows: 500, maxDuration: 5.0, now: t0)
        b.consume(rows: 700) // over-consumption
        XCTAssertEqual(b.rowsRemaining, 0)
    }

    func testWallclockBudgetExhaustion() {
        let b = BackfillBudget(maxRows: 500, maxDuration: 5.0, now: t0)
        let just_after = t0.addingTimeInterval(5.1)
        XCTAssertTrue(b.wallclockExhausted(now: just_after))
        XCTAssertTrue(b.exhausted(now: just_after))
    }

    func testWallclockNotExhaustedAtBoundary() {
        let b = BackfillBudget(maxRows: 500, maxDuration: 5.0, now: t0)
        XCTAssertFalse(b.wallclockExhausted(now: t0.addingTimeInterval(4.99)))
        XCTAssertTrue(b.wallclockExhausted(now: t0.addingTimeInterval(5.0)),
                      "wallclock exhausted at exactly the budget duration")
    }

    func testBothBudgetsCanExhaustSimultaneously() {
        var b = BackfillBudget(maxRows: 500, maxDuration: 5.0, now: t0)
        b.consume(rows: 500)
        let after = t0.addingTimeInterval(5.5)
        XCTAssertTrue(b.exhausted(now: after))
    }

    func testBudgetSplitBetweenBackfillAndLive() {
        // Backfill consumes 300, live gets 200 remaining.
        var b = BackfillBudget(maxRows: 500, maxDuration: 5.0, now: t0)
        b.consume(rows: 300)
        XCTAssertEqual(b.rowsRemaining, 200, "live gets the leftover row budget")

        b.consume(rows: 200)
        XCTAssertEqual(b.rowsRemaining, 0)
        XCTAssertTrue(b.exhausted(now: t0))
    }
}
