// Coverage for the Anarlog participant-to-payload mapping.
// Invariants: ID, display name, email and job title map to the wire
// fields; missing or empty values follow the established wire rules;
// CLI-sourced payload metadata is empty; encoded keys stay exact.
import XCTest
import CRMMacCore
@testable import CRMMacAnarlogSource

final class AnarlogHumansPayloadShapingTests: XCTestCase {

    private let hostID = UUID(uuidString: "11111111-2222-3333-4444-555555555555")!
    private let personID = "0aaaaaaa-0000-4000-8000-00000000000b"

    private func makeParticipant(
        displayName: String? = "Contact A",
        email: String? = nil,
        jobTitle: String? = nil
    ) -> AnarlogParticipant {
        AnarlogParticipant(
            personID: personID,
            displayName: displayName,
            email: email,
            jobTitle: jobTitle)
    }

    func testParticipantMapsToWireFields() {
        let payload = AnarlogHumansPayloadShaping.shape(
            participant: makeParticipant(
                displayName: "Contact A",
                email: "a@example.invalid",
                jobTitle: "Engineer"),
            hostID: hostID)
        XCTAssertEqual(payload.version, 1)
        XCTAssertEqual(payload.source, "anarlog_humans")
        XCTAssertEqual(payload.entityID, personID)
        XCTAssertEqual(payload.displayName, "Contact A")
        XCTAssertEqual(payload.emails, [AnarlogExternalContactMethodValue(value: "a@example.invalid")])
        XCTAssertEqual(payload.jobTitle, "Engineer")
        XCTAssertEqual(payload.metadata, [:])
    }

    func testNilOrEmptyDisplayNameFallsBackToNoName() {
        for displayName in [nil, ""] as [String?] {
            let payload = AnarlogHumansPayloadShaping.shape(
                participant: makeParticipant(displayName: displayName), hostID: hostID)
            XCTAssertEqual(payload.displayName, "<no name>")
        }
    }

    func testNilOrEmptyEmailProducesNoEmails() {
        for email in [nil, ""] as [String?] {
            let payload = AnarlogHumansPayloadShaping.shape(
                participant: makeParticipant(email: email), hostID: hostID)
            XCTAssertEqual(payload.emails, [])
        }
    }

    func testNilOrEmptyJobTitleBecomesNil() {
        for jobTitle in [nil, ""] as [String?] {
            let payload = AnarlogHumansPayloadShaping.shape(
                participant: makeParticipant(jobTitle: jobTitle), hostID: hostID)
            XCTAssertNil(payload.jobTitle)
        }
    }

    func testEncodedWireShapeUsesSnakeCase() throws {
        let participant = makeParticipant(
            displayName: "Contact A",
            email: "a@example.invalid",
            jobTitle: "Eng")
        let payload = AnarlogHumansPayloadShaping.shape(participant: participant, hostID: hostID)
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.sortedKeys]
        let data = try encoder.encode(payload)
        let json = String(data: data, encoding: .utf8)!
        XCTAssertTrue(json.contains("\"host_id\":\"11111111-2222-3333-4444-555555555555\""))
        XCTAssertTrue(json.contains("\"entity_id\":\"\(participant.personID)\""))
        XCTAssertTrue(json.contains("\"display_name\":\"Contact A\""))
        XCTAssertTrue(json.contains("\"job_title\":\"Eng\""))
        XCTAssertTrue(json.contains("\"source\":\"anarlog_humans\""))
        XCTAssertFalse(json.contains("\"hostID\""))  // camelCase must NOT leak
    }

    func testEncodedWireShapeOmitsEmptyOptionals() throws {
        let payload = AnarlogHumansPayloadShaping.shape(
            participant: makeParticipant(jobTitle: nil), hostID: hostID)
        let data = try JSONEncoder().encode(payload)
        let json = String(data: data, encoding: .utf8)!
        XCTAssertFalse(json.contains("\"job_title\""))
    }

    func testEncodedKeySetIsExact() throws {
        let payload = AnarlogHumansPayloadShaping.shape(
            participant: makeParticipant(
                displayName: "Contact A",
                email: "a@example.invalid",
                jobTitle: "Engineer"),
            hostID: hostID)
        let data = try JSONEncoder().encode(payload)
        let object = try XCTUnwrap(JSONSerialization.jsonObject(with: data) as? [String: Any])
        XCTAssertEqual(Set(object.keys), Set([
            "version", "host_id", "source", "entity_id", "display_name", "emails", "job_title",
        ]))
        XCTAssertEqual(object["version"] as? Int, 1)
        XCTAssertEqual(object["emails"] as? [[String: String]], [["value": "a@example.invalid"]])
    }

    func testEncodedKeySetOmitsEmptyOptionals() throws {
        let payload = AnarlogHumansPayloadShaping.shape(
            participant: makeParticipant(email: nil, jobTitle: nil), hostID: hostID)
        let data = try JSONEncoder().encode(payload)
        let object = try XCTUnwrap(JSONSerialization.jsonObject(with: data) as? [String: Any])
        XCTAssertEqual(Set(object.keys), Set([
            "version", "host_id", "source", "entity_id", "display_name",
        ]))
    }
}
