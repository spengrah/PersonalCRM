// Coverage for the CLI-backed orphan-notification metadata lookup.
import Foundation
import XCTest
import CRMMacCore
import CRMMacOrphanNotifications
@testable import CRMMacAnarlogSource

private struct SyntheticConfigLoadError: Error {}

final class AnarlogSessionMetadataLookupTests: XCTestCase {

    private let sessionUUID = "deadbeef-1111-2222-3333-444455556666"

    private final class StubConfigSource: AnarlogConfigSource, @unchecked Sendable {
        let cfg: AnarlogConfig?
        init(_ cfg: AnarlogConfig?) { self.cfg = cfg }
        func load() throws -> AnarlogConfig? { cfg }
    }

    private final class FailingConfigSource: AnarlogConfigSource, @unchecked Sendable {
        func load() throws -> AnarlogConfig? {
            throw SyntheticConfigLoadError()
        }
    }

    private final class LockedPathRecorder: @unchecked Sendable {
        private let lock = NSLock()
        private var values: [String?] = []

        func append(_ value: String?) {
            lock.lock()
            values.append(value)
            lock.unlock()
        }

        func read() -> [String?] {
            lock.lock()
            defer { lock.unlock() }
            return values
        }
    }

    private func makeConfig(enabled: Bool = true) -> AnarlogConfig {
        AnarlogConfig(humansEnabled: false,
                      sessionsEnabled: enabled)
    }

    private func listResponse() -> FakeAnarlogResponse {
        FakeAnarlogResponse(
            argv: FakeAnarlogCLI.listArgv(offset: 0),
            stdout: FakeAnarlogCLI.listStdout(entries: [], offset: 0, nextOffset: nil))
    }

    private func getResponse(
        title: String?,
        exit: Int32 = 0,
        stderr: String = "",
        id: String? = nil
    ) -> FakeAnarlogResponse {
        let output = exit == 0
            ? FakeAnarlogCLI.getStdout(FakeAnarlogRecord(
                id: id ?? sessionUUID,
                title: title,
                createdAt: "2026-05-27T14:00:00Z",
                noteMarkdown: nil,
                summaries: [],
                participants: []))
            : ""
        return FakeAnarlogResponse(
            argv: FakeAnarlogCLI.getArgv(id: id ?? sessionUUID),
            exit: exit,
            stdout: output,
            stderr: stderr)
    }

    private func makeFake(
        responses: [FakeAnarlogResponse]
    ) throws -> (FakeAnarlogCLI, AnarlogCLIProcessClient) {
        let fake = try FakeAnarlogCLI()
        try fake.setScenario(FakeAnarlogScenario(responses: responses))
        let client = fake.makeClient()
        return (fake, client)
    }

    func testReturnsMetadataFromCLIRecord() async throws {
        let (fake, client) = try makeFake(responses: [
            getResponse(title: "Synthetic Test Session"),
        ])
        let lookup = AnarlogSessionMetadataLookup(
            configSource: StubConfigSource(makeConfig()),
            makeCLIClient: { _ in client })
        let result = await lookup.lookup(sessionUUID: sessionUUID)
        XCTAssertEqual(result?.title, "Synthetic Test Session")
        XCTAssertEqual(result?.createdAt,
                       ISO8601DateFormatter().date(from: "2026-05-27T14:00:00Z"))
        XCTAssertNil(result?.sessionDirURL)
        XCTAssertEqual(try fake.invocations(), [FakeAnarlogCLI.getArgv(id: sessionUUID)])
    }

    func testReturnsNilWhenConfigDisabled() async throws {
        let (fake, client) = try makeFake(responses: [])
        let lookup = AnarlogSessionMetadataLookup(
            configSource: StubConfigSource(makeConfig(enabled: false)),
            makeCLIClient: { _ in client })
        let result = await lookup.lookup(sessionUUID: sessionUUID)
        XCTAssertNil(result)
        XCTAssertEqual(try fake.invocations(), [])
    }

    func testReturnsNilWhenConfigLoadThrows() async throws {
        let (fake, client) = try makeFake(responses: [])
        let lookup = AnarlogSessionMetadataLookup(
            configSource: FailingConfigSource(),
            makeCLIClient: { _ in client })
        let result = await lookup.lookup(sessionUUID: sessionUUID)
        XCTAssertNil(result)
        XCTAssertEqual(try fake.invocations(), [])
    }

    func testReturnsNilWhenConfigNil() async throws {
        let (fake, client) = try makeFake(responses: [])
        let lookup = AnarlogSessionMetadataLookup(
            configSource: StubConfigSource(nil),
            makeCLIClient: { _ in client })
        let result = await lookup.lookup(sessionUUID: sessionUUID)
        XCTAssertNil(result)
        XCTAssertEqual(try fake.invocations(), [])
    }

    func testReturnsNilWhenSessionNotFound() async throws {
        let (fake, client) = try makeFake(responses: [
            getResponse(title: nil, exit: 2,
                        stderr: FakeAnarlogCLI.errorStderr(code: "not_found", exitCode: 2)),
        ])
        let lookup = AnarlogSessionMetadataLookup(
            configSource: StubConfigSource(makeConfig()),
            makeCLIClient: { _ in client })
        let result = await lookup.lookup(sessionUUID: sessionUUID)
        XCTAssertNil(result)
        XCTAssertEqual(try fake.invocations(), [FakeAnarlogCLI.getArgv(id: sessionUUID)])
    }

    func testReturnsNilWhenCLIFails() async throws {
        let (fake, client) = try makeFake(responses: [
            getResponse(title: nil, exit: 1,
                        stderr: FakeAnarlogCLI.errorStderr(code: "internal", exitCode: 1)),
        ])
        let lookup = AnarlogSessionMetadataLookup(
            configSource: StubConfigSource(makeConfig()),
            makeCLIClient: { _ in client })
        let result = await lookup.lookup(sessionUUID: sessionUUID)
        XCTAssertNil(result)
        XCTAssertEqual(try fake.invocations(), [FakeAnarlogCLI.getArgv(id: sessionUUID)])
    }

    func testTitleEmptyOrNullMapsToNil() async throws {
        for title in ["", nil] as [String?] {
            let (fake, client) = try makeFake(responses: [getResponse(title: title)])
            let lookup = AnarlogSessionMetadataLookup(
                configSource: StubConfigSource(makeConfig()),
                makeCLIClient: { _ in client })
            let result = await lookup.lookup(sessionUUID: sessionUUID)
            XCTAssertNil(result?.title)
            XCTAssertEqual(try fake.invocations(), [FakeAnarlogCLI.getArgv(id: sessionUUID)])
        }
    }

    func testRejectsNonCanonicalUUID() async throws {
        let (fake, client) = try makeFake(responses: [])
        let lookup = AnarlogSessionMetadataLookup(
            configSource: StubConfigSource(makeConfig()),
            makeCLIClient: { _ in client })
        let result = await lookup.lookup(sessionUUID: "not-a-uuid")
        XCTAssertNil(result)
        XCTAssertEqual(try fake.invocations(), [])
    }

    func testUppercaseUUIDIsLowercasedInArgv() async throws {
        let (fake, client) = try makeFake(responses: [getResponse(title: "title")])
        let lookup = AnarlogSessionMetadataLookup(
            configSource: StubConfigSource(makeConfig()),
            makeCLIClient: { _ in client })
        _ = await lookup.lookup(sessionUUID: sessionUUID.uppercased())
        XCTAssertEqual(try fake.invocations(), [FakeAnarlogCLI.getArgv(id: sessionUUID)])
    }

    func testFactoryReceivesConfiguredCLIPath() async throws {
        var config = makeConfig()
        try config.setCLIPath("/synthetic/anarlog")
        let recorder = LockedPathRecorder()
        let (fake, client) = try makeFake(responses: [getResponse(title: "title")])
        let lookup = AnarlogSessionMetadataLookup(
            configSource: StubConfigSource(config),
            makeCLIClient: { path in
                recorder.append(path)
                return client
            })
        _ = await lookup.lookup(sessionUUID: sessionUUID)
        XCTAssertEqual(recorder.read(), ["/synthetic/anarlog"])
        XCTAssertEqual(try fake.invocations(), [FakeAnarlogCLI.getArgv(id: sessionUUID)])
    }

    func testDefaultFactoryRunsConfiguredCLIPath() async throws {
        let fake = try FakeAnarlogCLI()
        let script = fake.directory.appendingPathComponent("argv-recorder")
        let log = fake.directory.appendingPathComponent("argv.log")
        let scriptText = """
        #!/bin/sh
        printf '%s\\n' "$@" >> "\(log.path)"
        exit 1
        """
        try Data(scriptText.utf8).write(to: script)
        try FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: script.path)
        var config = makeConfig()
        try config.setCLIPath(script.path)
        let lookup = AnarlogSessionMetadataLookup(configSource: StubConfigSource(config))
        let result = await lookup.lookup(sessionUUID: sessionUUID)
        XCTAssertNil(result)
        let lines = try String(contentsOf: log, encoding: .utf8)
            .split(whereSeparator: \.isNewline).map(String.init)
        XCTAssertEqual(lines, FakeAnarlogCLI.getArgv(id: sessionUUID))
    }
}

final class AnarlogUUIDValidatorTests: XCTestCase {
    func testUUIDValidatorAcceptsLowercaseCanonical() {
        XCTAssertEqual(
            AnarlogUUIDValidator.canonicalize("0aaaaaaa-0000-4000-8000-00000000000b"),
            "0aaaaaaa-0000-4000-8000-00000000000b")
    }

    func testUUIDValidatorRejectsUppercase() {
        // Mixed-case UUIDs are rejected to keep cursor keys canonical.
        XCTAssertNil(
            AnarlogUUIDValidator.canonicalize("0AAAAAAA-0000-4000-8000-00000000000B"))
        XCTAssertNil(
            AnarlogUUIDValidator.canonicalize("0aaaaaaa-0000-4000-8000-00000000000B"),
            "mixed-case must also be rejected")
    }

    func testUUIDValidatorRejectsMalformed() {
        XCTAssertNil(AnarlogUUIDValidator.canonicalize(""))
        XCTAssertNil(AnarlogUUIDValidator.canonicalize("not-a-uuid"))
        XCTAssertNil(AnarlogUUIDValidator.canonicalize("0aaaaaaa-0000-4000-8000-00000000000"))
    }
}
