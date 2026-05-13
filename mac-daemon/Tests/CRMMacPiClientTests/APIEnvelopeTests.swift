import XCTest
@testable import CRMMacPiClient

final class APIEnvelopeTests: XCTestCase {
    func testDecodeSuccessEnvelopeWithData() throws {
        let json = """
        {"success": true, "data": {"host_id":"11111111-2222-3333-4444-555555555555","api_key":"k","cursor_epoch": 7}}
        """
        let env = try JSONDecoder().decode(APIEnvelope<PairData>.self, from: Data(json.utf8))
        XCTAssertTrue(env.success)
        XCTAssertNil(env.error)
        XCTAssertEqual(env.data?.apiKey, "k")
        XCTAssertEqual(env.data?.cursorEpoch, 7)
    }

    func testDecodeFailureEnvelopeWithError() throws {
        let json = """
        {"success": false, "error": {"code": "X", "message": "y"}}
        """
        let env = try JSONDecoder().decode(APIEnvelope<PairData>.self, from: Data(json.utf8))
        XCTAssertFalse(env.success)
        XCTAssertNil(env.data)
        XCTAssertEqual(env.error?.code, "X")
        XCTAssertEqual(env.error?.message, "y")
    }

    func testDecodeWithMeta() throws {
        let json = """
        {"success": true, "data": {"phones": [], "emails": []}, "meta": {"pagination": {"page": 1, "limit": 10}}}
        """
        let env = try JSONDecoder().decode(APIEnvelope<KnownIdentifiersData>.self, from: Data(json.utf8))
        XCTAssertNotNil(env.meta)
    }

    func testKnownIdentifiersFixtureDecodes() throws {
        let data = try loadFixture("known_identifiers_200")
        let env = try JSONDecoder().decode(APIEnvelope<KnownIdentifiersData>.self, from: data)
        XCTAssertEqual(env.data?.phones.count, 2)
        XCTAssertEqual(env.data?.emails.count, 2)
    }
}
