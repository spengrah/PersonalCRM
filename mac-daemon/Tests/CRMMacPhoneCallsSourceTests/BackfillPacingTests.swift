import XCTest
@testable import CRMMacPhoneCallsSource

final class BackfillPacingTests: XCTestCase {
    private let t0 = Date(timeIntervalSince1970: 1_700_000_000)

    func testInitialBudgetUnexhausted() {
        let b = PhoneCallsBudget(maxRows: 500, maxDuration: 5.0, now: t0)
        XCTAssertEqual(b.rowsRemaining, 500)
        XCTAssertFalse(b.wallclockExhausted(now: t0))
        XCTAssertFalse(b.exhausted(now: t0))
    }

    func testRowBudgetExhaustion() {
        var b = PhoneCallsBudget(maxRows: 500, maxDuration: 5.0, now: t0)
        b.consume(rows: 300)
        XCTAssertEqual(b.rowsRemaining, 200)
        b.consume(rows: 200)
        XCTAssertEqual(b.rowsRemaining, 0)
        XCTAssertTrue(b.exhausted(now: t0))
    }

    func testRowBudgetCannotGoNegative() {
        var b = PhoneCallsBudget(maxRows: 500, maxDuration: 5.0, now: t0)
        b.consume(rows: 700)
        XCTAssertEqual(b.rowsRemaining, 0)
    }

    func testWallclockBudgetExhaustion() {
        let b = PhoneCallsBudget(maxRows: 500, maxDuration: 5.0, now: t0)
        let justAfter = t0.addingTimeInterval(5.1)
        XCTAssertTrue(b.wallclockExhausted(now: justAfter))
        XCTAssertTrue(b.exhausted(now: justAfter))
    }
}
