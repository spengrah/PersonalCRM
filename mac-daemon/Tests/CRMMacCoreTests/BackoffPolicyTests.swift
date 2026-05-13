import XCTest
@testable import CRMMacCore

final class BackoffPolicyTests: XCTestCase {
    func testDefaultPolicyExpCurve() {
        let p = BackoffPolicy()
        XCTAssertEqual(p.delay(forAttempt: 1), 1.0, accuracy: 0.001)
        XCTAssertEqual(p.delay(forAttempt: 2), 2.0, accuracy: 0.001)
        XCTAssertEqual(p.delay(forAttempt: 3), 4.0, accuracy: 0.001)
        XCTAssertEqual(p.delay(forAttempt: 4), 8.0, accuracy: 0.001)
        XCTAssertEqual(p.delay(forAttempt: 5), 16.0, accuracy: 0.001)
    }

    func testInvalidAttemptsReturnZero() {
        let p = BackoffPolicy()
        XCTAssertEqual(p.delay(forAttempt: 0), 0)
        XCTAssertEqual(p.delay(forAttempt: -1), 0)
        XCTAssertEqual(p.delay(forAttempt: p.maxRetries + 1), 0)
    }

    func testMaxRetriesIsFive() {
        XCTAssertEqual(BackoffPolicy().maxRetries, 5)
    }
}
