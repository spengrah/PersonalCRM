// Coverage for AnarlogSessionMetaParser. Field set per spec lines
// 196-198. Required: created_at + parseable timestamp. Self-user UUID
// is filtered from participants per spec line 188.
import XCTest
@testable import CRMMacAnarlogSource

final class AnarlogSessionMetaParserTests: XCTestCase {

    private let uuid = "0a631ec3-fa11-47d2-aa0f-17b320866c87"

    func testFullShapeParses() throws {
        let json = """
        {
          "id": "\(uuid)",
          "title": "session title",
          "created_at": "2026-03-16T20:34:49.936Z",
          "user_id": "00000000-0000-0000-0000-000000000000",
          "participants": [
            {"human_id": "11111111-1111-1111-1111-111111111111", "id": "p1", "session_id": "\(uuid)", "source": "anarlog", "user_id": "00000000-0000-0000-0000-000000000000"},
            {"human_id": "22222222-2222-2222-2222-222222222222"}
          ]
        }
        """
        let meta = try XCTUnwrap(AnarlogSessionMetaParser.parse(
            uuid: uuid, metaJSONBytes: Data(json.utf8)))
        XCTAssertEqual(meta.uuid, uuid)
        XCTAssertEqual(meta.title, "session title")
        XCTAssertEqual(meta.userID, "00000000-0000-0000-0000-000000000000")
        XCTAssertEqual(meta.participants.count, 2)
        XCTAssertEqual(meta.participants[0].humanID, "11111111-1111-1111-1111-111111111111")
        XCTAssertEqual(meta.participants[1].humanID, "22222222-2222-2222-2222-222222222222")
    }

    func testSelfUserIDFilteredFromParticipants() throws {
        // If user_id == participant.human_id → that participant is
        // the recording user and gets filtered.
        let selfID = "33333333-3333-3333-3333-333333333333"
        let json = """
        {
          "title": "t",
          "created_at": "2026-03-16T20:34:49Z",
          "user_id": "\(selfID)",
          "participants": [
            {"human_id": "\(selfID)"},
            {"human_id": "44444444-4444-4444-4444-444444444444"}
          ]
        }
        """
        let meta = try XCTUnwrap(AnarlogSessionMetaParser.parse(
            uuid: uuid, metaJSONBytes: Data(json.utf8)))
        XCTAssertEqual(meta.participants.count, 1)
        XCTAssertEqual(meta.participants[0].humanID, "44444444-4444-4444-4444-444444444444")
    }

    func testWellKnownSelfUUIDSentinelFilteredEvenWithoutUserID() throws {
        let json = """
        {
          "title": "t",
          "created_at": "2026-03-16T20:34:49Z",
          "participants": [
            {"human_id": "\(CRMMacAnarlogSource.selfHumanUUID)"},
            {"human_id": "44444444-4444-4444-4444-444444444444"}
          ]
        }
        """
        let meta = try XCTUnwrap(AnarlogSessionMetaParser.parse(
            uuid: uuid, metaJSONBytes: Data(json.utf8)))
        XCTAssertEqual(meta.participants.count, 1)
        XCTAssertEqual(meta.participants[0].humanID, "44444444-4444-4444-4444-444444444444")
    }

    func testEmptyParticipants() throws {
        let json = """
        {
          "title": "t",
          "created_at": "2026-03-16T20:34:49Z",
          "participants": []
        }
        """
        let meta = try XCTUnwrap(AnarlogSessionMetaParser.parse(
            uuid: uuid, metaJSONBytes: Data(json.utf8)))
        XCTAssertEqual(meta.participants, [])
    }

    func testMissingTitleAcceptedAsEmpty() throws {
        let json = """
        { "created_at": "2026-03-16T20:34:49Z" }
        """
        let meta = try XCTUnwrap(AnarlogSessionMetaParser.parse(
            uuid: uuid, metaJSONBytes: Data(json.utf8)))
        XCTAssertEqual(meta.title, "")
    }

    func testMissingCreatedAtRejected() {
        let json = """
        { "title": "t" }
        """
        XCTAssertNil(AnarlogSessionMetaParser.parse(
            uuid: uuid, metaJSONBytes: Data(json.utf8)))
    }

    func testUnparseableCreatedAtRejected() {
        let json = """
        { "title": "t", "created_at": "not-a-date" }
        """
        XCTAssertNil(AnarlogSessionMetaParser.parse(
            uuid: uuid, metaJSONBytes: Data(json.utf8)))
    }

    func testInvalidJSONRejected() {
        XCTAssertNil(AnarlogSessionMetaParser.parse(
            uuid: uuid, metaJSONBytes: Data("not json".utf8)))
    }

    func testNonObjectJSONRejected() {
        XCTAssertNil(AnarlogSessionMetaParser.parse(
            uuid: uuid, metaJSONBytes: Data("[1,2,3]".utf8)))
    }

    func testMicrosecondTimestampParses() throws {
        let json = """
        { "title": "t", "created_at": "2026-03-04T07:40:49.531658+00:00" }
        """
        let meta = try XCTUnwrap(AnarlogSessionMetaParser.parse(
            uuid: uuid, metaJSONBytes: Data(json.utf8)))
        XCTAssertNotNil(meta.createdAt)
    }

    func testParticipantWithoutHumanIDSkipped() throws {
        let json = """
        {
          "title": "t",
          "created_at": "2026-03-16T20:34:49Z",
          "participants": [
            {"id": "no-human-id"},
            {"human_id": ""},
            {"human_id": "55555555-5555-5555-5555-555555555555"}
          ]
        }
        """
        let meta = try XCTUnwrap(AnarlogSessionMetaParser.parse(
            uuid: uuid, metaJSONBytes: Data(json.utf8)))
        XCTAssertEqual(meta.participants.count, 1)
        XCTAssertEqual(meta.participants[0].humanID, "55555555-5555-5555-5555-555555555555")
    }
}

final class AnarlogPathResolverTests: XCTestCase {
    func testHumansAndSessionsDirsAppend() {
        let humansURL = AnarlogPathResolver.humansDir(rootPath: "/tmp/notes")
        XCTAssertTrue(humansURL.path.hasSuffix("/tmp/notes/humans"))
        let sessionsURL = AnarlogPathResolver.sessionsDir(rootPath: "/tmp/notes")
        XCTAssertTrue(sessionsURL.path.hasSuffix("/tmp/notes/sessions"))
    }

    func testTildeExpansion() {
        let url = AnarlogPathResolver.expand("~/foo")
        XCTAssertFalse(url.path.hasPrefix("~"))
        XCTAssertTrue(url.path.contains("/foo"))
    }

    func testUUIDValidatorAcceptsLowercaseCanonical() {
        XCTAssertEqual(
            AnarlogUUIDValidator.canonicalize("0a18829e-12b6-40f6-93f8-6307973c926b"),
            "0a18829e-12b6-40f6-93f8-6307973c926b")
    }

    func testUUIDValidatorRejectsUppercase() {
        // The parent spec is explicit: case-sensitive lowercase. An
        // uppercase variant might indicate a case-insensitive
        // filesystem renamed a file behind the operator's back, so
        // we don't want to accept and risk cursor key collisions.
        XCTAssertNil(
            AnarlogUUIDValidator.canonicalize("0A18829E-12B6-40F6-93F8-6307973C926B"))
        XCTAssertNil(
            AnarlogUUIDValidator.canonicalize("0a18829e-12b6-40F6-93f8-6307973c926b"),
            "mixed-case must also be rejected")
    }

    func testUUIDValidatorRejectsMalformed() {
        XCTAssertNil(AnarlogUUIDValidator.canonicalize(""))
        XCTAssertNil(AnarlogUUIDValidator.canonicalize("not-a-uuid"))
        XCTAssertNil(AnarlogUUIDValidator.canonicalize("0a18829e-12b6-40f6-93f8-6307973c926"))
    }
}
