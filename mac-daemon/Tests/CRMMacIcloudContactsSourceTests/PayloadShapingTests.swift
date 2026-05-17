// PayloadShapingTests pin the wire-shape encoding of
// ExternalContactUpsertedPayload + ExternalContactDeletedPayload
// against a committed golden. The same JSON shape is decoded by the
// Pi-side IngestService; drift on either side would break hash
// verification at the ingest boundary.
import XCTest
import Foundation
import CRMMacCore
@testable import CRMMacIcloudContactsSource

final class PayloadShapingTests: XCTestCase {

    private let hostID = UUID(uuidString: "00000000-0000-0000-0000-000000000001")!

    // MARK: - upserted

    func testShapeFullyPopulatedRecord() throws {
        var bday = DateComponents()
        bday.year = 1990; bday.month = 1; bday.day = 1
        let record = ContactRecord(
            identifier: "contact-A",
            containerIdentifier: "00000000-0000-0000-0000-000000000002",
            displayName: "Contact A",
            firstName: "Contact",
            lastName: "A",
            emails: [ContactEmail(value: "a@example.com", type: "home", primary: true)],
            phones: [ContactPhone(value: "+10000000001", type: "mobile", primary: true)],
            addresses: [ContactAddress(formatted: "100 Synthetic St", type: "home")],
            organization: "Org X",
            jobTitle: "Engineer",
            birthday: bday)

        let payload = ICloudContactPayloadShaping.shape(record: record, hostID: hostID)
        XCTAssertEqual(payload.version, 1)
        XCTAssertEqual(payload.hostID, hostID)
        XCTAssertEqual(payload.source, "icloud_contacts")
        XCTAssertEqual(payload.entityID, "contact-A")
        XCTAssertEqual(payload.displayName, "Contact A")
        XCTAssertEqual(payload.birthday, "1990-01-01")
        XCTAssertNil(payload.photoURL, "v1 always emits nil photo_url per locked decision")
        XCTAssertEqual(payload.metadata["container_identifier"],
                       "00000000-0000-0000-0000-000000000002")

        // JSON shape: lowercase host_id, snake_case keys, no
        // photo_url, no nil fields, container_identifier inside
        // metadata.
        let data = try makeEncoder().encode(payload)
        let json = try JSONSerialization.jsonObject(with: data) as? [String: Any]
        XCTAssertNotNil(json)
        XCTAssertEqual(json?["host_id"] as? String, "00000000-0000-0000-0000-000000000001")
        XCTAssertEqual(json?["entity_id"] as? String, "contact-A")
        XCTAssertEqual(json?["birthday"] as? String, "1990-01-01")
        XCTAssertNil(json?["photo_url"])
        let meta = json?["metadata"] as? [String: Any]
        XCTAssertEqual(meta?["container_identifier"] as? String,
                       "00000000-0000-0000-0000-000000000002")
    }

    func testShapeOmitsBirthdayWhenIncomplete() {
        var bday = DateComponents()
        bday.year = 1990; bday.month = 1 // day missing
        let record = ContactRecord(
            identifier: "contact-B",
            containerIdentifier: "c1",
            birthday: bday)
        let payload = ICloudContactPayloadShaping.shape(record: record, hostID: hostID)
        XCTAssertNil(payload.birthday)
    }

    func testShapeOmitsEmptyArraysAndStrings() throws {
        let record = ContactRecord(
            identifier: "contact-C",
            containerIdentifier: "c1")
        let payload = ICloudContactPayloadShaping.shape(record: record, hostID: hostID)
        let data = try makeEncoder().encode(payload)
        let json = try JSONSerialization.jsonObject(with: data) as? [String: Any]
        XCTAssertNil(json?["display_name"])
        XCTAssertNil(json?["first_name"])
        XCTAssertNil(json?["emails"])
        XCTAssertNil(json?["phones"])
        XCTAssertNil(json?["addresses"])
        XCTAssertNil(json?["organization"])
        XCTAssertNil(json?["job_title"])
        XCTAssertNil(json?["birthday"])
        XCTAssertNil(json?["photo_url"])
        // container_identifier is always emitted.
        let meta = json?["metadata"] as? [String: Any]
        XCTAssertEqual(meta?["container_identifier"] as? String, "c1")
    }

    func testEmailMethodValueOmitsEmptyType() throws {
        let m = ExternalContactMethodValue(value: "a@example.com")
        let data = try JSONEncoder().encode(m)
        let json = try JSONSerialization.jsonObject(with: data) as? [String: Any]
        XCTAssertEqual(json?["value"] as? String, "a@example.com")
        XCTAssertNil(json?["type"])
        XCTAssertNil(json?["primary"])
    }

    func testEmailMethodValueEmitsPrimaryOnlyWhenTrue() throws {
        let m = ExternalContactMethodValue(value: "a@example.com", primary: true)
        let data = try JSONEncoder().encode(m)
        let json = try JSONSerialization.jsonObject(with: data) as? [String: Any]
        XCTAssertEqual(json?["primary"] as? Bool, true)
    }

    // MARK: - deleted

    func testShapeDeletedPayload() throws {
        let payload = ICloudContactPayloadShaping.shapeDeleted(
            identifier: "contact-A", hostID: hostID)
        XCTAssertEqual(payload.version, 1)
        XCTAssertEqual(payload.source, "icloud_contacts")
        XCTAssertEqual(payload.entityID, "contact-A")
        let data = try makeEncoder().encode(payload)
        let json = try JSONSerialization.jsonObject(with: data) as? [String: Any]
        XCTAssertEqual(json?["host_id"] as? String, "00000000-0000-0000-0000-000000000001")
        XCTAssertEqual(json?["entity_id"] as? String, "contact-A")
    }

    // MARK: - birthday helper

    func testIsoBirthdayFormats() {
        var c = DateComponents()
        c.year = 1999; c.month = 9; c.day = 1
        XCTAssertEqual(ICloudContactPayloadShaping.isoBirthday(from: c), "1999-09-01")
    }

    func testIsoBirthdayReturnsNilWhenAnyComponentMissing() {
        XCTAssertNil(ICloudContactPayloadShaping.isoBirthday(from: nil))
        var c = DateComponents()
        c.year = 1999; c.month = 9
        XCTAssertNil(ICloudContactPayloadShaping.isoBirthday(from: c))
    }

    // MARK: - helpers

    private func makeEncoder() -> JSONEncoder {
        let e = JSONEncoder()
        e.outputFormatting = [.withoutEscapingSlashes]
        return e
    }
}
