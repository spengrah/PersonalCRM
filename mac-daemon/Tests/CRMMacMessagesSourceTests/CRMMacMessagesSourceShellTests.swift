import XCTest
@testable import CRMMacMessagesSource

/// Sanity test asserting the new target builds and the GRDB dep resolves.
/// Real coverage lands in subsequent PR7 commits.
final class CRMMacMessagesSourceShellTests: XCTestCase {
    func testPayloadVersion() {
        XCTAssertEqual(CRMMacMessagesSource.payloadVersion, 1)
    }
}
