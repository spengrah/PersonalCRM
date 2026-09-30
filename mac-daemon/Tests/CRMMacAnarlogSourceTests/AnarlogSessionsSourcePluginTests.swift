// Contract tests for the CLI-backed sessions source.
import Foundation
import XCTest
import CRMMacCore
import CRMMacPiClient
@testable import CRMMacAnarlogSource

final class AnarlogSessionsSourcePluginTests: XCTestCase {

    private let testAuth = PiAuth(
        hostID: UUID(uuidString: "11111111-2222-3333-4444-555555555555")!,
        apiKey: "k")
    private let operatorID = "99999999-9999-9999-9999-999999999999"
    private let now = ISO8601DateFormatter().date(from: "2026-03-20T12:00:00Z")!

    private enum PiEndpoint: Hashable {
        case cursorGet
        case knownIDs
        case ingest
        case cursorCommit
    }

    private struct PiScript {
        var cursor = ""
        var knownIDs = KnownIDsData(ids: [])
        var failing: Set<PiEndpoint> = []
    }

    private final class MockTransport: @unchecked Sendable {
        private let lock = NSLock()
        private let initialCursor: String
        private let knownIDs: KnownIDsData
        private var failures: Set<PiEndpoint>
        private var savedCursor: String?
        private var bodies: [Data] = []
        private var requests = 0
        private var known = false
        private var ingestCount = 0
        private var commitAttempt = false

        init(_ script: PiScript) {
            initialCursor = script.cursor
            knownIDs = script.knownIDs
            failures = script.failing
        }

        func setFailing(_ value: Set<PiEndpoint>) {
            lock.lock(); defer { lock.unlock() }
            failures = value
        }

        var requestCount: Int { lock.lock(); defer { lock.unlock() }; return requests }
        var knownIDsCalled: Bool { lock.lock(); defer { lock.unlock() }; return known }
        var ingestCallCount: Int { lock.lock(); defer { lock.unlock() }; return ingestCount }
        var ingestBodies: [Data] { lock.lock(); defer { lock.unlock() }; return bodies }
        var commitWasAttempted: Bool { lock.lock(); defer { lock.unlock() }; return commitAttempt }
        var committedCursor: String? { lock.lock(); defer { lock.unlock() }; return savedCursor }

        func asFunc() -> TransportFunc {
            { [self] request in try respond(to: request) }
        }

        private func respond(to request: URLRequest) throws -> (Data, HTTPURLResponse) {
            let path = request.url?.path ?? ""
            let method = request.httpMethod ?? "GET"
            let endpoint: PiEndpoint
            if path.hasSuffix("/cursor") && method == "GET" {
                endpoint = .cursorGet
            } else if path.hasSuffix("/known-ids") && method == "GET" {
                endpoint = .knownIDs
            } else if path.hasSuffix("/ingest/events") && method == "POST" {
                endpoint = .ingest
            } else if path.hasSuffix("/cursor") && method == "POST" {
                endpoint = .cursorCommit
            } else {
                throw URLError(.unsupportedURL)
            }

            lock.lock()
            requests += 1
            let shouldFail = failures.contains(endpoint)
            if endpoint == .knownIDs { known = true }
            if endpoint == .ingest {
                ingestCount += 1
                if let body = request.httpBody { bodies.append(body) }
            }
            if endpoint == .cursorCommit { commitAttempt = true }
            if endpoint == .cursorCommit, !shouldFail, let body = request.httpBody,
               let parsed = try? JSONSerialization.jsonObject(with: body) as? [String: Any] {
                savedCursor = parsed["cursor"] as? String
            }
            let cursor = savedCursor ?? initialCursor
            lock.unlock()

            if shouldFail {
                let failure = Data(#"{"success":false,"error":{"code":"SYNTHETIC","message":"synthetic"}}"#.utf8)
                return (failure, Self.response(request, status: 400))
            }
            switch endpoint {
            case .cursorGet:
                return (Self.cursorResponse(cursor), Self.response(request))
            case .knownIDs:
                return (Self.knownIDsResponse(knownIDs), Self.response(request))
            case .ingest:
                return (Self.ingestResponse, Self.response(request))
            case .cursorCommit:
                return (Data(#"{"success":true,"data":{"ok":true}}"#.utf8), Self.response(request))
            }
        }

        private static let ingestResponse = Data(
            #"{"accepted":100,"duplicate":0,"rejected":0,"errors":[],"needs_attention":[]}"#.utf8)

        private static func cursorResponse(_ cursor: String) -> Data {
            let json: [String: Any] = [
                "success": true,
                "data": ["cursor": cursor, "cursor_epoch": 0, "backfill_complete": false],
            ]
            return try! JSONSerialization.data(withJSONObject: json)
        }

        private static func knownIDsResponse(_ value: KnownIDsData) -> Data {
            let ids: [[String: Any]] = value.ids.map { entry in
                ["source_id": entry.sourceID,
                 "last_content_hash": entry.lastContentHash as Any? ?? NSNull()]
            }
            return try! JSONSerialization.data(withJSONObject: [
                "success": true, "data": ["ids": ids],
            ])
        }

        private static func response(_ request: URLRequest, status: Int = 200) -> HTTPURLResponse {
            HTTPURLResponse(url: request.url!, statusCode: status,
                            httpVersion: "HTTP/1.1",
                            headerFields: ["Content-Type": "application/json"])!
        }
    }

    private final class MutableConfigSource: AnarlogConfigSource, @unchecked Sendable {
        private let lock = NSLock()
        private var value: AnarlogConfig?
        init(_ value: AnarlogConfig?) { self.value = value }
        func load() throws -> AnarlogConfig? {
            lock.lock(); defer { lock.unlock() }
            return value
        }
        func setDeletionCap(_ cap: Int) throws {
            lock.lock(); defer { lock.unlock() }
            try value?.setDeletionCap(cap)
        }
    }

    private final class FailingConfigSource: AnarlogConfigSource, @unchecked Sendable {
        func load() throws -> AnarlogConfig? { throw AnarlogFilesystemError.ioError("synthetic") }
    }

    private final class LockedPathRecorder: @unchecked Sendable {
        private let lock = NSLock()
        private var paths: [String?] = []
        func append(_ path: String?) { lock.lock(); paths.append(path); lock.unlock() }
        func read() -> [String?] { lock.lock(); defer { lock.unlock() }; return paths }
    }

    private actor RecordingHealthSink: AnarlogHealthSink {
        private var values: [(SourceID, AnarlogTickOutcome)] = []
        func record(source: SourceID, outcome: AnarlogTickOutcome) async {
            values.append((source, outcome))
        }
        func recorded() -> [(SourceID, AnarlogTickOutcome)] { values }
    }

    private struct Rig {
        let plugin: AnarlogSessionsSourcePlugin
        let fake: FakeAnarlogCLI
        let transport: MockTransport
        let mutator: StateMutator
        let configSource: MutableConfigSource
        let healthSink: RecordingHealthSink
    }

    private func config(
        enabled: Bool = true,
        operatorSet: Bool = true,
        deletionCap: Int = 5,
        cliPath: String? = nil
    ) throws -> AnarlogConfig {
        var result = AnarlogConfig(
            rootPath: "/tmp/anarlog-test", humansEnabled: false, sessionsEnabled: enabled)
        if operatorSet { try result.setOperatorPersonID(operatorID) }
        if let cliPath { try result.setCLIPath(cliPath) }
        try result.setDeletionCap(deletionCap)
        return result
    }

    private func makeRig(
        responses: [FakeAnarlogResponse] = [],
        config suppliedConfig: AnarlogConfig? = nil,
        piScript: PiScript = PiScript(),
        defaultFactory: Bool = false,
        factoryRecorder: LockedPathRecorder? = nil,
        delayMsForFake: Int = 0
    ) throws -> Rig {
        let fake = try FakeAnarlogCLI()
        var adjusted = responses
        if delayMsForFake > 0, !adjusted.isEmpty {
            adjusted[0].delayMs = delayMsForFake
        }
        try fake.setScenario(FakeAnarlogScenario(responses: adjusted))
        let client = fake.makeClient()
        let configSource = MutableConfigSource(try suppliedConfig ?? config())
        let transport = MockTransport(piScript)
        let piClient = PiClient(
            baseURL: URL(string: "https://test.invalid")!,
            transport: transport.asFunc(), logger: NoopLogger())
        let stateURL = URL(fileURLWithPath: NSTemporaryDirectory())
            .appendingPathComponent("anarlog-sessions-state-\(UUID().uuidString).json")
        let stateStore = StateStore(fileURL: stateURL)
        try stateStore.initializeIfMissing()
        let mutator = StateMutator(store: stateStore)
        let publisher = AnarlogSessionsPublisher(
            sender: { auth, body in try await piClient.ingestEvents(auth: auth, body: body) },
            auth: testAuth, logger: NoopLogger())
        let healthSink = RecordingHealthSink()
        let healthRegistry = SourceHealthRegistry()
        let tickNow = now
        let plugin: AnarlogSessionsSourcePlugin
        if defaultFactory {
            plugin = AnarlogSessionsSourcePlugin(
                piClient: piClient, auth: testAuth, mutator: mutator,
                publisher: publisher, configSource: configSource,
                healthSink: healthSink, healthRegistry: healthRegistry,
                logger: NoopLogger(), clock: { tickNow })
        } else if let factoryRecorder {
            plugin = AnarlogSessionsSourcePlugin(
                piClient: piClient, auth: testAuth, mutator: mutator,
                publisher: publisher, configSource: configSource,
                makeCLIClient: { path in factoryRecorder.append(path); return client },
                healthSink: healthSink, healthRegistry: healthRegistry,
                logger: NoopLogger(), clock: { tickNow })
        } else {
            plugin = AnarlogSessionsSourcePlugin(
                piClient: piClient, auth: testAuth, mutator: mutator,
                publisher: publisher, configSource: configSource,
                makeCLIClient: { _ in client },
                healthSink: healthSink, healthRegistry: healthRegistry,
                logger: NoopLogger(), clock: { tickNow })
        }
        return Rig(plugin: plugin, fake: fake, transport: transport,
                   mutator: mutator, configSource: configSource, healthSink: healthSink)
    }

    private func listEntry(_ id: String, _ createdAt: String = "2026-03-16T20:34:49Z") -> FakeAnarlogListEntry {
        FakeAnarlogListEntry(id: id, createdAt: createdAt)
    }

    private func record(
        _ id: String,
        title: String? = "Synthetic Session",
        createdAt: String = "2026-03-16T20:34:49Z",
        memo: String? = nil,
        summaries: [String] = [],
        participants: [FakeAnarlogParticipant] = []
    ) -> FakeAnarlogRecord {
        FakeAnarlogRecord(id: id, title: title, createdAt: createdAt,
                          noteMarkdown: memo, summaries: summaries,
                          participants: participants)
    }

    private func listResponse(_ entries: [FakeAnarlogListEntry]) -> FakeAnarlogResponse {
        FakeAnarlogResponse(
            argv: FakeAnarlogCLI.listArgv(offset: 0),
            stdout: FakeAnarlogCLI.listStdout(entries: entries, offset: 0, nextOffset: nil))
    }

    private func getResponse(_ value: FakeAnarlogRecord, delayMs: Int = 0) -> FakeAnarlogResponse {
        FakeAnarlogResponse(argv: FakeAnarlogCLI.getArgv(id: value.id),
                            stdout: FakeAnarlogCLI.getStdout(value), delayMs: delayMs)
    }

    private func getNotFound(_ id: String) -> FakeAnarlogResponse {
        FakeAnarlogResponse(
            argv: FakeAnarlogCLI.getArgv(id: id), exit: 2,
            stderr: FakeAnarlogCLI.errorStderr(code: "not_found", exitCode: 2))
    }

    private func listFailure(code: String, exit: Int32) -> FakeAnarlogResponse {
        FakeAnarlogResponse(argv: FakeAnarlogCLI.listArgv(offset: 0), exit: exit,
                            stderr: FakeAnarlogCLI.errorStderr(code: code, exitCode: exit))
    }

    private func events(_ rig: Rig) throws -> [[String: Any]] {
        try rig.transport.ingestBodies.flatMap { body in
            let parsed = try JSONSerialization.jsonObject(with: body) as! [String: Any]
            return parsed["events"] as! [[String: Any]]
        }
    }

    private func eventPayload(_ event: [String: Any]) -> [String: Any] {
        event["payload"] as! [String: Any]
    }

    private func priorCursor(_ entries: [String: String]) throws -> String {
        try AnarlogSessionsCursorCodec.encode(
            entries.mapValues(AnarlogSessionsCursorEntry.init(recordHash:)))
    }

    private func health(_ rig: Rig) async -> [(SourceID, AnarlogTickOutcome)] {
        await rig.healthSink.recorded()
    }

    // MARK: CLI read and payload projection

    func testSessionWithSummaryEmits() async throws {
        let id = "0a631ec3-fa11-47d2-aa0f-17b320866c87"
        let rig = try makeRig(responses: [listResponse([listEntry(id)]),
                                         getResponse(record(id, summaries: ["summary body"]))])
        try await rig.plugin.tick()
        let all = try events(rig)
        XCTAssertEqual(all.count, 1)
        XCTAssertEqual(all[0]["kind"] as? String, "meeting_note.recorded")
        XCTAssertEqual(eventPayload(all[0])["summary"] as? String, "summary body")
    }

    func testSessionWithAllFieldsEmits() async throws {
        let id = "0a631ec3-fa11-47d2-aa0f-17b320866c88"
        let participant = FakeAnarlogParticipant(
            humanID: "11111111-1111-1111-1111-111111111111",
            displayName: nil, email: nil, jobTitle: nil)
        let rig = try makeRig(responses: [
            listResponse([listEntry(id)]),
            getResponse(record(id, title: "Title", memo: "memo", summaries: ["summary"],
                               participants: [participant])),
        ])
        try await rig.plugin.tick()
        let payload = eventPayload(try XCTUnwrap(events(rig).first))
        XCTAssertEqual(payload["title"] as? String, "Title")
        XCTAssertEqual(payload["summary"] as? String, "summary")
        XCTAssertEqual(payload["memo"] as? String, "memo")
        XCTAssertEqual(payload["participant_ids"] as? [String], [participant.humanID])
    }

    func testPreFloorSessionIsNeverSent() async throws {
        let id = "0a631ec3-fa11-47d2-aa0f-17b320866c89"
        let rig = try makeRig(responses: [listResponse([listEntry(id, "2025-12-15T10:00:00Z")])])
        try await rig.plugin.tick()
        XCTAssertEqual(rig.transport.ingestBodies.count, 0)
        XCTAssertEqual(try rig.fake.invocations(), [FakeAnarlogCLI.listArgv(offset: 0)])
        XCTAssertEqual(AnarlogSessionsCursorCodec.decodeOrNil(rig.transport.committedCursor ?? ""), [:])
    }

    func testOversizedSessionPayloadPreservesPriorEntryAndEmitsNoDelete() async throws {
        let id = "0a631ec3-fa11-47d2-aa0f-17b320866c90"
        let old = try priorCursor([id: "prevhash"])
        var script = PiScript(cursor: old)
        script.knownIDs = KnownIDsData(ids: [])
        let bigMemo = String(repeating: "X", count: CRMMacAnarlogSource.maxPayloadBytes + 1024)
        let rig = try makeRig(responses: [listResponse([listEntry(id)]),
                                         getResponse(record(id, memo: bigMemo))], piScript: script)
        try await rig.plugin.tick()
        for event in try events(rig) {
            XCTAssertNotEqual(event["kind"] as? String, "meeting_note.deleted",
                              "P0 violation: oversized session produced a delete")
        }
        let decoded = try XCTUnwrap(AnarlogSessionsCursorCodec.decodeOrNil(
            try XCTUnwrap(rig.transport.committedCursor)))
        XCTAssertEqual(decoded[id]?.recordHash, "prevhash")
    }

    func testSettleBoundary() async throws {
        let at = "0a631ec3-fa11-47d2-aa0f-17b320866c91"
        let after = "0a631ec3-fa11-47d2-aa0f-17b320866c92"
        let rig = try makeRig(responses: [
            listResponse([listEntry(at, "2026-03-20T09:00:00Z"), listEntry(after, "2026-03-20T09:00:01Z")]),
            getResponse(record(at, createdAt: "2026-03-20T09:00:00Z")),
        ])
        try await rig.plugin.tick()
        XCTAssertEqual(try events(rig).count, 1)
        XCTAssertEqual(try rig.fake.invocations(), [FakeAnarlogCLI.listArgv(offset: 0),
                                                     FakeAnarlogCLI.getArgv(id: at)])
    }

    func testFloorBoundary() async throws {
        let at = "0a631ec3-fa11-47d2-aa0f-17b320866c93"
        let before = "0a631ec3-fa11-47d2-aa0f-17b320866c94"
        let rig = try makeRig(responses: [
            listResponse([listEntry(at, "2026-01-01T00:00:00Z"), listEntry(before, "2025-12-31T23:59:59Z")]),
            getResponse(record(at, createdAt: "2026-01-01T00:00:00Z")),
        ])
        try await rig.plugin.tick()
        XCTAssertEqual(try events(rig).count, 1)
        XCTAssertEqual(try rig.fake.invocations(), [FakeAnarlogCLI.listArgv(offset: 0),
                                                     FakeAnarlogCLI.getArgv(id: at)])
    }

    func testInvocationsAreListThenEligibleGetsOnly() async throws {
        let eligible = "0a631ec3-fa11-47d2-aa0f-17b320866c95"
        let young = "0a631ec3-fa11-47d2-aa0f-17b320866c96"
        let old = "0a631ec3-fa11-47d2-aa0f-17b320866c97"
        let rig = try makeRig(responses: [
            listResponse([listEntry(eligible), listEntry(young, "2026-03-20T11:30:00Z"),
                          listEntry(old, "2025-12-31T00:00:00Z")]),
            getResponse(record(eligible)),
        ])
        try await rig.plugin.tick()
        XCTAssertEqual(try rig.fake.invocations(), [FakeAnarlogCLI.listArgv(offset: 0),
                                                     FakeAnarlogCLI.getArgv(id: eligible)])
    }

    func testUnchangedRecordIsNotResent() async throws {
        let id = "0a631ec3-fa11-47d2-aa0f-17b320866c98"
        let value = record(id, summaries: ["same"])
        let rig = try makeRig(responses: [listResponse([listEntry(id)]), getResponse(value),
                                         listResponse([listEntry(id)]), getResponse(value)])
        try await rig.plugin.tick()
        try await rig.plugin.tick()
        XCTAssertEqual(try events(rig).count, 1)
    }

    func testChangedFieldResendsSession() async throws {
        let id = "0a631ec3-fa11-47d2-aa0f-17b320866c99"
        let participantA = FakeAnarlogParticipant(humanID: "11111111-1111-1111-1111-111111111111",
                                                   displayName: nil, email: nil, jobTitle: nil)
        let participantB = FakeAnarlogParticipant(humanID: "22222222-2222-2222-2222-222222222222",
                                                   displayName: nil, email: nil, jobTitle: nil)
        let cases: [(FakeAnarlogRecord, FakeAnarlogRecord)] = [
            (record(id, title: "one"), record(id, title: "two")),
            (record(id, createdAt: "2026-03-16T20:34:49Z"), record(id, createdAt: "2026-03-16T21:34:49Z")),
            (record(id, summaries: ["one"]), record(id, summaries: ["two"])),
            (record(id, memo: "one"), record(id, memo: "two")),
            (record(id, participants: [participantA]), record(id, participants: [participantB])),
        ]
        for (old, new) in cases {
            let rig = try makeRig(responses: [listResponse([listEntry(id)]), getResponse(old)])
            try await rig.plugin.tick()
            try rig.fake.setScenario(FakeAnarlogScenario(responses: [
                listResponse([listEntry(id)]), getResponse(new),
            ]))
            try await rig.plugin.tick()
            let emitted = try events(rig)
            XCTAssertEqual(emitted.count, 2)
            guard emitted.count == 2 else { continue }
            XCTAssertNotEqual(emitted[0]["source_id"] as? String,
                              emitted[1]["source_id"] as? String)
        }
    }

    func testParticipantNameOnlyChangeIsNotResent() async throws {
        let id = "0a631ec3-fa11-47d2-aa0f-17b320866c9a"
        let first = FakeAnarlogParticipant(humanID: "11111111-1111-1111-1111-111111111111",
                                           displayName: "Name A", email: nil, jobTitle: nil)
        let second = FakeAnarlogParticipant(humanID: first.humanID,
                                            displayName: "Name B", email: nil, jobTitle: nil)
        let rig = try makeRig(responses: [listResponse([listEntry(id)]),
                                         getResponse(record(id, participants: [first]))])
        try await rig.plugin.tick()
        try rig.fake.setScenario(FakeAnarlogScenario(responses: [
            listResponse([listEntry(id)]), getResponse(record(id, participants: [second])),
        ]))
        try await rig.plugin.tick()
        XCTAssertEqual(try events(rig).count, 1)
    }

    func testOperatorAndZeroIDFilteredFromParticipants() async throws {
        let id = "0a631ec3-fa11-47d2-aa0f-17b320866c9b"
        let kept = "11111111-1111-1111-1111-111111111111"
        let participants = [
            FakeAnarlogParticipant(humanID: kept, displayName: nil, email: nil, jobTitle: nil),
            FakeAnarlogParticipant(humanID: operatorID.uppercased(), displayName: nil, email: nil, jobTitle: nil),
            FakeAnarlogParticipant(humanID: CRMMacAnarlogSource.selfHumanUUID, displayName: nil, email: nil, jobTitle: nil),
            FakeAnarlogParticipant(humanID: "22222222-2222-2222-2222-222222222222", displayName: nil, email: nil, jobTitle: nil),
        ]
        let rig = try makeRig(responses: [listResponse([listEntry(id)]),
                                         getResponse(record(id, participants: participants))])
        try await rig.plugin.tick()
        XCTAssertEqual(eventPayload(try XCTUnwrap(events(rig).first))["participant_ids"] as? [String],
                       [kept, "22222222-2222-2222-2222-222222222222"])
    }

    func testGetNotFoundSkipsSessionAndKeepsPriorEntry() async throws {
        let id = "0a631ec3-fa11-47d2-aa0f-17b320866c9c"
        let rig = try makeRig(responses: [listResponse([listEntry(id)]), getNotFound(id)],
                              piScript: PiScript(cursor: try priorCursor([id: "prior"])))
        try await rig.plugin.tick()
        XCTAssertEqual(try events(rig).count, 0)
        let decoded = try XCTUnwrap(AnarlogSessionsCursorCodec.decodeOrNil(
            try XCTUnwrap(rig.transport.committedCursor)))
        XCTAssertEqual(decoded[id]?.recordHash, "prior")
    }

    func testGetFailureFailsTickWithoutPiRequests() async throws {
        let id = "0a631ec3-fa11-47d2-aa0f-17b320866c9d"
        let failure = FakeAnarlogResponse(
            argv: FakeAnarlogCLI.getArgv(id: id), exit: 1,
            stderr: FakeAnarlogCLI.errorStderr(code: "internal", exitCode: 1))
        let rig = try makeRig(responses: [listResponse([listEntry(id)]), failure])
        try await rig.plugin.tick()
        XCTAssertEqual(rig.transport.requestCount, 0)
        let observed = await health(rig)
        XCTAssertEqual(observed.map(\.1), [.failed(.nonZeroExit(code: 1, errorCode: "internal"))])
    }

    func testListingFailureSendsNothing() async throws {
        let rig = try makeRig(responses: [listFailure(code: "database_not_found", exit: 3)])
        try await rig.plugin.tick()
        XCTAssertEqual(rig.transport.requestCount, 0)
        let observed = await health(rig)
        XCTAssertEqual(observed.map(\.1), [.failed(.databaseNotFound)])
        let state = try await rig.mutator.read()
        XCTAssertEqual(state.sources["anarlog_sessions"]?.lastError,
                       "anarlog_cli_failed:databaseNotFound")
    }

    func testOperatorUnsetSendsNothing() async throws {
        let rig = try makeRig(responses: [], config: config(operatorSet: false))
        try await rig.plugin.tick()
        XCTAssertEqual(try rig.fake.invocations(), [])
        XCTAssertEqual(rig.transport.requestCount, 0)
        let observed = await health(rig)
        XCTAssertEqual(observed.map(\.1), [.failed(.operatorPersonIDUnset)])
        let state = try await rig.mutator.read()
        XCTAssertEqual(state.sources["anarlog_sessions"]?.lastError, "operator_person_id_unset")
    }

    // MARK: deletion reconciliation

    func testDeletionUsesPriorRecordHash() async throws {
        let id = "0a631ec3-fa11-47d2-aa0f-17b320866c9e"
        let rig = try makeRig(responses: [listResponse([])],
                              piScript: PiScript(cursor: try priorCursor([id: "priorhash"])))
        try await rig.plugin.tick()
        XCTAssertEqual(try events(rig).first?["source_id"] as? String,
                       "\(id)@deleted@priorhash")
    }

    func testPresentButIneligibleSessionIsNeverDeleted() async throws {
        let old = "0a631ec3-fa11-47d2-aa0f-17b320866c9f"
        let young = "0a631ec3-fa11-47d2-aa0f-17b320866ca0"
        let prior = try priorCursor([old: "oldhash", young: "younghash"])
        let rig = try makeRig(responses: [listResponse([
            listEntry(old, "2025-12-31T23:59:59Z"), listEntry(young, "2026-03-20T11:30:00Z"),
        ])], piScript: PiScript(cursor: prior))
        try await rig.plugin.tick()
        XCTAssertEqual(try events(rig).count, 0)
        let decoded = try XCTUnwrap(AnarlogSessionsCursorCodec.decodeOrNil(
            try XCTUnwrap(rig.transport.committedCursor)))
        XCTAssertEqual(decoded[old]?.recordHash, "oldhash")
        XCTAssertEqual(decoded[young]?.recordHash, "younghash")
    }

    func testDeletionsAtCapAreSent() async throws {
        let a = "0a631ec3-fa11-47d2-aa0f-17b320866ca1"
        let b = "0a631ec3-fa11-47d2-aa0f-17b320866ca2"
        let rig = try makeRig(responses: [listResponse([])],
                              config: config(deletionCap: 2),
                              piScript: PiScript(cursor: try priorCursor([a: "a", b: "b"])))
        try await rig.plugin.tick()
        XCTAssertEqual(try events(rig).filter { $0["kind"] as? String == "meeting_note.deleted" }.count, 2)
        let observed = await health(rig)
        XCTAssertEqual(observed.map(\.1), [.clean(newestSessionCreatedAt: nil)])
    }

    func testDeletionsOverCapAreWithheld() async throws {
        let ids = ["0a631ec3-fa11-47d2-aa0f-17b320866ca3", "0a631ec3-fa11-47d2-aa0f-17b320866ca4",
                   "0a631ec3-fa11-47d2-aa0f-17b320866ca5"]
        let rig = try makeRig(responses: [listResponse([])], config: config(deletionCap: 2),
                              piScript: PiScript(cursor: try priorCursor(Dictionary(uniqueKeysWithValues: ids.map { ($0, "hash") }))))
        try await rig.plugin.tick()
        XCTAssertEqual(try events(rig).count, 0)
        XCTAssertEqual(AnarlogSessionsCursorCodec.decodeOrNil(rig.transport.committedCursor ?? "")?.count, 3)
        let observed = await health(rig)
        XCTAssertEqual(observed.map(\.1), [.deletionsWithheld(withheld: 3, newestSessionCreatedAt: nil)])
    }

    func testEmptyListingOverCapWithholds() async throws {
        let ids = ["0a631ec3-fa11-47d2-aa0f-17b320866ca6", "0a631ec3-fa11-47d2-aa0f-17b320866ca7",
                   "0a631ec3-fa11-47d2-aa0f-17b320866ca8"]
        let rig = try makeRig(responses: [listResponse([])], config: config(deletionCap: 2),
                              piScript: PiScript(cursor: try priorCursor(Dictionary(uniqueKeysWithValues: ids.map { ($0, "hash") }))))
        try await rig.plugin.tick()
        let observed = await health(rig)
        XCTAssertEqual(observed.map(\.1), [.deletionsWithheld(withheld: 3, newestSessionCreatedAt: nil)])
    }

    func testBootstrapOverCapWithholds() async throws {
        let ids = ["0a631ec3-fa11-47d2-aa0f-17b320866ca9", "0a631ec3-fa11-47d2-aa0f-17b320866caa",
                   "0a631ec3-fa11-47d2-aa0f-17b320866cab"]
        let known = KnownIDsData(ids: ids.map { KnownContactID(sourceID: "\($0)@old", lastContentHash: "hash") })
        let rig = try makeRig(responses: [listResponse([])], config: config(deletionCap: 2),
                              piScript: PiScript(cursor: "", knownIDs: known))
        try await rig.plugin.tick()
        XCTAssertEqual(try events(rig).count, 0)
        let decoded = try XCTUnwrap(AnarlogSessionsCursorCodec.decodeOrNil(
            try XCTUnwrap(rig.transport.committedCursor)))
        XCTAssertEqual(decoded.count, 3)
        XCTAssertTrue(decoded.values.allSatisfy { $0.recordHash == "hash" })
    }

    func testRaisedCapReleasesWithheldDeletions() async throws {
        let ids = ["0a631ec3-fa11-47d2-aa0f-17b320866cac", "0a631ec3-fa11-47d2-aa0f-17b320866cad",
                   "0a631ec3-fa11-47d2-aa0f-17b320866cae"]
        let rig = try makeRig(responses: [listResponse([]), listResponse([])],
                              config: config(deletionCap: 2),
                              piScript: PiScript(cursor: try priorCursor(Dictionary(uniqueKeysWithValues: ids.map { ($0, "hash") }))))
        try await rig.plugin.tick()
        try rig.configSource.setDeletionCap(3)
        try await rig.plugin.tick()
        XCTAssertEqual(try events(rig).filter { $0["kind"] as? String == "meeting_note.deleted" }.count, 3)
        let observed = await health(rig).map(\.1)
        XCTAssertEqual(observed.count, 2)
        XCTAssertEqual(observed.last, .clean(newestSessionCreatedAt: nil))
    }

    func testRaisedCapReleasesWithheldBootstrapDeletions() async throws {
        let ids = ["0a631ec3-fa11-47d2-aa0f-17b320866caf", "0a631ec3-fa11-47d2-aa0f-17b320866cb0",
                   "0a631ec3-fa11-47d2-aa0f-17b320866cb1"]
        let known = KnownIDsData(ids: zip(ids, ["hash-a", "hash-b", "hash-c"]).map {
            KnownContactID(sourceID: "\($0.0)@old", lastContentHash: $0.1)
        })
        let rig = try makeRig(responses: [listResponse([]), listResponse([])],
                              config: config(deletionCap: 2),
                              piScript: PiScript(cursor: "", knownIDs: known))
        try await rig.plugin.tick()
        try rig.configSource.setDeletionCap(3)
        try await rig.plugin.tick()
        let deletedIDs = Set(try events(rig).map { $0["source_id"] as? String ?? "" })
        XCTAssertEqual(deletedIDs, Set(zip(ids, ["hash-a", "hash-b", "hash-c"]).map {
            "\($0.0)@deleted@\($0.1)"
        }))
    }

    func testLegacyFileTreeCursorBootstraps() async throws {
        let id = "0a631ec3-fa11-47d2-aa0f-17b320866cb2"
        let legacy = #"{"session":{"meta_hash":"m","summary_hash":"s","memo_hash":"n","payload_hash":"p"}}"#
        let rig = try makeRig(responses: [listResponse([listEntry(id)]), getResponse(record(id))],
                              piScript: PiScript(cursor: legacy))
        try await rig.plugin.tick()
        XCTAssertTrue(rig.transport.knownIDsCalled)
        XCTAssertEqual(try events(rig).count, 1)
    }

    func testOutcomeReportsNewestAcrossWholeListing() async throws {
        let young = "0a631ec3-fa11-47d2-aa0f-17b320866cb3"
        let recent = now
        let rig = try makeRig(responses: [listResponse([listEntry(young, "2026-03-20T11:00:00Z")])])
        try await rig.plugin.tick()
        let observed = await health(rig)
        XCTAssertEqual(observed.map(\.1), [.clean(newestSessionCreatedAt: recent.addingTimeInterval(-3600))])
    }

    // MARK: outcome timing

    func testPiFailureAfterCompleteReadRecordsNoOutcome() async throws {
        let id = "0a631ec3-fa11-47d2-aa0f-17b320866cb4"
        for endpoint in [PiEndpoint.cursorGet, .knownIDs, .ingest, .cursorCommit] {
            let script: PiScript
            if endpoint == .knownIDs {
                script = PiScript(cursor: "", failing: [.knownIDs])
            } else if endpoint == .cursorGet {
                script = PiScript(cursor: "", failing: [.cursorGet])
            } else {
                script = PiScript(cursor: "", failing: [endpoint])
            }
            let rig = try makeRig(responses: [listResponse([listEntry(id)]), getResponse(record(id))],
                                  piScript: script)
            try await rig.plugin.tick()
            let observed = await health(rig)
            XCTAssertEqual(observed.count, 0, "endpoint \(endpoint)")
            if endpoint == .cursorGet || endpoint == .knownIDs {
                XCTAssertEqual(rig.transport.ingestCallCount, 0)
            }
        }
    }

    func testHeldDeletionOutcomeSurvivesPiFailure() async throws {
        let ids = ["0a631ec3-fa11-47d2-aa0f-17b320866cb5", "0a631ec3-fa11-47d2-aa0f-17b320866cb6",
                   "0a631ec3-fa11-47d2-aa0f-17b320866cb7"]
        let rig = try makeRig(responses: [listResponse([]), listResponse([])],
                              config: config(deletionCap: 2),
                              piScript: PiScript(cursor: try priorCursor(Dictionary(uniqueKeysWithValues: ids.map { ($0, "hash") }))))
        try await rig.plugin.tick()
        let before = await health(rig)
        rig.transport.setFailing([.cursorGet])
        try await rig.plugin.tick()
        let after = await health(rig)
        XCTAssertEqual(after.map(\.1), before.map(\.1))
    }

    func testExactlyOneOutcomePerTick() async throws {
        let id = "0a631ec3-fa11-47d2-aa0f-17b320866cb8"
        let clean = try makeRig(responses: [listResponse([])])
        try await clean.plugin.tick()
        let cleanHealth = await health(clean)
        XCTAssertEqual(cleanHealth.count, 1)

        let ids = ["0a631ec3-fa11-47d2-aa0f-17b320866cb9", "0a631ec3-fa11-47d2-aa0f-17b320866cba",
                   "0a631ec3-fa11-47d2-aa0f-17b320866cbb"]
        let withheld = try makeRig(responses: [listResponse([])], config: config(deletionCap: 2),
                                  piScript: PiScript(cursor: try priorCursor(Dictionary(uniqueKeysWithValues: ids.map { ($0, "h") }))))
        try await withheld.plugin.tick()
        let withheldHealth = await health(withheld)
        XCTAssertEqual(withheldHealth.count, 1)

        let cliFailed = try makeRig(responses: [listFailure(code: "database_not_found", exit: 3)])
        try await cliFailed.plugin.tick()
        let failedHealth = await health(cliFailed)
        XCTAssertEqual(failedHealth.count, 1)

        let unset = try makeRig(responses: [], config: config(operatorSet: false))
        try await unset.plugin.tick()
        let unsetHealth = await health(unset)
        XCTAssertEqual(unsetHealth.count, 1)
        let sources = cleanHealth + withheldHealth + failedHealth + unsetHealth
        XCTAssertTrue(sources.allSatisfy { $0.0 == .anarlogSessions })
        _ = id
    }

    // MARK: factory, scheduling, and established behavior

    func testFactoryReceivesConfiguredCLIPath() async throws {
        var cfg = try config()
        try cfg.setCLIPath("/synthetic/anarlog")
        let recorder = LockedPathRecorder()
        let rig = try makeRig(responses: [listResponse([])], config: cfg, factoryRecorder: recorder)
        try await rig.plugin.tick()
        XCTAssertEqual(recorder.read(), ["/synthetic/anarlog"])
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
        let cfg = try config(cliPath: script.path)
        let rig = try makeRig(config: cfg, defaultFactory: true)
        try await rig.plugin.tick()
        let lines = try String(contentsOf: log, encoding: .utf8)
            .split(whereSeparator: \.isNewline).map(String.init)
        XCTAssertEqual(lines, FakeAnarlogCLI.listArgv(offset: 0))
        XCTAssertEqual(rig.transport.requestCount, 0)
        let observed = await health(rig)
        XCTAssertEqual(observed.count, 1)
        guard case .failed = observed.first?.1 else {
            return XCTFail("default factory failure should be reported")
        }
    }

    func testDefaultTickIntervalIsThirtyMinutes() async throws {
        let configSource = MutableConfigSource(try config())
        let piClient = PiClient(baseURL: URL(string: "https://test.invalid")!,
                                transport: { _ in throw URLError(.unsupportedURL) }, logger: NoopLogger())
        let stateStore = StateStore(fileURL: URL(fileURLWithPath: NSTemporaryDirectory())
            .appendingPathComponent("anarlog-sessions-state-\(UUID().uuidString).json"))
        try stateStore.initializeIfMissing()
        let mutator = StateMutator(store: stateStore)
        let publisher = AnarlogSessionsPublisher(
            sender: { _, _ in IngestEventsData(accepted: 0, duplicate: 0, rejected: 0, errors: []) },
            auth: testAuth, logger: NoopLogger())
        let plugin = AnarlogSessionsSourcePlugin(
            piClient: piClient, auth: testAuth, mutator: mutator, publisher: publisher,
            configSource: configSource, healthSink: NoopAnarlogHealthSink(),
            healthRegistry: SourceHealthRegistry(), logger: NoopLogger())
        XCTAssertEqual(plugin.tickInterval, 1800)
    }

    func testInFlightCoalescing() async throws {
        let id = "0a631ec3-fa11-47d2-aa0f-17b320866cbc"
        let rig = try makeRig(responses: [listResponse([listEntry(id)]),
                                         getResponse(record(id), delayMs: 250),
                                         listResponse([listEntry(id)]), getResponse(record(id))])
        let plugin = rig.plugin
        async let t1: () = plugin.tick()
        async let t2: () = plugin.tick()
        async let t3: () = plugin.tick()
        _ = try await (t1, t2, t3)
        XCTAssertLessThanOrEqual(rig.transport.ingestCallCount, 2)
        XCTAssertGreaterThanOrEqual(rig.transport.ingestCallCount, 1)
    }

    func testRecoveryBranchCallsKnownIDsEvenForSessions() async throws {
        let rig = try makeRig(responses: [listResponse([])])
        try await rig.mutator.mutate { state in
            var src = state.sources["anarlog_sessions"] ?? SourceState()
            src.lastError = "recovery_requested:test"
            state.sources["anarlog_sessions"] = src
        }
        try await rig.plugin.tick()
        XCTAssertTrue(rig.transport.knownIDsCalled,
                      "recovery branch must consult /known-ids")
    }

    func testSessionsDisabledMarksNotConfigured() async throws {
        let rig = try makeRig(responses: [], config: config(enabled: false))
        try await rig.plugin.tick()
        let state = try await rig.mutator.read()
        let src = try XCTUnwrap(state.sources["anarlog_sessions"])
        XCTAssertEqual(src.lastError, "not_configured")
        let observed = await health(rig)
        XCTAssertEqual(observed.count, 0)
        XCTAssertEqual(try rig.fake.invocations(), [])
    }

    func testConfigLoadFailureRecordsNoOutcome() async throws {
        let fake = try FakeAnarlogCLI()
        try fake.setScenario(FakeAnarlogScenario(responses: []))
        let client = fake.makeClient()
        let transport = MockTransport(PiScript())
        let piClient = PiClient(baseURL: URL(string: "https://test.invalid")!,
                                transport: transport.asFunc(), logger: NoopLogger())
        let store = StateStore(fileURL: URL(fileURLWithPath: NSTemporaryDirectory())
            .appendingPathComponent("anarlog-sessions-state-\(UUID().uuidString).json"))
        try store.initializeIfMissing()
        let mutator = StateMutator(store: store)
        let publisher = AnarlogSessionsPublisher(
            sender: { auth, body in try await piClient.ingestEvents(auth: auth, body: body) },
            auth: testAuth, logger: NoopLogger())
        let sink = RecordingHealthSink()
        let tickNow = now
        let plugin = AnarlogSessionsSourcePlugin(
            piClient: piClient, auth: testAuth, mutator: mutator, publisher: publisher,
            configSource: FailingConfigSource(), makeCLIClient: { _ in client },
            healthSink: sink, healthRegistry: SourceHealthRegistry(),
            logger: NoopLogger(), clock: { tickNow })
        try await plugin.tick()
        let observed = await sink.recorded()
        XCTAssertEqual(observed.count, 0)
        _ = fake
    }
}
