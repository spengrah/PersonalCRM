// MessagesCursorTests verifies the JSON encode/decode round-trip of
// the daemon-private cursor envelope that gets packed into the Pi-side
// opaque cursor string.
import XCTest
import CRMMacCore
@testable import CRMMacMessagesSource

final class MessagesCursorTests: XCTestCase {
    private let floor = MessagesCursorWire.defaultBackfillFloor

    func testRoundTripEmpty() throws {
        let original = MessagesCursor(backfillFloorSentAt: floor)
        let encoded = try MessagesCursorCodec.encode(original)
        let decoded = try MessagesCursorCodec.decode(encoded)
        XCTAssertEqual(decoded, original)
    }

    func testRoundTripFullPopulated() throws {
        let original = MessagesCursor(
            backfillCursor: 5000,
            liveCursor: 12_000,
            installMaxRowID: 10_000,
            backfillFloorSentAt: floor,
            backfillComplete: false,
            pendingScans: [
                PendingScan(normalizedHandle: "+15551234567",
                            since: Date(timeIntervalSince1970: 1_700_000_000)),
                PendingScan(normalizedHandle: "foo@example.com",
                            since: Date(timeIntervalSince1970: 1_710_000_000)),
            ],
            knownIdentifiersHash: "abc123")
        let encoded = try MessagesCursorCodec.encode(original)
        let decoded = try MessagesCursorCodec.decode(encoded)
        XCTAssertEqual(decoded, original)
    }

    func testDecodeEmptyStringReturnsNil() throws {
        let decoded = try MessagesCursorCodec.decode("")
        XCTAssertNil(decoded)
    }

    func testJSONSnakeCaseKeys() throws {
        let cursor = MessagesCursor(
            backfillCursor: 1,
            liveCursor: 2,
            installMaxRowID: 3,
            backfillFloorSentAt: floor,
            backfillComplete: true,
            knownIdentifiersHash: "deadbeef")
        let encoded = try MessagesCursorCodec.encode(cursor)
        // Spot-check snake_case keys appear in the encoded JSON.
        XCTAssertTrue(encoded.contains("\"backfill_cursor\""))
        XCTAssertTrue(encoded.contains("\"live_cursor\""))
        XCTAssertTrue(encoded.contains("\"install_max_rowid\""))
        XCTAssertTrue(encoded.contains("\"backfill_floor_sent_at\""))
        XCTAssertTrue(encoded.contains("\"backfill_complete\""))
        XCTAssertTrue(encoded.contains("\"pending_scans\""))
        XCTAssertTrue(encoded.contains("\"known_identifiers_hash\""))
    }

    func testPendingScanSnakeCaseKey() throws {
        let cursor = MessagesCursor(
            backfillFloorSentAt: floor,
            pendingScans: [
                PendingScan(normalizedHandle: "+15551234567",
                            since: Date(timeIntervalSince1970: 1_700_000_000)),
            ])
        let encoded = try MessagesCursorCodec.encode(cursor)
        XCTAssertTrue(encoded.contains("\"normalized_handle\""))
        XCTAssertTrue(encoded.contains("\"since\""))
    }

    func testPendingScansCapConstant() {
        XCTAssertEqual(MessagesCursor.pendingScansCap, 256)
    }

    func testPendingScanProgressRoundTrip() throws {
        let original = MessagesCursor(
            backfillFloorSentAt: floor,
            pendingScans: [
                PendingScan(normalizedHandle: "+15550000001",
                            since: Date(timeIntervalSince1970: 1_775_000_000),
                            progressBelowRowID: 4242),
            ])
        let encoded = try MessagesCursorCodec.encode(original)
        XCTAssertTrue(encoded.contains("\"progress_below_rowid\""))
        let decoded = try MessagesCursorCodec.decode(encoded)
        XCTAssertEqual(decoded, original)
        XCTAssertEqual(decoded?.pendingScans.first?.progressBelowRowID, 4242)
    }

    func testOutboundBackfillDoneRoundTrip() throws {
        let original = MessagesCursor(
            backfillFloorSentAt: floor,
            backfillComplete: true,
            outboundBackfillDone: true)
        let encoded = try MessagesCursorCodec.encode(original)
        XCTAssertTrue(encoded.contains("\"outbound_backfill_done\""))
        let decoded = try MessagesCursorCodec.decode(encoded)
        XCTAssertEqual(decoded, original)
        XCTAssertEqual(decoded?.outboundBackfillDone, true)
    }

    func testLegacyCursorWithoutOutboundFlagDecodesFalse() throws {
        // A cursor JSON from before the field existed: the key is absent
        // and must decode as false (additive Codable).
        let json = """
            {
                "backfill_floor_sent_at": "2026-01-01T00:00:00Z",
                "backfill_complete": true,
                "live_cursor": 1234,
                "install_max_rowid": 1234
            }
            """
        let decoded = try MessagesCursorCodec.decode(json)
        let cursor = try XCTUnwrap(decoded)
        XCTAssertFalse(cursor.outboundBackfillDone,
                       "legacy cursor missing the key decodes as false")
        XCTAssertEqual(cursor.liveCursor, 1234)
    }

    func testPendingScanWithoutProgressDecodesNil() throws {
        // An operator-CLI-queued or pre-existing entry omits the
        // progress field → nil ("not started").
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
        let decoded = try MessagesCursorCodec.decode(json)
        let scan = try XCTUnwrap(decoded?.pendingScans.first)
        XCTAssertNil(scan.progressBelowRowID)
    }
}
