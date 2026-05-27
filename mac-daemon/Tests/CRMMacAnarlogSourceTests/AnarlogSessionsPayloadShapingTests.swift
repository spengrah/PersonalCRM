// Coverage for AnarlogSessionsPayloadShaping. Verifies:
//   - shape produces the right wire field set
//   - empty summary / memo become nil (omitted on the wire)
//   - participantIDs come straight from the meta
//   - isPreBackfillFloor returns true for sessions older than 2026-01-01
//   - meeting_at encodes with fractional-second ISO8601 + Z
//   - unicode title roundtrips intact
import XCTest
import CRMMacCore
@testable import CRMMacAnarlogSource

final class AnarlogSessionsPayloadShapingTests: XCTestCase {

    private let hostID = UUID(uuidString: "11111111-2222-3333-4444-555555555555")!
    private let sessionUUID = "0a631ec3-fa11-47d2-aa0f-17b320866c87"

    private func makeMeta(
        title: String = "Session A",
        createdAt: Date = ISO8601DateFormatter().date(from: "2026-03-16T20:34:49Z")!,
        userID: String = "00000000-0000-0000-0000-000000000000",
        participantHumanIDs: [String] = ["11111111-1111-1111-1111-111111111111"]
    ) -> AnarlogSessionMeta {
        AnarlogSessionMeta(
            uuid: sessionUUID,
            title: title,
            createdAt: createdAt,
            userID: userID,
            participants: participantHumanIDs.map { AnarlogSessionParticipant(humanID: $0) })
    }

    func testFullShape() {
        let meta = makeMeta()
        let payload = AnarlogSessionsPayloadShaping.shape(
            meta: meta,
            summary: "summary body",
            memo: "memo body",
            hostID: hostID)
        XCTAssertEqual(payload.version, 1)
        XCTAssertEqual(payload.source, "anarlog_sessions")
        XCTAssertEqual(payload.sourceID, sessionUUID)
        XCTAssertEqual(payload.title, "Session A")
        XCTAssertEqual(payload.summary, "summary body")
        XCTAssertEqual(payload.memo, "memo body")
        XCTAssertEqual(payload.participantIDs,
                       ["11111111-1111-1111-1111-111111111111"])
        XCTAssertEqual(payload.tags, [])
    }

    func testEmptyOptionalsBecomeNil() {
        let meta = makeMeta()
        let payload = AnarlogSessionsPayloadShaping.shape(
            meta: meta, summary: "", memo: nil, hostID: hostID)
        XCTAssertNil(payload.summary)
        XCTAssertNil(payload.memo)
    }

    func testPreBackfillFloorDetection() {
        let before = ISO8601DateFormatter().date(from: "2025-12-31T23:59:59Z")!
        let after = ISO8601DateFormatter().date(from: "2026-01-01T00:00:01Z")!
        let onFloor = CRMMacAnarlogSource.sessionsBackfillFloor
        let metaBefore = makeMeta(createdAt: before)
        let metaAfter = makeMeta(createdAt: after)
        let metaOn = makeMeta(createdAt: onFloor)
        XCTAssertTrue(AnarlogSessionsPayloadShaping.isPreBackfillFloor(metaBefore))
        XCTAssertFalse(AnarlogSessionsPayloadShaping.isPreBackfillFloor(metaAfter))
        XCTAssertFalse(AnarlogSessionsPayloadShaping.isPreBackfillFloor(metaOn))
    }

    func testDeletedPayloadShape() {
        let payload = AnarlogSessionsPayloadShaping.shapeDeleted(
            sessionID: sessionUUID, hostID: hostID)
        XCTAssertEqual(payload.version, 1)
        XCTAssertEqual(payload.source, "anarlog_sessions")
        XCTAssertEqual(payload.sourceID, sessionUUID)
    }

    func testEncodedWireShapeUsesSnakeCase() throws {
        let meta = makeMeta()
        let payload = AnarlogSessionsPayloadShaping.shape(
            meta: meta, summary: "s", memo: "m", hostID: hostID)
        let data = try JSONEncoder().encode(payload)
        let json = String(data: data, encoding: .utf8)!
        XCTAssertTrue(json.contains("\"host_id\":\"11111111-2222-3333-4444-555555555555\""))
        XCTAssertTrue(json.contains("\"source_id\":\"\(sessionUUID)\""))
        XCTAssertTrue(json.contains("\"meeting_at\":\""))
        XCTAssertTrue(json.contains("\"participant_ids\":["))
        XCTAssertFalse(json.contains("\"hostID\""))
    }

    func testEncodedMeetingAtRoundTrips() throws {
        let original = ISO8601DateFormatter().date(from: "2026-03-16T20:34:49Z")!
        let meta = makeMeta(createdAt: original)
        let payload = AnarlogSessionsPayloadShaping.shape(
            meta: meta, summary: nil, memo: nil, hostID: hostID)
        let data = try JSONEncoder().encode(payload)
        let any = try JSONSerialization.jsonObject(with: data) as! [String: Any]
        let raw = any["meeting_at"] as! String
        let parsed = AnarlogTimestampParser.parse(raw)
        XCTAssertEqual(parsed, original)
    }

    func testUnicodeTitleRoundTrips() throws {
        let meta = makeMeta(title: "Meeting with Aleks - kickoff smile-emoji")
        let payload = AnarlogSessionsPayloadShaping.shape(
            meta: meta, summary: nil, memo: nil, hostID: hostID)
        let data = try JSONEncoder().encode(payload)
        let any = try JSONSerialization.jsonObject(with: data) as! [String: Any]
        XCTAssertEqual(any["title"] as? String, meta.title)
    }

    func testTagsAlwaysEmptyInV1() {
        let payload = AnarlogSessionsPayloadShaping.shape(
            meta: makeMeta(), summary: nil, memo: nil, hostID: hostID)
        XCTAssertEqual(payload.tags, [])
    }
}
