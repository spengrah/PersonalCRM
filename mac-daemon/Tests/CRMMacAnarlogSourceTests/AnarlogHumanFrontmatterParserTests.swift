// Coverage for AnarlogHumanFrontmatterParser. Tolerance per the
// parser's inline notes: missing optional fields are fine; missing
// closer is fatal; unknown keys land in rawExtras; arrays accept
// the v1.0.1 `[]` flow-style format observed in real notes.
import XCTest
@testable import CRMMacAnarlogSource

final class AnarlogHumanFrontmatterParserTests: XCTestCase {

    private let uuid = "0a18829e-12b6-40f6-93f8-6307973c926b"

    func testMinimalFrontmatterParses() throws {
        let body = """
        ---
        name: contact-a
        emails: []
        job_title: ''
        linkedin_username: ''
        org_id: ''
        pin_order: 0
        pinned: false
        user_id: 00000000-0000-0000-0000-000000000000
        ---
        """
        let rec = try XCTUnwrap(AnarlogHumanFrontmatterParser.parse(
            uuid: uuid, fileBytes: Data(body.utf8)))
        XCTAssertEqual(rec.uuid, uuid)
        XCTAssertEqual(rec.name, "contact-a")
        XCTAssertEqual(rec.emails, [])
        XCTAssertEqual(rec.jobTitle, "")
        XCTAssertEqual(rec.linkedinUsername, "")
        XCTAssertEqual(rec.orgID, "")
        XCTAssertEqual(rec.pinOrder, 0)
        XCTAssertFalse(rec.pinned)
        XCTAssertEqual(rec.userID, "00000000-0000-0000-0000-000000000000")
        XCTAssertNil(rec.createdAt)
        XCTAssertNil(rec.memo)
    }

    func testWithCreatedAtMicroOffset() throws {
        let body = """
        ---
        name: contact-a
        created_at: 2026-03-04T07:40:49.531658+00:00
        pin_order: 2
        pinned: true
        ---
        """
        let rec = try XCTUnwrap(AnarlogHumanFrontmatterParser.parse(
            uuid: uuid, fileBytes: Data(body.utf8)))
        XCTAssertNotNil(rec.createdAt)
        XCTAssertEqual(rec.pinOrder, 2)
        XCTAssertTrue(rec.pinned)
    }

    func testCreatedAtParseFailureGoesToRawExtras() throws {
        let body = """
        ---
        name: contact-a
        created_at: not-a-date
        ---
        """
        let rec = try XCTUnwrap(AnarlogHumanFrontmatterParser.parse(
            uuid: uuid, fileBytes: Data(body.utf8)))
        XCTAssertNil(rec.createdAt)
        XCTAssertEqual(rec.rawExtras["created_at"], "not-a-date")
    }

    func testEmailsArrayParse() throws {
        let body = """
        ---
        name: contact-a
        emails: [a@example.invalid, b@example.invalid, c@example.invalid]
        ---
        """
        let rec = try XCTUnwrap(AnarlogHumanFrontmatterParser.parse(
            uuid: uuid, fileBytes: Data(body.utf8)))
        XCTAssertEqual(rec.emails.count, 3)
        XCTAssertTrue(rec.emails.allSatisfy { $0.hasSuffix("@example.invalid") })
    }

    func testEmailsArrayWithQuotedEntries() throws {
        let body = """
        ---
        emails: ['a@example.invalid', "b@example.invalid"]
        ---
        """
        let rec = try XCTUnwrap(AnarlogHumanFrontmatterParser.parse(
            uuid: uuid, fileBytes: Data(body.utf8)))
        XCTAssertEqual(rec.emails, ["a@example.invalid", "b@example.invalid"])
    }

    func testQuotedScalarsStripQuotes() throws {
        let body = """
        ---
        job_title: 'Engineer'
        linkedin_username: "exampleuser"
        ---
        """
        let rec = try XCTUnwrap(AnarlogHumanFrontmatterParser.parse(
            uuid: uuid, fileBytes: Data(body.utf8)))
        XCTAssertEqual(rec.jobTitle, "Engineer")
        XCTAssertEqual(rec.linkedinUsername, "exampleuser")
    }

    func testBodyBecomesMemo() throws {
        let body = """
        ---
        name: contact-a
        ---
        First line of memo.

        Second line.
        """
        let rec = try XCTUnwrap(AnarlogHumanFrontmatterParser.parse(
            uuid: uuid, fileBytes: Data(body.utf8)))
        XCTAssertEqual(rec.memo, "First line of memo.\n\nSecond line.")
    }

    func testEmptyBodyMemoIsNil() throws {
        let body = """
        ---
        name: contact-a
        ---
        """
        let rec = try XCTUnwrap(AnarlogHumanFrontmatterParser.parse(
            uuid: uuid, fileBytes: Data(body.utf8)))
        XCTAssertNil(rec.memo)
    }

    func testMissingCloserReturnsNil() {
        let body = """
        ---
        name: contact-a
        """
        XCTAssertNil(AnarlogHumanFrontmatterParser.parse(
            uuid: uuid, fileBytes: Data(body.utf8)))
    }

    func testMissingOpenerReturnsNil() {
        let body = """
        name: contact-a
        emails: []
        """
        XCTAssertNil(AnarlogHumanFrontmatterParser.parse(
            uuid: uuid, fileBytes: Data(body.utf8)))
    }

    func testNonUTF8ReturnsNil() {
        let bytes = Data([0xff, 0xfe, 0xfd])
        XCTAssertNil(AnarlogHumanFrontmatterParser.parse(
            uuid: uuid, fileBytes: bytes))
    }

    func testUnknownKeysGoToRawExtras() throws {
        let body = """
        ---
        name: contact-a
        future_field: hello
        another_one: world
        ---
        """
        let rec = try XCTUnwrap(AnarlogHumanFrontmatterParser.parse(
            uuid: uuid, fileBytes: Data(body.utf8)))
        XCTAssertEqual(rec.rawExtras["future_field"], "hello")
        XCTAssertEqual(rec.rawExtras["another_one"], "world")
    }

    func testWindowsLineEndings() throws {
        let body = "---\r\nname: contact-a\r\nemails: []\r\n---\r\nmemo line\r\n"
        let rec = try XCTUnwrap(AnarlogHumanFrontmatterParser.parse(
            uuid: uuid, fileBytes: Data(body.utf8)))
        XCTAssertEqual(rec.name, "contact-a")
        XCTAssertEqual(rec.memo, "memo line")
    }

    func testValueWithColonInside() throws {
        // Common shape: a memo / value containing a colon. The parser
        // splits on the FIRST `:` only.
        let body = """
        ---
        name: contact: with-colon
        ---
        """
        let rec = try XCTUnwrap(AnarlogHumanFrontmatterParser.parse(
            uuid: uuid, fileBytes: Data(body.utf8)))
        XCTAssertEqual(rec.name, "contact: with-colon")
    }
}
