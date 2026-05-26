import XCTest
import CRMMacCore
@testable import CRMMacPhoneCallsSource

final class PhoneCallsCursorWireTests: XCTestCase {
    func testEncodeDecodeRoundTrip() throws {
        let original = PhoneCallsCursor(
            backfillCursorZDate: Date(timeIntervalSince1970: 1_700_000_000),
            backfillCursorZPK: 42,
            liveCursorZDate: Date(timeIntervalSince1970: 1_750_000_000),
            liveCursorZPK: 100,
            installMaxZDate: Date(timeIntervalSince1970: 1_750_000_000),
            installMaxZPK: 100,
            backfillFloorSentAt: PhoneCallsCursor.defaultBackfillFloor,
            backfillComplete: false,
            knownIdentifiersHash: "abc123")
        let json = try PhoneCallsCursorCodec.encode(original)
        let decoded = try PhoneCallsCursorCodec.decode(json)
        XCTAssertEqual(decoded, original)
    }

    func testDecodeEmptyStringReturnsNil() throws {
        XCTAssertNil(try PhoneCallsCursorCodec.decode(""))
    }

    func testBackwardCompatMissingKnownIdentifiersHash() throws {
        // A cursor JSON written before known_identifiers_hash was added
        // must still decode. The field is optional and defaults to nil.
        let json = """
            {
                "backfill_floor_sent_at": "2026-01-01T00:00:00Z",
                "backfill_complete": false
            }
            """
        let decoded = try PhoneCallsCursorCodec.decode(json)
        XCTAssertNotNil(decoded)
        XCTAssertNil(decoded?.knownIdentifiersHash)
    }

    func testDefaultBackfillFloorMatches20260101() {
        // Verify the constant.
        let expected = Date(timeIntervalSince1970: 1_767_225_600)
        XCTAssertEqual(PhoneCallsCursor.defaultBackfillFloor, expected)
    }

    func testJSONUsesSnakeCaseKeys() throws {
        let cursor = PhoneCallsCursor(
            backfillFloorSentAt: PhoneCallsCursor.defaultBackfillFloor)
        let json = try PhoneCallsCursorCodec.encode(cursor)
        // sortedKeys output, so we can check field presence.
        XCTAssertTrue(json.contains("\"backfill_complete\":false"))
        XCTAssertTrue(json.contains("\"backfill_floor_sent_at\""))
    }
}
