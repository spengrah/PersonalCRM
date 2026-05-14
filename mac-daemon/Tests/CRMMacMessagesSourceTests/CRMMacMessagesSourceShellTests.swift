import XCTest
@testable import CRMMacMessagesSource

/// Sanity test asserting the new target builds and the GRDB dep resolves.
/// Per-feature coverage lives in the per-file test files
/// (UTIMappingTests, ChatDBReaderTests, etc.).
final class CRMMacMessagesSourceShellTests: XCTestCase {
    func testPayloadVersion() {
        XCTAssertEqual(CRMMacMessagesSource.payloadVersion, 1)
    }
}
