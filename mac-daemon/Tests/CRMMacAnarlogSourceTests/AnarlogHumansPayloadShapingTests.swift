// Coverage for AnarlogHumansPayloadShaping. The critical invariants:
//   - displayName falls back to "<no name>" when frontmatter.name empty
//     (Pi rejects empty for matching).
//   - metadata ALWAYS carries pinned + pin_order (so metadata is never
//     empty on the wire).
//   - org_id / user_id / created_at land in metadata when present.
//   - emails arrive as ExternalContactMethodValue with no type and
//     primary=false.
//   - jobTitle becomes nil on empty (omitted on the wire).
import XCTest
import CRMMacCore
@testable import CRMMacAnarlogSource

final class AnarlogHumansPayloadShapingTests: XCTestCase {

    private let hostID = UUID(uuidString: "11111111-2222-3333-4444-555555555555")!

    private func makeRecord(
        name: String = "Contact A",
        emails: [String] = [],
        jobTitle: String = "",
        linkedinUsername: String = "",
        orgID: String = "",
        userID: String = "",
        pinOrder: Int = 0,
        pinned: Bool = false,
        createdAt: Date? = nil,
        memo: String? = nil
    ) -> AnarlogHumanRecord {
        AnarlogHumanRecord(
            uuid: "0a18829e-12b6-40f6-93f8-6307973c926b",
            name: name,
            emails: emails,
            jobTitle: jobTitle,
            linkedinUsername: linkedinUsername,
            orgID: orgID,
            userID: userID,
            pinOrder: pinOrder,
            pinned: pinned,
            createdAt: createdAt,
            memo: memo)
    }

    func testFullRecordRoundTrips() throws {
        let createdAt = ISO8601DateFormatter().date(from: "2026-03-04T07:40:49Z")!
        let rec = makeRecord(
            name: "Contact A",
            emails: ["a@example.invalid", "b@example.invalid"],
            jobTitle: "Engineer",
            linkedinUsername: "example",
            orgID: "org-1",
            userID: "user-1",
            pinOrder: 5,
            pinned: true,
            createdAt: createdAt,
            memo: "memo body")
        let payload = AnarlogHumansPayloadShaping.shape(record: rec, hostID: hostID)
        XCTAssertEqual(payload.version, 1)
        XCTAssertEqual(payload.source, "anarlog_humans")
        XCTAssertEqual(payload.entityID, rec.uuid)
        XCTAssertEqual(payload.displayName, "Contact A")
        XCTAssertEqual(payload.jobTitle, "Engineer")
        XCTAssertEqual(payload.emails.count, 2)
        XCTAssertEqual(payload.emails[0].value, "a@example.invalid")
        XCTAssertNil(payload.emails[0].type)
        XCTAssertFalse(payload.emails[0].primary)

        XCTAssertEqual(payload.metadata["pinned"], "true")
        XCTAssertEqual(payload.metadata["pin_order"], "5")
        XCTAssertEqual(payload.metadata["linkedin_username"], "example")
        XCTAssertEqual(payload.metadata["org_id"], "org-1")
        XCTAssertEqual(payload.metadata["user_id"], "user-1")
        XCTAssertEqual(payload.metadata["memo"], "memo body")
        XCTAssertNotNil(payload.metadata["created_at"])
    }

    func testEmptyNameFallsBackToNoName() {
        let rec = makeRecord(name: "")
        let payload = AnarlogHumansPayloadShaping.shape(record: rec, hostID: hostID)
        XCTAssertEqual(payload.displayName, "<no name>")
    }

    func testMetadataAlwaysCarriesPinnedAndPinOrder() {
        let rec = makeRecord(pinOrder: 0, pinned: false)
        let payload = AnarlogHumansPayloadShaping.shape(record: rec, hostID: hostID)
        XCTAssertEqual(payload.metadata["pinned"], "false")
        XCTAssertEqual(payload.metadata["pin_order"], "0")
    }

    func testEmptyOptionalFieldsOmittedFromMetadata() {
        let rec = makeRecord(linkedinUsername: "", orgID: "", userID: "")
        let payload = AnarlogHumansPayloadShaping.shape(record: rec, hostID: hostID)
        XCTAssertNil(payload.metadata["linkedin_username"])
        XCTAssertNil(payload.metadata["org_id"])
        XCTAssertNil(payload.metadata["user_id"])
        XCTAssertNil(payload.metadata["memo"])
        XCTAssertNil(payload.metadata["created_at"])
    }

    func testEmptyJobTitleBecomesNil() {
        let rec = makeRecord(jobTitle: "")
        let payload = AnarlogHumansPayloadShaping.shape(record: rec, hostID: hostID)
        XCTAssertNil(payload.jobTitle)
    }

    func testEmptyEmailsArrayProducesEmptyList() {
        let rec = makeRecord(emails: [])
        let payload = AnarlogHumansPayloadShaping.shape(record: rec, hostID: hostID)
        XCTAssertEqual(payload.emails, [])
    }

    func testEmailsFilterEmptyStrings() {
        let rec = makeRecord(emails: ["a@example.invalid", "", "b@example.invalid"])
        let payload = AnarlogHumansPayloadShaping.shape(record: rec, hostID: hostID)
        XCTAssertEqual(payload.emails.count, 2)
    }

    func testDeletedPayloadShape() {
        let payload = AnarlogHumansPayloadShaping.shapeDeleted(
            entityID: "uuid", hostID: hostID)
        XCTAssertEqual(payload.version, 1)
        XCTAssertEqual(payload.source, "anarlog_humans")
        XCTAssertEqual(payload.entityID, "uuid")
    }

    func testEncodedWireShapeUsesSnakeCase() throws {
        let rec = makeRecord(
            name: "Contact A",
            emails: ["a@example.invalid"],
            jobTitle: "Eng",
            pinOrder: 3,
            pinned: true)
        let payload = AnarlogHumansPayloadShaping.shape(record: rec, hostID: hostID)
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.sortedKeys]
        let data = try encoder.encode(payload)
        let json = String(data: data, encoding: .utf8)!
        XCTAssertTrue(json.contains("\"host_id\":\"11111111-2222-3333-4444-555555555555\""))
        XCTAssertTrue(json.contains("\"entity_id\":\"\(rec.uuid)\""))
        XCTAssertTrue(json.contains("\"display_name\":\"Contact A\""))
        XCTAssertTrue(json.contains("\"job_title\":\"Eng\""))
        XCTAssertTrue(json.contains("\"source\":\"anarlog_humans\""))
        XCTAssertFalse(json.contains("\"hostID\""))  // camelCase must NOT leak
    }

    func testEncodedWireShapeOmitsEmptyOptionals() throws {
        let rec = makeRecord(jobTitle: "")
        let payload = AnarlogHumansPayloadShaping.shape(record: rec, hostID: hostID)
        let data = try JSONEncoder().encode(payload)
        let json = String(data: data, encoding: .utf8)!
        XCTAssertFalse(json.contains("\"job_title\""))
    }
}
