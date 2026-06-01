// PhoneCallsCursorWirePendingScansTests — backward-compatible decode of
// the additive pendingScans queue + per-entry progress fields.
//
// Synthetic handles only (+15550000001 etc.); no real PII.
import XCTest
import CRMMacCore
@testable import CRMMacPhoneCallsSource

final class PhoneCallsCursorWirePendingScansTests: XCTestCase {
    func testOldJSONWithoutPendingScansDecodesEmpty() throws {
        // A cursor JSON written before pending_scans existed must decode
        // with an empty queue, not throw.
        let json = """
            {
                "backfill_floor_sent_at": "2026-01-01T00:00:00Z",
                "backfill_complete": false
            }
            """
        let decoded = try PhoneCallsCursorCodec.decode(json)
        XCTAssertNotNil(decoded)
        XCTAssertEqual(decoded?.pendingScans, [])
    }

    func testOldEntryWithoutProgressDecodesNilProgress() throws {
        // An operator-CLI-queued entry omits the progress fields → nil.
        let json = """
            {
                "backfill_floor_sent_at": "2026-01-01T00:00:00Z",
                "backfill_complete": false,
                "pending_scans": [
                    {
                        "normalized_handle": "+15550000001",
                        "since": "2026-04-01T00:00:00Z"
                    }
                ]
            }
            """
        let decoded = try PhoneCallsCursorCodec.decode(json)
        let scan = try XCTUnwrap(decoded?.pendingScans.first)
        XCTAssertEqual(scan.normalizedHandle, "+15550000001")
        XCTAssertNil(scan.progressBelowZDate)
        XCTAssertNil(scan.progressBelowZPK)
    }

    func testRoundTripWithProgress() throws {
        let original = PhoneCallsCursor(
            backfillFloorSentAt: PhoneCallsCursor.defaultBackfillFloor,
            pendingScans: [
                PhoneCallsCursorPendingScan(
                    normalizedHandle: "+15550000001",
                    since: Date(timeIntervalSince1970: 1_775_000_000),
                    progressBelowZDate: 771_692_400.654321,
                    progressBelowZPK: 100),
                PhoneCallsCursorPendingScan(
                    normalizedHandle: "test@example.com",
                    since: Date(timeIntervalSince1970: 1_775_000_000)),
            ])
        let json = try PhoneCallsCursorCodec.encode(original)
        let decoded = try PhoneCallsCursorCodec.decode(json)
        XCTAssertEqual(decoded, original)
        // Sub-second ZDATE progress survives the Double round-trip.
        XCTAssertEqual(decoded?.pendingScans.first?.progressBelowZDate, 771_692_400.654321)
        // Second entry's progress stays nil.
        XCTAssertNil(decoded?.pendingScans.last?.progressBelowZPK)
    }

    func testPendingScansUsesSnakeCaseKeys() throws {
        let cursor = PhoneCallsCursor(
            backfillFloorSentAt: PhoneCallsCursor.defaultBackfillFloor,
            pendingScans: [
                PhoneCallsCursorPendingScan(
                    normalizedHandle: "+15550000001",
                    since: Date(timeIntervalSince1970: 1_775_000_000),
                    progressBelowZDate: 1.0,
                    progressBelowZPK: 2),
            ])
        let json = try PhoneCallsCursorCodec.encode(cursor)
        XCTAssertTrue(json.contains("\"pending_scans\""))
        XCTAssertTrue(json.contains("\"normalized_handle\""))
        XCTAssertTrue(json.contains("\"progress_below_zdate\""))
        XCTAssertTrue(json.contains("\"progress_below_z_pk\""))
    }

    func testCapEnforcedOnEncode() throws {
        // The cap is enforced by the queue wrapper / ops paths, not the
        // wire struct itself; this test documents the constant matches
        // the messages cap so both sources behave identically.
        XCTAssertEqual(PhoneCallsCursor.pendingScansCap, 256)
        XCTAssertEqual(MessagesCursorWire.pendingScansCap, PhoneCallsCursor.pendingScansCap)
    }
}
