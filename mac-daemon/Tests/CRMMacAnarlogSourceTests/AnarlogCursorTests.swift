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
            "uuid-1": AnarlogHumansCursorEntry(recordHash: "abc"),
            "uuid-2": AnarlogHumansCursorEntry(recordHash: "ghi"),
        ]
        let encoded = try AnarlogHumansCursorCodec.encode(map)
        let decoded = try XCTUnwrap(AnarlogHumansCursorCodec.decodeOrNil(encoded))
        XCTAssertEqual(decoded, map)
    }

    func testHumansEncodeIsByteStable() throws {
        let map: [String: AnarlogHumansCursorEntry] = [
            "uuid-z": AnarlogHumansCursorEntry(recordHash: "a"),
            "uuid-a": AnarlogHumansCursorEntry(recordHash: "c"),
            "uuid-m": AnarlogHumansCursorEntry(recordHash: "e"),
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

    func testHumansLegacyFileTreeEntryDecodesNil() {
        let cursor = #"{"0aaaaaaa-0000-4000-8000-00000000000b":{"content_hash":"a","payload_hash":"b","mtime_epoch_ms":1}}"#
        XCTAssertNil(AnarlogHumansCursorCodec.decodeOrNil(cursor))
    }

    func testHumansEntryEncodesOnlyRecordHash() throws {
        let encoded = try AnarlogHumansCursorCodec.encode([
            "u": AnarlogHumansCursorEntry(recordHash: "h"),
        ])
        XCTAssertEqual(encoded, #"{"u":{"record_hash":"h"}}"#)
    }

    // MARK: - Sessions

    func testSessionsDecodeEmptyReturnsNil() {
        XCTAssertNil(AnarlogSessionsCursorCodec.decodeOrNil(""))
    }

    func testSessionsDecodeMalformedReturnsNil() {
        XCTAssertNil(AnarlogSessionsCursorCodec.decodeOrNil("not-json"))
        XCTAssertNil(AnarlogSessionsCursorCodec.decodeOrNil("{\"a\": {}}"))
    }

    func testSessionsRoundTripWithRecordHash() throws {
        let map: [String: AnarlogSessionsCursorEntry] = [
            "uuid-1": AnarlogSessionsCursorEntry(recordHash: "abc"),
            "uuid-2": AnarlogSessionsCursorEntry(recordHash: "pqr"),
        ]
        let encoded = try AnarlogSessionsCursorCodec.encode(map)
        let decoded = try XCTUnwrap(AnarlogSessionsCursorCodec.decodeOrNil(encoded))
        XCTAssertEqual(decoded, map)
    }

    func testSessionsEncodeIsByteStable() throws {
        let encoded = try AnarlogSessionsCursorCodec.encode([
            "b": .init(recordHash: "h2"),
            "a": .init(recordHash: "h1"),
        ])
        XCTAssertEqual(encoded, #"{"a":{"record_hash":"h1"},"b":{"record_hash":"h2"}}"#)
    }

    func testSessionsLegacyFileTreeCursorDecodesNil() throws {
        let fileTreeEntry = #"{"meta_hash":"m","summary_hash":"s","memo_hash":"n","payload_hash":"p"}"#
        let floorSkipEntry = #"{"meta_hash":"floor_skip","payload_hash":""}"#
        XCTAssertNil(AnarlogSessionsCursorCodec.decodeOrNil("{\"a\":\(fileTreeEntry)}"))
        XCTAssertNil(AnarlogSessionsCursorCodec.decodeOrNil("{\"a\":\(floorSkipEntry)}"))
        XCTAssertNil(AnarlogSessionsCursorCodec.decodeOrNil(
            "{\"a\":{\"record_hash\":\"h\"},\"b\":\(fileTreeEntry)}"))
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
