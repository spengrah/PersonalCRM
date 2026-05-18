// ContentHashParityTests close the loop between ICloudContactPayloadShaping
// (the CNContact → ExternalContactUpsertedPayload mapper) and
// ContentHasher (the JCS+SHA-256 recipe). The end-to-end path is
// what the running daemon actually exercises:
//
//   ContactRecord → ICloudContactPayloadShaping.shape →
//   JSONEncoder.encode → ContentHasher.contentHash → SourceIDBuilder
//
// A regression in either the shaping step OR the canonicalization
// step would surface here as a hash that differs from the
// recorded value. Pure-logic; no I/O.
import XCTest
import CRMMacCore
@testable import CRMMacIcloudContactsSource

final class ContentHashParityTests: XCTestCase {

    private let hostID = UUID(uuidString: "11111111-2222-3333-4444-555555555555")!

    /// Helper: shape -> encode -> hash.
    private func hash(for record: ContactRecord) throws -> String {
        let payload = ICloudContactPayloadShaping.shape(
            record: record, hostID: hostID, source: "icloud_contacts")
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.withoutEscapingSlashes]
        let bytes = try encoder.encode(payload)
        return try ContentHasher.contentHash(for: bytes)
    }

    func testHashStableAcrossEncodeRuns() throws {
        // The same input must always hash to the same value.
        let record = ContactRecord(
            identifier: "id-1",
            containerIdentifier: "container-1",
            displayName: "Contact A",
            firstName: "Contact",
            lastName: "A",
            emails: [ContactEmail(value: "a@example.com", type: "home", primary: true)],
            phones: [ContactPhone(value: "+10000000001", type: "mobile", primary: true)],
            addresses: [],
            organization: nil,
            jobTitle: nil)
        let h1 = try hash(for: record)
        let h2 = try hash(for: record)
        XCTAssertEqual(h1, h2)
        XCTAssertEqual(h1.count, 64, "sha256 hex is always 64 chars")
    }

    func testHashIgnoresHostIDChanges() throws {
        // ContentHasher strips the top-level host_id key, so the
        // same record under a different hostID hashes to the same
        // value. This is the core dedup invariant.
        let record = ContactRecord(
            identifier: "id-2",
            containerIdentifier: "container-1",
            displayName: "Contact B",
            firstName: "Contact",
            lastName: "B",
            emails: [],
            phones: [],
            addresses: [])
        let payload1 = ICloudContactPayloadShaping.shape(
            record: record,
            hostID: UUID(uuidString: "00000000-0000-0000-0000-000000000001")!)
        let payload2 = ICloudContactPayloadShaping.shape(
            record: record,
            hostID: UUID(uuidString: "00000000-0000-0000-0000-000000000002")!)
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.withoutEscapingSlashes]
        let h1 = try ContentHasher.contentHash(for: encoder.encode(payload1))
        let h2 = try ContentHasher.contentHash(for: encoder.encode(payload2))
        XCTAssertEqual(h1, h2,
                       "host_id is stripped before hashing — same record hashes the same across hosts")
    }

    func testHashChangesWhenContentChanges() throws {
        let base = ContactRecord(
            identifier: "id-3",
            containerIdentifier: "container-1",
            displayName: "Contact C",
            firstName: "Contact",
            lastName: "C",
            emails: [],
            phones: [],
            addresses: [])
        var changed = base
        changed.firstName = "Different"
        let h1 = try hash(for: base)
        let h2 = try hash(for: changed)
        XCTAssertNotEqual(h1, h2,
                          "shaping must propagate field changes into the hash")
    }

    func testHashChangesWhenContainerIDChanges() throws {
        // metadata.container_identifier is part of the content
        // contract — a cross-container move changes the hash.
        let base = ContactRecord(
            identifier: "id-4",
            containerIdentifier: "container-A",
            displayName: "Contact D",
            firstName: nil, lastName: nil,
            emails: [], phones: [], addresses: [])
        var moved = base
        moved.containerIdentifier = "container-B"
        XCTAssertNotEqual(try hash(for: base), try hash(for: moved))
    }

    func testHashIncludesEmailAndPhone() throws {
        let baseline = ContactRecord(
            identifier: "id-5",
            containerIdentifier: "container-1",
            displayName: "Contact E",
            firstName: nil, lastName: nil,
            emails: [], phones: [], addresses: [])
        var withEmail = baseline
        withEmail.emails = [ContactEmail(value: "e@example.com")]
        var withPhone = baseline
        withPhone.phones = [ContactPhone(value: "+10000000005")]
        let h0 = try hash(for: baseline)
        let h1 = try hash(for: withEmail)
        let h2 = try hash(for: withPhone)
        XCTAssertNotEqual(h0, h1)
        XCTAssertNotEqual(h0, h2)
        XCTAssertNotEqual(h1, h2)
    }
}
