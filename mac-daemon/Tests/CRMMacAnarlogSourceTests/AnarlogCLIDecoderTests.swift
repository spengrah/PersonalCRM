import Foundation
import CRMMacCore
import XCTest
@testable import CRMMacAnarlogSource

final class AnarlogCLIDecoderTests: XCTestCase {

    private enum Change {
        case absent
        case wrongType
        case null
    }

    func testListPageDecodesRequiredFieldsAndPagination() throws {
        let decoded = try AnarlogCLIDecoder.listPage(data(listObject()))
        XCTAssertEqual(decoded.entries, [
            AnarlogSessionListEntry(
                id: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
                createdAt: try XCTUnwrap(AnarlogTimestampParser.parse("2026-03-16T20:34:49.936Z"))),
        ])
        XCTAssertEqual(decoded.nextOffset, 200)
    }

    func testSessionDecodesUsedFieldsAndIgnoresUnknownKeys() throws {
        let decoded = try AnarlogCLIDecoder.session(data(getObject()))
        XCTAssertEqual(decoded.id, "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
        XCTAssertEqual(decoded.title, "")
        XCTAssertEqual(decoded.createdAt, try XCTUnwrap(
            AnarlogTimestampParser.parse("2026-03-16T20:34:49.936+02:00")))
        XCTAssertEqual(decoded.memo, "memo")
        XCTAssertEqual(decoded.summaries, ["first", "second"])
        XCTAssertEqual(decoded.participants, [
            AnarlogParticipant(
                personID: "11111111-2222-3333-4444-555555555555",
                displayName: "",
                email: nil,
                jobTitle: "Engineer"),
        ])
    }

    func testNullNoteProducesNilMemo() throws {
        var root = getObject()
        var session = root["data"] as! [String: Any]
        session["note"] = NSNull()
        root["data"] = session
        XCTAssertNil(try AnarlogCLIDecoder.session(data(root)).memo)
    }

    func testBothTimestampFormsParse() throws {
        let z = try AnarlogCLIDecoder.listPage(data(listObject()))
        XCTAssertEqual(z.entries[0].createdAt,
                       AnarlogTimestampParser.parse("2026-03-16T20:34:49.936Z"))

        var root = listObject()
        var entries = root["data"] as! [[String: Any]]
        entries[0]["created_at"] = "2026-03-16T20:34:49.936+02:00"
        root["data"] = entries
        let offset = try AnarlogCLIDecoder.listPage(data(root))
        XCTAssertEqual(offset.entries[0].createdAt,
                       AnarlogTimestampParser.parse("2026-03-16T20:34:49.936+02:00"))
    }

    func testUnknownKeysAtUsedLevelsAreIgnored() throws {
        var list = listObject()
        list["future_root"] = ["anything": true]
        var listEntry = (list["data"] as! [[String: Any]])[0]
        listEntry["future_entry"] = 17
        list["data"] = [listEntry]
        list["future_pagination"] = "ignored"
        XCTAssertNoThrow(try AnarlogCLIDecoder.listPage(data(list)))

        var record = getObject()
        record["future_root"] = NSNull()
        var session = record["data"] as! [String: Any]
        session["future_data"] = 17
        var note = session["note"] as! [String: Any]
        note["future_note"] = true
        session["note"] = note
        var summaries = session["summaries"] as! [[String: Any]]
        summaries[0]["future_summary"] = "ignored"
        session["summaries"] = summaries
        var participants = session["participants"] as! [[String: Any]]
        participants[0]["future_participant"] = "ignored"
        session["participants"] = participants
        record["data"] = session
        XCTAssertNoThrow(try AnarlogCLIDecoder.session(data(record)))
    }

    func testListUsedFieldsRejectAbsentWrongTypeAndDisallowedNull() {
        let paths = [
            "data",
            "data[].id",
            "data[].created_at",
            "pagination",
            "pagination.next_offset",
        ]
        for path in paths {
            assertMissingField(command: "meetings list", field: path, change: .absent)
            assertMissingField(command: "meetings list", field: path, change: .wrongType)
            if path != "pagination.next_offset" {
                assertMissingField(command: "meetings list", field: path, change: .null)
            }
        }
    }

    func testGetUsedFieldsRejectAbsentWrongTypeAndDisallowedNull() {
        let required = [
            "data",
            "data.id",
            "data.created_at",
            "data.note.markdown",
            "data.summaries",
            "data.summaries[].markdown",
            "data.participants",
            "data.participants[].human_id",
        ]
        for path in required {
            assertMissingField(command: "meetings get", field: path, change: .absent)
            assertMissingField(command: "meetings get", field: path, change: .wrongType)
            assertMissingField(command: "meetings get", field: path, change: .null)
        }

        let nullable = [
            "data.title",
            "data.note",
            "data.participants[].display_name",
            "data.participants[].email",
            "data.participants[].job_title",
        ]
        for path in nullable {
            assertMissingField(command: "meetings get", field: path, change: .absent)
            assertMissingField(command: "meetings get", field: path, change: .wrongType)
        }
    }

    func testUnparseableCreatedAtUsesExactFieldPath() {
        var list = listObject()
        var entry = (list["data"] as! [[String: Any]])[0]
        entry["created_at"] = "not-a-date"
        list["data"] = [entry]
        assertFailure(.missingField(command: "meetings list", field: "data[].created_at")) {
            try AnarlogCLIDecoder.listPage(data(list))
        }

        var record = getObject()
        var session = record["data"] as! [String: Any]
        session["created_at"] = "not-a-date"
        record["data"] = session
        assertFailure(.missingField(command: "meetings get", field: "data.created_at")) {
            try AnarlogCLIDecoder.session(data(record))
        }
    }

    func testSchemaVersionIsRequiredAndPinnedForBothCommands() {
        for command in ["meetings list", "meetings get"] {
            assertEnvelopeFailure(command: command, schema: nil,
                                  expected: .missingField(command: command, field: "schema_version"))
            assertEnvelopeFailure(command: command, schema: 1,
                                  expected: .missingField(command: command, field: "schema_version"))
            assertEnvelopeFailure(command: command, schema: "2",
                                  expected: .unsupportedSchemaVersion("2"))
        }
    }

    func testMalformedStdoutAndNonObjectRoot() {
        assertFailure(.malformedOutput(command: "meetings list")) {
            try AnarlogCLIDecoder.envelope(Data([0xff]), command: "meetings list")
        }
        assertFailure(.malformedOutput(command: "meetings get")) {
            try AnarlogCLIDecoder.envelope(Data("[1,2,3]".utf8), command: "meetings get")
        }
        assertFailure(.malformedOutput(command: "meetings get")) {
            try AnarlogCLIDecoder.envelope(Data("not json".utf8), command: "meetings get")
        }
    }

    func testErrorCodeReturnsCodeAndIgnoresUnusedExtraFields() throws {
        let error = [
            "code": "not_found",
            "exit_code": 2,
            "message": "synthetic",
            "future": ["ignored": true],
        ] as [String: Any]
        let root: [String: Any] = [
            "schema_version": "1",
            "error": error,
            "future_root": "ignored",
        ]
        XCTAssertEqual(try AnarlogCLIDecoder.errorCode(
            stderr: data(root), command: "meetings get"), "not_found")
    }

    func testErrorCodeReturnsNilOnlyForNonObjectOrNonJSONStderr() throws {
        XCTAssertNil(try AnarlogCLIDecoder.errorCode(
            stderr: Data("plain diagnostic".utf8), command: "meetings get"))
        XCTAssertNil(try AnarlogCLIDecoder.errorCode(
            stderr: Data("[1,2]".utf8), command: "meetings get"))
        XCTAssertNil(try AnarlogCLIDecoder.errorCode(
            stderr: Data([0xff]), command: "meetings get"))
    }

    func testErrorCodeUsesEnvelopeSchemaAndRequiredKeys() {
        assertErrorFailure(["schema_version": "2", "error": ["code": "x"]],
                           expected: .unsupportedSchemaVersion("2"))
        assertErrorFailure(["error": ["code": "x"]],
                           expected: .missingField(command: "meetings get", field: "schema_version"))
        assertErrorFailure(["schema_version": "1"],
                           expected: .missingField(command: "meetings get", field: "error"))
        assertErrorFailure(["schema_version": "1", "error": "bad"],
                           expected: .missingField(command: "meetings get", field: "error"))
        assertErrorFailure(["schema_version": "1", "error": NSNull()],
                           expected: .missingField(command: "meetings get", field: "error"))
        assertErrorFailure(["schema_version": "1", "error": [:] as [String: Any]],
                           expected: .missingField(command: "meetings get", field: "error.code"))
        assertErrorFailure(["schema_version": "1", "error": ["code": 1]],
                           expected: .missingField(command: "meetings get", field: "error.code"))
        assertErrorFailure(["schema_version": "1", "error": ["code": NSNull()]],
                           expected: .missingField(command: "meetings get", field: "error.code"))

        assertFailure(.missingField(command: "meetings list", field: "error.code")) {
            try AnarlogCLIDecoder.errorCode(
                stderr: data(["schema_version": "1", "error": [:] as [String: Any]]),
                command: "meetings list")
        }
    }

    private func assertMissingField(command: String, field: String, change: Change,
                                    file: StaticString = #filePath, line: UInt = #line) {
        var root = command == "meetings list" ? listObject() : getObject()
        apply(change, at: field, to: &root)
        let bytes = data(root)
        assertFailure(.missingField(command: command, field: field), file: file, line: line) {
            if command == "meetings list" {
                _ = try AnarlogCLIDecoder.listPage(bytes)
            } else {
                _ = try AnarlogCLIDecoder.session(bytes)
            }
        }
    }

    private func assertEnvelopeFailure(command: String, schema: Any?,
                                       expected: AnarlogCLIFailure,
                                       file: StaticString = #filePath, line: UInt = #line) {
        var root: [String: Any] = ["data": []]
        if let schema { root["schema_version"] = schema }
        assertFailure(expected, file: file, line: line) {
            try AnarlogCLIDecoder.envelope(data(root), command: command)
        }
    }

    private func assertErrorFailure(_ object: [String: Any],
                                    expected: AnarlogCLIFailure,
                                    file: StaticString = #filePath, line: UInt = #line) {
        assertFailure(expected, file: file, line: line) {
            try AnarlogCLIDecoder.errorCode(stderr: data(object), command: "meetings get")
        }
    }

    private func assertFailure<T>(
        _ expected: AnarlogCLIFailure,
        file: StaticString = #filePath,
        line: UInt = #line,
        _ body: () throws -> T
    ) {
        do {
            _ = try body()
            XCTFail("expected \(expected)", file: file, line: line)
        } catch {
            guard let failure = error as? AnarlogCLIFailure else {
                XCTFail("unexpected error \(error)", file: file, line: line)
                return
            }
            XCTAssertEqual(failure, expected, file: file, line: line)
        }
    }

    private func apply(_ change: Change, at path: String, to root: inout [String: Any]) {
        func update(_ value: inout Any, components: ArraySlice<String>) {
            guard let component = components.first else { return }
            if component.hasSuffix("[]") {
                let key = String(component.dropLast(2))
                var values = value as! [String: Any]
                var array = values[key] as! [Any]
                var first = array[0]
                update(&first, components: components.dropFirst())
                array[0] = first
                values[key] = array
                value = values
                return
            }
            var object = value as! [String: Any]
            if components.count == 1 {
                switch change {
                case .absent:
                    object.removeValue(forKey: component)
                case .wrongType:
                    object[component] = path == "pagination.next_offset" ? 1.5 : 123
                case .null:
                    object[component] = NSNull()
                }
            } else {
                var child: Any = object[component]!
                update(&child, components: components.dropFirst())
                object[component] = child
            }
            value = object
        }
        var value: Any = root
        update(&value, components: path.split(separator: ".").map(String.init)[...])
        root = value as! [String: Any]
    }

    private func listObject() -> [String: Any] {
        [
            "schema_version": "1",
            "command": "meetings list",
            "data": [[
                "id": "AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE",
                "created_at": "2026-03-16T20:34:49.936Z",
            ]],
            "pagination": [
                "offset": 0,
                "limit": 200,
                "returned": 1,
                "total": NSNull(),
                "next_offset": 200,
            ],
        ]
    }

    private func getObject() -> [String: Any] {
        [
            "schema_version": "1",
            "command": "meetings get",
            "data": [
                "id": "AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE",
                "title": "",
                "created_at": "2026-03-16T20:34:49.936+02:00",
                "note": ["markdown": "memo"],
                "summaries": [
                    ["kind": "summary", "markdown": "first"],
                    ["kind": "template_output", "markdown": "second"],
                ],
                "participants": [[
                    "human_id": "11111111-2222-3333-4444-555555555555",
                    "display_name": "",
                    "email": NSNull(),
                    "job_title": "Engineer",
                ]],
            ],
        ]
    }

    private func data(_ object: [String: Any]) -> Data {
        try! JSONSerialization.data(withJSONObject: object, options: [.sortedKeys])
    }
}
