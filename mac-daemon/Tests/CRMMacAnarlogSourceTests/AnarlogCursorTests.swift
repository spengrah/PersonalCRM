// Coverage for AnarlogHumansCursorCodec, AnarlogSessionsCursorCodec,
// and AnarlogSourceIDBuilder. The critical invariant is:
//
//   decodeOrNil("") == nil
//   decodeOrNil(malformed) == nil
//   decodeOrNil(valid) == [:] populated map
//
// Returning nil on empty/malformed routes the tick into the
// bootstrap-via-known-ids path per D4 — an empty `[:]` would silently
// mean "I have a cursor; it's just empty" and emit deletes for
// everything on the Pi.
import XCTest
@testable import CRMMacAnarlogSource

final class AnarlogCursorTests: XCTestCase {

    // MARK: - Humans

    func testHumansDecodeEmptyReturnsNil() {
        XCTAssertNil(AnarlogHumansCursorCodec.decodeOrNil(""))
    }

    func testHumansDecodeMalformedReturnsNil() {
        XCTAssertNil(AnarlogHumansCursorCodec.decodeOrNil("not-json"))
        XCTAssertNil(AnarlogHumansCursorCodec.decodeOrNil("[1,2,3]"))
        XCTAssertNil(AnarlogHumansCursorCodec.decodeOrNil("{\"a\": 1}"))
    }

    func testHumansRoundTrip() throws {
        let map: [String: AnarlogHumansCursorEntry] = [
            "uuid-1": AnarlogHumansCursorEntry(
                contentHash: "abc",
                payloadHash: "def",
                mtimeEpochMs: 1234567890),
            "uuid-2": AnarlogHumansCursorEntry(
                contentHash: "ghi",
                payloadHash: "jkl",
                mtimeEpochMs: nil),
        ]
        let encoded = try AnarlogHumansCursorCodec.encode(map)
        let decoded = try XCTUnwrap(AnarlogHumansCursorCodec.decodeOrNil(encoded))
        XCTAssertEqual(decoded, map)
    }

    func testHumansEncodeIsByteStable() throws {
        let map: [String: AnarlogHumansCursorEntry] = [
            "uuid-z": AnarlogHumansCursorEntry(contentHash: "a", payloadHash: "b"),
            "uuid-a": AnarlogHumansCursorEntry(contentHash: "c", payloadHash: "d"),
            "uuid-m": AnarlogHumansCursorEntry(contentHash: "e", payloadHash: "f"),
        ]
        // Two independent encodes must produce identical bytes.
        let a = try AnarlogHumansCursorCodec.encode(map)
        let b = try AnarlogHumansCursorCodec.encode(map)
        XCTAssertEqual(a, b)
        // Sorted keys: 'uuid-a' must precede 'uuid-m' which must
        // precede 'uuid-z' in the output string.
        let idxA = a.range(of: "uuid-a")!.lowerBound
        let idxM = a.range(of: "uuid-m")!.lowerBound
        let idxZ = a.range(of: "uuid-z")!.lowerBound
        XCTAssertLessThan(idxA, idxM)
        XCTAssertLessThan(idxM, idxZ)
    }

    func testHumansEmptyMapEncodes() throws {
        let s = try AnarlogHumansCursorCodec.encode([:])
        XCTAssertEqual(s, "{}")
        // And `{}` decodes to an empty (NOT nil!) map — the cursor
        // exists but is empty, which is what the post-commit state
        // looks like.
        XCTAssertEqual(AnarlogHumansCursorCodec.decodeOrNil("{}"), [:])
    }

    // MARK: - Sessions

    func testSessionsDecodeEmptyReturnsNil() {
        XCTAssertNil(AnarlogSessionsCursorCodec.decodeOrNil(""))
    }

    func testSessionsDecodeMalformedReturnsNil() {
        XCTAssertNil(AnarlogSessionsCursorCodec.decodeOrNil("not-json"))
        XCTAssertNil(AnarlogSessionsCursorCodec.decodeOrNil("{\"a\": {}}"))
    }

    func testSessionsRoundTripWithAllFields() throws {
        let map: [String: AnarlogSessionsCursorEntry] = [
            "uuid-1": AnarlogSessionsCursorEntry(
                metaHash: "abc",
                summaryHash: "def",
                memoHash: "ghi",
                payloadHash: "jkl"),
            "uuid-2": AnarlogSessionsCursorEntry(
                metaHash: "mno",
                summaryHash: nil,
                memoHash: nil,
                payloadHash: "pqr"),
        ]
        let encoded = try AnarlogSessionsCursorCodec.encode(map)
        let decoded = try XCTUnwrap(AnarlogSessionsCursorCodec.decodeOrNil(encoded))
        XCTAssertEqual(decoded, map)
    }

    func testSessionsFloorSkipSentinelRoundTrip() throws {
        let entry = AnarlogSessionsCursorCodec.floorSkippedEntry()
        XCTAssertEqual(entry.metaHash, "floor_skip")
        XCTAssertNil(entry.summaryHash)
        XCTAssertNil(entry.memoHash)
        XCTAssertEqual(entry.payloadHash, "")
        XCTAssertTrue(entry.isFloorSkipped)

        let map = ["uuid-pre-floor": entry]
        let encoded = try AnarlogSessionsCursorCodec.encode(map)
        let decoded = try XCTUnwrap(AnarlogSessionsCursorCodec.decodeOrNil(encoded))
        XCTAssertEqual(decoded, map)
        XCTAssertTrue(decoded["uuid-pre-floor"]!.isFloorSkipped)
    }

    // MARK: - Source ID Builder

    func testUpsertSourceIDFormat() {
        XCTAssertEqual(
            AnarlogSourceIDBuilder.upsertSourceID(entityID: "uuid", payloadHash: "hash"),
            "uuid@hash")
    }

    func testDeleteSourceIDWithKnownPriorHash() {
        XCTAssertEqual(
            AnarlogSourceIDBuilder.deleteSourceID(entityID: "uuid", priorPayloadHash: "h"),
            "uuid@deleted@h")
    }

    func testDeleteSourceIDWithNilFallsBackToUnknown() {
        XCTAssertEqual(
            AnarlogSourceIDBuilder.deleteSourceID(entityID: "uuid", priorPayloadHash: nil),
            "uuid@deleted@unknown")
    }

    func testDeleteSourceIDWithEmptyStringFallsBackToUnknown() {
        XCTAssertEqual(
            AnarlogSourceIDBuilder.deleteSourceID(entityID: "uuid", priorPayloadHash: ""),
            "uuid@deleted@unknown")
    }
}
