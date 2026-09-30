// Coverage for AnarlogSessionsPayloadShaping's CLI record projection.
import XCTest
import CRMMacCore
@testable import CRMMacAnarlogSource

final class AnarlogSessionsPayloadShapingTests: XCTestCase {

    private let hostID = UUID(uuidString: "11111111-2222-3333-4444-555555555555")!
    private let sessionUUID = "0abbbbbb-0000-4000-8000-000000006c87"
    private let operatorPersonID = "99999999-9999-9999-9999-999999999999"
    private let createdAt = ISO8601DateFormatter().date(from: "2026-03-16T20:34:49Z")!

    private func makeRecord(
        title: String? = "Session A",
        createdAt: Date? = nil,
        memo: String? = nil,
        summaries: [String] = [],
        participantIDs: [String] = ["11111111-1111-1111-1111-111111111111"]
    ) -> AnarlogSessionRecord {
        AnarlogSessionRecord(
            id: sessionUUID,
            title: title,
            createdAt: createdAt ?? self.createdAt,
            memo: memo,
            summaries: summaries,
            participants: participantIDs.map {
                AnarlogParticipant(personID: $0, displayName: nil, email: nil, jobTitle: nil)
            })
    }

    private func shape(
        _ record: AnarlogSessionRecord
    ) -> MeetingNoteRecordedPayload {
        AnarlogSessionsPayloadShaping.shape(
            record: record, operatorPersonID: operatorPersonID, hostID: hostID)
    }

    func testFullShape() {
        let payload = shape(makeRecord(memo: "memo body", summaries: ["summary body"]))
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
        let payload = shape(makeRecord(memo: nil, summaries: [""]))
        XCTAssertNil(payload.summary)
        XCTAssertNil(payload.memo)
    }

    func testDeletedPayloadShape() {
        let payload = AnarlogSessionsPayloadShaping.shapeDeleted(
            sessionID: sessionUUID, hostID: hostID)
        XCTAssertEqual(payload.version, 1)
        XCTAssertEqual(payload.source, "anarlog_sessions")
        XCTAssertEqual(payload.sourceID, sessionUUID)
    }

    func testEncodedWireShapeUsesSnakeCase() throws {
        let payload = shape(makeRecord(memo: "m", summaries: ["s"]))
        let data = try JSONEncoder().encode(payload)
        let json = String(data: data, encoding: .utf8)!
        XCTAssertTrue(json.contains("\"host_id\":\"11111111-2222-3333-4444-555555555555\""))
        XCTAssertTrue(json.contains("\"source_id\":\"\(sessionUUID)\""))
        XCTAssertTrue(json.contains("\"meeting_at\":\""))
        XCTAssertTrue(json.contains("\"participant_ids\":["))
        XCTAssertFalse(json.contains("\"hostID\""))
    }

    func testEncodedMeetingAtRoundTrips() throws {
        let original = createdAt
        let payload = shape(makeRecord(createdAt: original))
        let data = try JSONEncoder().encode(payload)
        let any = try JSONSerialization.jsonObject(with: data) as! [String: Any]
        let raw = any["meeting_at"] as! String
        let parsed = AnarlogTimestampParser.parse(raw)
        XCTAssertEqual(parsed, original)
    }

    func testUnicodeTitleRoundTrips() throws {
        let record = makeRecord(title: "Meeting with Aleks - kickoff smile-emoji")
        let payload = shape(record)
        let data = try JSONEncoder().encode(payload)
        let any = try JSONSerialization.jsonObject(with: data) as! [String: Any]
        XCTAssertEqual(any["title"] as? String, record.title)
    }

    func testTagsAlwaysEmptyInV1() {
        XCTAssertEqual(shape(makeRecord()).tags, [])
    }

    func testOperatorAndZeroIDExcludedInOrder() {
        let first = "11111111-1111-1111-1111-111111111111"
        let last = "22222222-2222-2222-2222-222222222222"
        let record = makeRecord(participantIDs: [
            first,
            operatorPersonID.uppercased(),
            CRMMacAnarlogSource.selfHumanUUID,
            last,
        ])
        XCTAssertEqual(shape(record).participantIDs, [first, last])
    }

    func testFirstSummaryIsSent() {
        XCTAssertEqual(shape(makeRecord(summaries: ["first", "second"])).summary, "first")
    }

    func testNilTitleOmitsKey() throws {
        let payload = shape(makeRecord(title: nil))
        let data = try JSONEncoder().encode(payload)
        let any = try JSONSerialization.jsonObject(with: data) as! [String: Any]
        XCTAssertNil(any["title"])
    }

    func testEncodedKeySetIsUnchanged() throws {
        let record = makeRecord(
            memo: "memo",
            summaries: ["summary"],
            participantIDs: ["11111111-1111-1111-1111-111111111111"])
        let data = try JSONEncoder().encode(shape(record))
        let any = try JSONSerialization.jsonObject(with: data) as! [String: Any]
        XCTAssertEqual(Set(any.keys), Set([
            "version", "host_id", "source", "source_id", "title", "meeting_at",
            "summary", "memo", "participant_ids", "tags",
        ]))
        XCTAssertEqual(any["version"] as? Int, 1)
    }
}
