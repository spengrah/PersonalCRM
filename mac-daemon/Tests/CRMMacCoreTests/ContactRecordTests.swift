// ContactRecordTests verify Codable round-trip for the daemon's
// projection of a CNContact + the three value-type helpers
// (ContactEmail / ContactPhone / ContactAddress).
//
// ContactRecord is not itself a wire shape (the icloud source
// plugin shapes it to ExternalContactUpsertedPayload first), but
// it IS the fixture shape every pure-logic test uses, so locking
// the Codable contract here documents the producer/consumer
// convention.
import XCTest
@testable import CRMMacCore

final class ContactRecordTests: XCTestCase {

    func testFullyPopulatedRoundTrip() throws {
        var components = DateComponents()
        components.year = 1990
        components.month = 1
        components.day = 1

        let record = ContactRecord(
            identifier: "contact-A",
            containerIdentifier: "container-1",
            displayName: "Contact A",
            firstName: "Contact",
            lastName: "A",
            emails: [
                ContactEmail(value: "a@example.com", type: "home", primary: true),
                ContactEmail(value: "work@example.com", type: "work", primary: false),
            ],
            phones: [
                ContactPhone(value: "+10000000001", type: "mobile", primary: true),
            ],
            addresses: [
                ContactAddress(formatted: "100 Synthetic St\nNowhere", type: "home"),
            ],
            organization: "Org X",
            jobTitle: "Engineer",
            birthday: components)

        let data = try JSONEncoder().encode(record)
        let decoded = try JSONDecoder().decode(ContactRecord.self, from: data)
        XCTAssertEqual(record, decoded)
    }

    func testMinimalRoundTripOmitsNils() throws {
        let record = ContactRecord(
            identifier: "contact-B",
            containerIdentifier: "container-1")
        let data = try JSONEncoder().encode(record)
        let decoded = try JSONDecoder().decode(ContactRecord.self, from: data)
        XCTAssertEqual(record, decoded)
        XCTAssertNil(decoded.displayName)
        XCTAssertNil(decoded.birthday)
        XCTAssertEqual(decoded.emails, [])
    }

    func testEmailNonPrimaryDefault() {
        let e = ContactEmail(value: "a@example.com")
        XCTAssertFalse(e.primary)
        XCTAssertNil(e.type)
    }

    func testPhoneNonPrimaryDefault() {
        let p = ContactPhone(value: "+10000000001")
        XCTAssertFalse(p.primary)
        XCTAssertNil(p.type)
    }

    func testAddressNilTypeAllowed() {
        let a = ContactAddress(formatted: "100 Synthetic St")
        XCTAssertNil(a.type)
    }
}
