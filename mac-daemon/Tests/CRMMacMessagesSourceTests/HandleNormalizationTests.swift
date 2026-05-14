import XCTest
@testable import CRMMacMessagesSource

final class HandleNormalizationTests: XCTestCase {
    func testEmailHandle() {
        XCTAssertEqual(HandleNormalization.canonicalize("Foo@Example.com"), "foo@example.com")
        XCTAssertEqual(HandleNormalization.canonicalize("  user@host.com  "), "user@host.com")
    }

    func testPhoneHandle() {
        XCTAssertEqual(HandleNormalization.canonicalize("+1-555-123-4567"), "+15551234567")
        XCTAssertEqual(HandleNormalization.canonicalize("(555) 123-4567"), "+15551234567")
        XCTAssertEqual(HandleNormalization.canonicalize("+44 20 7946 0958"), "+442079460958")
    }

    func testEmpty() {
        XCTAssertEqual(HandleNormalization.canonicalize(""), "")
        XCTAssertEqual(HandleNormalization.canonicalize("   "), "")
    }

    func testWhitespaceOnly() {
        XCTAssertEqual(HandleNormalization.canonicalize("\n\t "), "")
    }
}
