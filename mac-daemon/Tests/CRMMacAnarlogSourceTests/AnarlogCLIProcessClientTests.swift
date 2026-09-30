import Foundation
import Darwin
import CRMMacCore
import XCTest
@testable import CRMMacAnarlogSource

final class AnarlogCLIProcessClientTests: XCTestCase {

    func testOnePageListingUsesExactArgv() async throws {
        let fake = try FakeAnarlogCLI()
        let entries = [FakeAnarlogListEntry(id: "session-a", createdAt: "2026-03-16T20:34:49Z")]
        try fake.setScenario(FakeAnarlogScenario(responses: [
            response(FakeAnarlogCLI.listArgv(offset: 0),
                     stdout: FakeAnarlogCLI.listStdout(entries: entries, offset: 0, nextOffset: nil)),
        ]))

        let result = try await fake.makeClient().listSessions()
        XCTAssertEqual(result.map(\.id), ["session-a"])
        XCTAssertEqual(try fake.invocations(), [FakeAnarlogCLI.listArgv(offset: 0)])
    }

    func testListingReadsEveryPageInOrder() async throws {
        let fake = try FakeAnarlogCLI()
        let first = (0..<200).map { listEntry($0) }
        let second = (200..<400).map { listEntry($0) }
        let third = (400..<405).map { listEntry($0) }
        try fake.setScenario(FakeAnarlogScenario(responses: [
            response(FakeAnarlogCLI.listArgv(offset: 0),
                     stdout: FakeAnarlogCLI.listStdout(entries: first, offset: 0, nextOffset: 200)),
            response(FakeAnarlogCLI.listArgv(offset: 200),
                     stdout: FakeAnarlogCLI.listStdout(entries: second, offset: 200, nextOffset: 400)),
            response(FakeAnarlogCLI.listArgv(offset: 400),
                     stdout: FakeAnarlogCLI.listStdout(entries: third, offset: 400, nextOffset: nil)),
        ]))

        let result = try await fake.makeClient().listSessions()
        XCTAssertEqual(result.count, 405)
        XCTAssertEqual(result.map(\.id), (0..<405).map { "session-\($0)" })
        XCTAssertEqual(try fake.invocations(), [
            FakeAnarlogCLI.listArgv(offset: 0),
            FakeAnarlogCLI.listArgv(offset: 200),
            FakeAnarlogCLI.listArgv(offset: 400),
        ])
    }

    func testEmptyListingReturnsEmptyArray() async throws {
        let fake = try FakeAnarlogCLI()
        try fake.setScenario(FakeAnarlogScenario(responses: [
            response(FakeAnarlogCLI.listArgv(offset: 0),
                     stdout: FakeAnarlogCLI.listStdout(entries: [], offset: 0, nextOffset: nil)),
        ]))
        let result = try await fake.makeClient().listSessions()
        XCTAssertEqual(result, [])
    }

    func testFailureOnLaterPageThrowsWithoutReturningPartialListing() async throws {
        let fake = try FakeAnarlogCLI()
        try fake.setScenario(FakeAnarlogScenario(responses: [
            response(FakeAnarlogCLI.listArgv(offset: 0),
                     stdout: FakeAnarlogCLI.listStdout(
                        entries: [listEntry(0)], offset: 0, nextOffset: 200)),
            response(FakeAnarlogCLI.listArgv(offset: 200), exit: 1, stderr: "usage failure"),
        ]))

        await assertFailure(.nonZeroExit(code: 1, errorCode: nil)) {
            try await fake.makeClient().listSessions()
        }
        XCTAssertEqual(try fake.invocations(), [
            FakeAnarlogCLI.listArgv(offset: 0),
            FakeAnarlogCLI.listArgv(offset: 200),
        ])
    }

    func testNonAdvancingNextOffsetIsMalformedOutput() async throws {
        let fake = try FakeAnarlogCLI()
        try fake.setScenario(FakeAnarlogScenario(responses: [
            response(FakeAnarlogCLI.listArgv(offset: 0),
                     stdout: FakeAnarlogCLI.listStdout(
                        entries: [listEntry(0)], offset: 0, nextOffset: 0)),
        ]))
        await assertFailure(.malformedOutput(command: "meetings list")) {
            try await fake.makeClient().listSessions()
        }
    }

    func testRepeatedIDAcrossPagesKeepsFirstOccurrenceOnly() async throws {
        let fake = try FakeAnarlogCLI()
        let repeated = FakeAnarlogListEntry(id: "same-id", createdAt: "2026-03-16T20:34:49Z")
        try fake.setScenario(FakeAnarlogScenario(responses: [
            response(FakeAnarlogCLI.listArgv(offset: 0),
                     stdout: FakeAnarlogCLI.listStdout(
                        entries: [repeated], offset: 0, nextOffset: 200)),
            response(FakeAnarlogCLI.listArgv(offset: 200),
                     stdout: FakeAnarlogCLI.listStdout(
                        entries: [repeated, listEntry(1)], offset: 200, nextOffset: nil)),
        ]))

        let result = try await fake.makeClient().listSessions()
        XCTAssertEqual(result.map(\.id), ["same-id", "session-1"])
    }

    func testGetSessionReturnsDecodedRecordAndExactArgv() async throws {
        let fake = try FakeAnarlogCLI()
        let record = FakeAnarlogRecord(
            id: "session-get",
            title: "Synthetic meeting",
            createdAt: "2026-03-16T20:34:49Z",
            noteMarkdown: "memo",
            summaries: ["summary"],
            participants: [
                FakeAnarlogParticipant(
                    humanID: "11111111-2222-3333-4444-555555555555",
                    displayName: "Participant",
                    email: "participant@example.invalid",
                    jobTitle: "Engineer"),
            ])
        try fake.setScenario(FakeAnarlogScenario(responses: [
            response(FakeAnarlogCLI.getArgv(id: "session-get"),
                     stdout: FakeAnarlogCLI.getStdout(record)),
        ]))

        let optionalResult = try await fake.makeClient().getSession(id: "session-get")
        let result = try XCTUnwrap(optionalResult)
        XCTAssertEqual(result.id, "session-get")
        XCTAssertEqual(result.title, "Synthetic meeting")
        XCTAssertEqual(result.memo, "memo")
        XCTAssertEqual(result.summaries, ["summary"])
        XCTAssertEqual(result.participants.map(\.personID), ["11111111-2222-3333-4444-555555555555"])
        XCTAssertEqual(try fake.invocations(), [FakeAnarlogCLI.getArgv(id: "session-get")])
    }

    func testNotFoundEnvelopeReturnsNilOnlyForGet() async throws {
        let fake = try FakeAnarlogCLI()
        try fake.setScenario(FakeAnarlogScenario(responses: [
            response(FakeAnarlogCLI.getArgv(id: "missing-session"), exit: 2,
                     stderr: FakeAnarlogCLI.errorStderr(code: "not_found", exitCode: 2)),
        ]))
        let result = try await fake.makeClient().getSession(id: "missing-session")
        XCTAssertNil(result)
    }

    func testExitTwoWithPlainTextIsRuntimeFailure() async throws {
        let fake = try FakeAnarlogCLI()
        try fake.setScenario(FakeAnarlogScenario(responses: [
            response(FakeAnarlogCLI.getArgv(id: "session-a"), exit: 2,
                     stderr: "usage error"),
        ]))
        await assertFailure(.nonZeroExit(code: 2, errorCode: nil)) {
            try await fake.makeClient().getSession(id: "session-a")
        }
    }

    func testDatabaseNotFoundEnvelopeFailsBothOperations() async throws {
        let fake = try FakeAnarlogCLI()
        try fake.setScenario(FakeAnarlogScenario(responses: [
            response(FakeAnarlogCLI.listArgv(offset: 0), exit: 3,
                     stderr: FakeAnarlogCLI.errorStderr(code: "database_not_found", exitCode: 3)),
            response(FakeAnarlogCLI.getArgv(id: "session-a"), exit: 3,
                     stderr: FakeAnarlogCLI.errorStderr(code: "database_not_found", exitCode: 3)),
        ]))

        await assertFailure(.databaseNotFound) {
            try await fake.makeClient().listSessions()
        }
        await assertFailure(.databaseNotFound) {
            try await fake.makeClient().getSession(id: "session-a")
        }
    }

    func testExitThreeWithPlainTextIsRuntimeFailure() async throws {
        let fake = try FakeAnarlogCLI()
        try fake.setScenario(FakeAnarlogScenario(responses: [
            response(FakeAnarlogCLI.listArgv(offset: 0), exit: 3, stderr: "database missing"),
        ]))
        await assertFailure(.nonZeroExit(code: 3, errorCode: nil)) {
            try await fake.makeClient().listSessions()
        }
    }

    func testOtherNonzeroExitPreservesErrorCode() async throws {
        let fake = try FakeAnarlogCLI()
        try fake.setScenario(FakeAnarlogScenario(responses: [
            response(FakeAnarlogCLI.listArgv(offset: 0), exit: 5,
                     stderr: FakeAnarlogCLI.errorStderr(code: "internal", exitCode: 5)),
        ]))
        await assertFailure(.nonZeroExit(code: 5, errorCode: "internal")) {
            try await fake.makeClient().listSessions()
        }
    }

    func testUnsupportedErrorEnvelopeSchemaFails() async throws {
        let fake = try FakeAnarlogCLI()
        try fake.setScenario(FakeAnarlogScenario(responses: [
            response(FakeAnarlogCLI.listArgv(offset: 0), exit: 1,
                     stderr: #"{"schema_version":"2","error":{"code":"internal"}}"#),
        ]))
        await assertFailure(.unsupportedSchemaVersion("2")) {
            try await fake.makeClient().listSessions()
        }
    }

    func testMissingErrorCodeFailsListingAndGetInsteadOfBecomingNil() async throws {
        let fake = try FakeAnarlogCLI()
        try fake.setScenario(FakeAnarlogScenario(responses: [
            response(FakeAnarlogCLI.listArgv(offset: 0), exit: 1,
                     stderr: #"{"schema_version":"1","error":{"message":"synthetic"}}"#),
            response(FakeAnarlogCLI.getArgv(id: "session-a"), exit: 2,
                     stderr: #"{"schema_version":"1","error":{"message":"synthetic"}}"#),
        ]))

        await assertFailure(.missingField(command: "meetings list", field: "error.code")) {
            try await fake.makeClient().listSessions()
        }
        await assertFailure(.missingField(command: "meetings get", field: "error.code")) {
            try await fake.makeClient().getSession(id: "session-a")
        }
    }

    func testTimeoutTerminatesAndReapsChildPromptly() async throws {
        let fake = try FakeAnarlogCLI()
        try fake.setScenario(FakeAnarlogScenario(responses: [
            FakeAnarlogResponse(
                argv: FakeAnarlogCLI.listArgv(offset: 0),
                stdout: FakeAnarlogCLI.listStdout(entries: [], offset: 0, nextOffset: nil),
                delayMs: 10_000),
        ]))
        let start = Date()
        await assertFailure(.timeout) {
            try await fake.makeClient(timeout: 0.5).listSessions()
        }
        XCTAssertLessThan(Date().timeIntervalSince(start), 5)
    }

    func testTimeoutIsBoundedWhenChildIgnoresTermAndDescendantHoldsPipes() async throws {
        let fake = try FakeAnarlogCLI()
        let childPIDURL = fake.directory.appendingPathComponent("child.pid")
        let holderPIDURL = fake.directory.appendingPathComponent("holder.pid")
        let scriptURL = fake.directory.appendingPathComponent("ignore-term-and-hold-pipes")
        let script = """
            #!/bin/sh
            trap '' TERM
            echo $$ > "\(childPIDURL.path)"
            sleep 20 &
            echo $! > "\(holderPIDURL.path)"
            wait
            """
        try Data(script.utf8).write(to: scriptURL)
        try FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: scriptURL.path)
        defer {
            if let holderPID = try? String(contentsOf: holderPIDURL, encoding: .utf8),
               let pid = Int32(holderPID.trimmingCharacters(in: .whitespacesAndNewlines)) {
                _ = kill(pid, SIGKILL)
            }
        }
        let client = AnarlogCLIProcessClient(
            cliPath: scriptURL.path,
            homeDirectory: fake.directory,
            environment: fake.environment,
            timeout: 0.5)

        let start = Date()
        await assertFailure(.timeout) {
            try await client.listSessions()
        }
        XCTAssertLessThan(Date().timeIntervalSince(start), 5)

        let childPID = try XCTUnwrap(Int32(
            String(contentsOf: childPIDURL, encoding: .utf8)
                .trimmingCharacters(in: .whitespacesAndNewlines)))
        XCTAssertEqual(kill(childPID, 0), -1)
        XCTAssertEqual(errno, ESRCH)
    }

    func testTimeoutIsBoundedWhenChildExitsButDescendantHoldsStdout() async throws {
        let fake = try FakeAnarlogCLI()
        let holderPIDURL = fake.directory.appendingPathComponent("holder.pid")
        let scriptURL = fake.directory.appendingPathComponent("exit-with-held-stdout")
        let script = """
            #!/bin/sh
            sleep 20 &
            echo $! > "\(holderPIDURL.path)"
            exit 0
            """
        try Data(script.utf8).write(to: scriptURL)
        try FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: scriptURL.path)
        defer {
            if let holderPID = try? String(contentsOf: holderPIDURL, encoding: .utf8),
               let pid = Int32(holderPID.trimmingCharacters(in: .whitespacesAndNewlines)) {
                _ = kill(pid, SIGKILL)
            }
        }
        let client = AnarlogCLIProcessClient(
            cliPath: scriptURL.path,
            homeDirectory: fake.directory,
            environment: fake.environment,
            timeout: 0.5)

        let start = Date()
        await assertFailure(.timeout) {
            try await client.listSessions()
        }
        XCTAssertLessThan(Date().timeIntervalSince(start), 5)
    }

    func testLargeStdoutIsDrainedWhileChildRuns() async throws {
        let fake = try FakeAnarlogCLI()
        var object = try JSONSerialization.jsonObject(
            with: Data(FakeAnarlogCLI.listStdout(
                entries: [listEntry(0)], offset: 0, nextOffset: nil).utf8)) as! [String: Any]
        object["padding"] = String(repeating: "x", count: 256 * 1024)
        let stdout = String(data: try JSONSerialization.data(withJSONObject: object), encoding: .utf8)!
        XCTAssertGreaterThanOrEqual(stdout.utf8.count, 256 * 1024)
        try fake.setScenario(FakeAnarlogScenario(responses: [
            response(FakeAnarlogCLI.listArgv(offset: 0), stdout: stdout),
        ]))

        let result = try await fake.makeClient(timeout: 5).listSessions()
        XCTAssertEqual(result.map(\.id), ["session-0"])
    }

    func testUnresolvableExecutablesThrowBinaryNotFound() async throws {
        let fake = try FakeAnarlogCLI()
        let missing = fake.directory.appendingPathComponent("missing-anarlog").path
        let client = AnarlogCLIProcessClient(
            cliPath: missing, homeDirectory: fake.directory, environment: fake.environment)
        await assertFailure(.binaryNotFound) {
            try await client.listSessions()
        }

        let notExecutable = fake.directory.appendingPathComponent("not-executable").path
        try Data("synthetic".utf8).write(to: URL(fileURLWithPath: notExecutable))
        let nonExecutableClient = AnarlogCLIProcessClient(
            cliPath: notExecutable, homeDirectory: fake.directory, environment: fake.environment)
        await assertFailure(.binaryNotFound) {
            try await nonExecutableClient.listSessions()
        }

        let noDefault = AnarlogCLIProcessClient(
            cliPath: nil, homeDirectory: fake.directory, environment: fake.environment)
        await assertFailure(.binaryNotFound) {
            try await noDefault.listSessions()
        }
    }

    func testDefaultCandidateSymlinkResolvesAndExplicitPathHasNoFallback() async throws {
        let fake = try FakeAnarlogCLI()
        let defaultCandidate = URL(fileURLWithPath:
            AnarlogCLIExecutableResolver.defaultCandidatePaths(homeDirectory: fake.directory)[0])
        try FileManager.default.createDirectory(
            at: defaultCandidate.deletingLastPathComponent(),
            withIntermediateDirectories: true)
        try FileManager.default.createSymbolicLink(
            at: defaultCandidate, withDestinationURL: URL(fileURLWithPath: FakeAnarlogCLI.executablePath))
        try fake.setScenario(FakeAnarlogScenario(responses: [
            response(FakeAnarlogCLI.listArgv(offset: 0),
                     stdout: FakeAnarlogCLI.listStdout(entries: [], offset: 0, nextOffset: nil)),
        ]))
        let defaultClient = AnarlogCLIProcessClient(
            cliPath: nil, homeDirectory: fake.directory, environment: fake.environment)
        let result = try await defaultClient.listSessions()
        XCTAssertEqual(result, [])

        let missingExplicitPath = fake.directory.appendingPathComponent("missing-explicit").path
        let explicitClient = AnarlogCLIProcessClient(
            cliPath: missingExplicitPath, homeDirectory: fake.directory, environment: fake.environment)
        await assertFailure(.binaryNotFound) {
            try await explicitClient.listSessions()
        }
    }

    private func listEntry(_ index: Int) -> FakeAnarlogListEntry {
        FakeAnarlogListEntry(
            id: "session-\(index)",
            createdAt: "2026-03-16T20:34:49.936Z")
    }

    private func response(_ argv: [String], exit: Int32 = 0,
                          stdout: String = "", stderr: String = "") -> FakeAnarlogResponse {
        FakeAnarlogResponse(argv: argv, exit: exit, stdout: stdout, stderr: stderr)
    }

    private func assertFailure<T>(
        _ expected: AnarlogCLIFailure,
        _ body: () async throws -> T,
        file: StaticString = #filePath,
        line: UInt = #line
    ) async {
        do {
            _ = try await body()
            XCTFail("expected \(expected)", file: file, line: line)
        } catch {
            guard let failure = error as? AnarlogCLIFailure else {
                XCTFail("unexpected error \(error)", file: file, line: line)
                return
            }
            XCTAssertEqual(failure, expected, file: file, line: line)
        }
    }
}
