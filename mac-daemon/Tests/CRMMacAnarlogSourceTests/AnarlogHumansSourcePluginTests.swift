// Tests the CLI-driven people source with synthetic Anarlog CLI and Pi
// responses. People come from eligible sessions, are sent as upserts,
// and the source reports the completed CLI phase exactly once.
import Foundation
import XCTest
import CRMMacCore
import CRMMacPiClient
@testable import CRMMacAnarlogSource

final class AnarlogHumansSourcePluginTests: XCTestCase {

    private let testAuth = PiAuth(
        hostID: UUID(uuidString: "11111111-2222-3333-4444-555555555555")!,
        apiKey: "k")
    private let operatorID = "aaaaaaaa-0000-4000-8000-000000000001"
    private let p1 = "bbbbbbbb-0000-4000-8000-000000000001"
    private let p2 = "bbbbbbbb-0000-4000-8000-000000000002"
    private let p3 = "bbbbbbbb-0000-4000-8000-000000000003"
    private let p4 = "bbbbbbbb-0000-4000-8000-000000000004"
    private let p5 = "bbbbbbbb-0000-4000-8000-000000000005"
    private let p9 = "bbbbbbbb-0000-4000-8000-000000000009"
    private let s1 = "cccccccc-0000-4000-8000-000000000001"
    private let s2 = "cccccccc-0000-4000-8000-000000000002"
    private let s3 = "cccccccc-0000-4000-8000-000000000003"
    private let s4 = "cccccccc-0000-4000-8000-000000000004"
    private let fixedNow = ISO8601DateFormatter().date(from: "2026-06-01T12:00:00Z")!

    private struct PiScript {
        var cursorGet: SourceCursorState = SourceCursorState(
            cursor: "", cursorEpoch: 0, backfillComplete: false)
        var cursorGetStatus = 200
        var knownIDs = KnownIDsData(ids: [])
        var ingestResult = IngestEventsData(accepted: 0, duplicate: 0, rejected: 0, errors: [])
    }

    private final class MockTransport: @unchecked Sendable {
        struct Snapshot {
            let requests: [String]
            let ingestBodies: [Data]
            let committedCursor: String?
            let commitWasAttempted: Bool
        }

        private let lock = NSLock()
        private let script: PiScript
        private var requestLog: [String] = []
        private var bodies: [Data] = []
        private var committed: String?
        private var didCommit = false

        init(_ script: PiScript) { self.script = script }

        func asFunc() -> TransportFunc {
            { [self] request in
                let path = request.url?.path ?? ""
                let method = request.httpMethod ?? "GET"
                recordRequest("\(method) \(path)")

                if path.hasSuffix("/cursor") && method == "GET" {
                    if script.cursorGetStatus == 200 {
                        return (encodeCursor(script.cursorGet), Self.response(request, status: 200))
                    }
                    let error = Data(#"{"success":false,"error":{"code":"BAD_REQUEST","message":"synthetic"}}"#.utf8)
                    return (error, Self.response(request, status: script.cursorGetStatus))
                }
                if path.hasSuffix("/known-ids") && method == "GET" {
                    return (encodeKnownIDs(script.knownIDs), Self.response(request, status: 200))
                }
                if path.hasSuffix("/ingest/events") && method == "POST" {
                    if let body = request.httpBody {
                        recordBody(body)
                    }
                    return (encodeIngest(script.ingestResult), Self.response(request, status: 200))
                }
                if path.hasSuffix("/cursor") && method == "POST" {
                    recordCursorCommit(request.httpBody)
                    return (Data(#"{"success":true,"data":{"ok":true}}"#.utf8), Self.response(request, status: 200))
                }
                throw URLError(.unsupportedURL)
            }
        }

        func snapshot() -> Snapshot {
            lock.lock()
            defer { lock.unlock() }
            return Snapshot(
                requests: requestLog,
                ingestBodies: bodies,
                committedCursor: committed,
                commitWasAttempted: didCommit)
        }

        private func recordRequest(_ value: String) {
            lock.lock()
            requestLog.append(value)
            lock.unlock()
        }

        private func recordBody(_ value: Data) {
            lock.lock()
            bodies.append(value)
            lock.unlock()
        }

        private func recordCursorCommit(_ body: Data?) {
            lock.lock()
            didCommit = true
            if let body,
               let parsed = try? JSONSerialization.jsonObject(with: body) as? [String: Any] {
                committed = parsed["cursor"] as? String
            }
            lock.unlock()
        }

        private func encodeCursor(_ cursor: SourceCursorState) -> Data {
            let root: [String: Any] = [
                "success": true,
                "data": [
                    "cursor": cursor.cursor,
                    "cursor_epoch": cursor.cursorEpoch,
                    "backfill_complete": cursor.backfillComplete,
                ],
            ]
            return try! JSONSerialization.data(withJSONObject: root)
        }

        private func encodeKnownIDs(_ knownIDs: KnownIDsData) -> Data {
            let ids: [[String: Any]] = knownIDs.ids.map { entry in
                ["source_id": entry.sourceID, "last_content_hash": entry.lastContentHash as Any? ?? NSNull()]
            }
            return try! JSONSerialization.data(withJSONObject: [
                "success": true, "data": ["ids": ids],
            ])
        }

        private func encodeIngest(_ ingest: IngestEventsData) -> Data {
            let errors: [[String: Any]] = ingest.errors.map { error in
                ["index": error.index, "code": error.code, "message": error.message]
            }
            return try! JSONSerialization.data(withJSONObject: [
                "accepted": ingest.accepted,
                "duplicate": ingest.duplicate,
                "rejected": ingest.rejected,
                "errors": errors,
            ])
        }

        private static func response(_ request: URLRequest, status: Int) -> HTTPURLResponse {
            HTTPURLResponse(
                url: request.url!, statusCode: status, httpVersion: "HTTP/1.1",
                headerFields: ["Content-Type": "application/json"])!
        }
    }

    private struct RecordedOutcome: Equatable {
        let source: SourceID
        let outcome: AnarlogTickOutcome
    }

    private actor SpyHealthSink: AnarlogHealthSink {
        private var values: [RecordedOutcome] = []

        func record(source: SourceID, outcome: AnarlogTickOutcome) async {
            values.append(RecordedOutcome(source: source, outcome: outcome))
        }

        func snapshot() -> [RecordedOutcome] { values }
    }

    private final class StubConfigSource: AnarlogConfigSource, @unchecked Sendable {
        let value: AnarlogConfig?
        init(_ value: AnarlogConfig?) { self.value = value }
        func load() throws -> AnarlogConfig? { value }
    }

    private final class PathRecorder: @unchecked Sendable {
        private let lock = NSLock()
        private var recorded: [String?] = []

        func append(_ path: String?) {
            lock.lock()
            recorded.append(path)
            lock.unlock()
        }

        func values() -> [String?] {
            lock.lock()
            defer { lock.unlock() }
            return recorded
        }
    }

    private struct Rig {
        let plugin: AnarlogHumansSourcePlugin
        let fake: FakeAnarlogCLI
        let mutator: StateMutator
        let transport: MockTransport
        let healthSink: SpyHealthSink
    }

    private var listing: [FakeAnarlogListEntry] {
        [
            FakeAnarlogListEntry(id: s4, createdAt: "2026-06-01T10:00:00.000Z"),
            FakeAnarlogListEntry(id: s2, createdAt: "2026-05-20T09:00:00.000Z"),
            FakeAnarlogListEntry(id: s1, createdAt: "2026-05-10T09:00:00.000Z"),
            FakeAnarlogListEntry(id: s3, createdAt: "2025-12-31T23:59:59.000Z"),
        ]
    }

    private var standardRecords: [String: FakeAnarlogRecord] {
        [
            s2: FakeAnarlogRecord(
                id: s2,
                title: "Session two",
                createdAt: "2026-05-20T09:00:00.000Z",
                noteMarkdown: nil,
                summaries: [],
                participants: [
                    FakeAnarlogParticipant(humanID: p2, displayName: "Contact B newer", email: "b@example.invalid", jobTitle: nil),
                    FakeAnarlogParticipant(humanID: p3, displayName: "Contact C", email: nil, jobTitle: "Designer"),
                ]),
            s1: FakeAnarlogRecord(
                id: s1,
                title: "Session one",
                createdAt: "2026-05-10T09:00:00.000Z",
                noteMarkdown: nil,
                summaries: [],
                participants: [
                    FakeAnarlogParticipant(humanID: operatorID, displayName: "Operator", email: nil, jobTitle: nil),
                    FakeAnarlogParticipant(humanID: CRMMacAnarlogSource.selfHumanUUID, displayName: "Sentinel", email: nil, jobTitle: nil),
                    FakeAnarlogParticipant(humanID: p1, displayName: "Contact A", email: "a@example.invalid", jobTitle: "Engineer"),
                    FakeAnarlogParticipant(humanID: p2, displayName: "Contact B older", email: "b@example.invalid", jobTitle: nil),
                ]),
        ]
    }

    private func configuredConfig() throws -> AnarlogConfig {
        var config = AnarlogConfig(
            humansEnabled: true, sessionsEnabled: false)
        try config.setOperatorPersonID(operatorID)
        return config
    }

    private func scenario(
        listing entries: [FakeAnarlogListEntry]? = nil,
        records: [String: FakeAnarlogRecord]? = nil,
        overrides: [String: FakeAnarlogResponse] = [:]
    ) -> FakeAnarlogScenario {
        let listEntries = entries ?? listing
        let values = records ?? standardRecords
        let listArgv = FakeAnarlogCLI.listArgv(offset: 0)
        var responses = [overrides[responseKey(listArgv)] ?? FakeAnarlogResponse(
            argv: listArgv,
            stdout: FakeAnarlogCLI.listStdout(entries: listEntries, offset: 0, nextOffset: nil))]
        for entry in listEntries {
            let argv = FakeAnarlogCLI.getArgv(id: entry.id)
            if let override = overrides[responseKey(argv)] {
                responses.append(override)
            } else if let record = values[entry.id] {
                responses.append(FakeAnarlogResponse(
                    argv: argv, stdout: FakeAnarlogCLI.getStdout(record)))
            }
        }
        return FakeAnarlogScenario(responses: responses)
    }

    private func responseKey(_ argv: [String]) -> String {
        argv.joined(separator: "\u{1f}")
    }

    private func makeRig(
        config: AnarlogConfig? = nil,
        missingConfig: Bool = false,
        defaultFactory: Bool = false,
        cursor: String = "",
        cursorStatus: Int = 200,
        knownIDs: [KnownContactID] = [],
        ingestResult: IngestEventsData = IngestEventsData(accepted: 0, duplicate: 0, rejected: 0, errors: []),
        cliScenario: FakeAnarlogScenario? = nil,
        tickInterval: TimeInterval? = nil,
        makeClient overrideFactory: (@Sendable (String?) -> any AnarlogCLIClient)? = nil
    ) throws -> Rig {
        let fake = try FakeAnarlogCLI()
        try fake.setScenario(cliScenario ?? scenario())
        let client = fake.makeClient()
        let factory: @Sendable (String?) -> any AnarlogCLIClient = overrideFactory ?? { _ in client }
        let configuredCLIPath: String?
        if defaultFactory {
            let script = fake.directory.appendingPathComponent("argv-recorder")
            let log = fake.directory.appendingPathComponent("argv.log")
            let scriptText = """
            #!/bin/sh
            printf '%s\\n' "$@" >> "\(log.path)"
            exit 1
            """
            try Data(scriptText.utf8).write(to: script)
            try FileManager.default.setAttributes(
                [.posixPermissions: 0o755], ofItemAtPath: script.path)
            configuredCLIPath = script.path
        } else {
            configuredCLIPath = nil
        }

        let script = PiScript(
            cursorGet: SourceCursorState(cursor: cursor, cursorEpoch: 0, backfillComplete: !cursor.isEmpty),
            cursorGetStatus: cursorStatus,
            knownIDs: KnownIDsData(ids: knownIDs),
            ingestResult: ingestResult)
        let transport = MockTransport(script)
        let piClient = PiClient(
            baseURL: URL(string: "https://test.invalid")!,
            transport: transport.asFunc(), logger: NoopLogger())
        let stateURL = URL(fileURLWithPath: NSTemporaryDirectory())
            .appendingPathComponent("anarlog-humans-state-\(UUID().uuidString).json")
        let stateStore = StateStore(fileURL: stateURL)
        try stateStore.initializeIfMissing()
        let mutator = StateMutator(store: stateStore)
        let publisher = AnarlogHumansPublisher(
            sender: { auth, body in try await piClient.ingestEvents(auth: auth, body: body) },
            auth: testAuth, logger: NoopLogger())
        let healthSink = SpyHealthSink()
        let fixedNowValue = fixedNow
        let effectiveConfig: AnarlogConfig?
        if missingConfig {
            effectiveConfig = nil
        } else {
            var value = try config ?? configuredConfig()
            if let configuredCLIPath { try value.setCLIPath(configuredCLIPath) }
            effectiveConfig = value
        }
        let common: (TimeInterval?, PiClient, StateMutator, AnarlogHumansPublisher, AnarlogConfigSource, SpyHealthSink) = (
            tickInterval, piClient, mutator, publisher, StubConfigSource(effectiveConfig), healthSink)
        let plugin: AnarlogHumansSourcePlugin
        if defaultFactory {
            plugin = AnarlogHumansSourcePlugin(
                piClient: common.1,
                auth: testAuth,
                mutator: common.2,
                publisher: common.3,
                configSource: common.4,
                healthSink: common.5,
                healthRegistry: SourceHealthRegistry(),
                logger: NoopLogger(),
                clock: { fixedNowValue })
        } else if let tickInterval = common.0 {
            plugin = AnarlogHumansSourcePlugin(
                tickInterval: tickInterval,
                piClient: common.1,
                auth: testAuth,
                mutator: common.2,
                publisher: common.3,
                configSource: common.4,
                makeCLIClient: factory,
                healthSink: common.5,
                healthRegistry: SourceHealthRegistry(),
                logger: NoopLogger(),
                clock: { fixedNowValue })
        } else {
            plugin = AnarlogHumansSourcePlugin(
                piClient: common.1,
                auth: testAuth,
                mutator: common.2,
                publisher: common.3,
                configSource: common.4,
                makeCLIClient: factory,
                healthSink: common.5,
                healthRegistry: SourceHealthRegistry(),
                logger: NoopLogger(),
                clock: { fixedNowValue })
        }
        return Rig(plugin: plugin, fake: fake, mutator: mutator, transport: transport, healthSink: healthSink)
    }

    func testDefaultFactoryRunsConfiguredCLIPath() async throws {
        let rig = try makeRig(defaultFactory: true)
        try await rig.plugin.performTick()

        let log = rig.fake.directory.appendingPathComponent("argv.log")
        let lines = try String(contentsOf: log, encoding: .utf8)
            .split(whereSeparator: \.isNewline).map(String.init)
        XCTAssertEqual(lines, FakeAnarlogCLI.listArgv(offset: 0))
        XCTAssertTrue(rig.transport.snapshot().requests.isEmpty)
        let recorded = await outcomes(rig.healthSink)
        XCTAssertEqual(recorded.count, 1)
        if let first = recorded.first {
            if case .failed = first.outcome {
            } else {
                XCTFail("expected a failed CLI outcome")
            }
        }
    }

    private func expectedHash(_ participant: AnarlogParticipant) throws -> String {
        let payload = AnarlogHumansPayloadShaping.shape(participant: participant, hostID: testAuth.hostID)
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.withoutEscapingSlashes]
        return try ContentHasher.contentHash(for: encoder.encode(payload))
    }

    private func standardCursor() throws -> String {
        try AnarlogHumansCursorCodec.encode([
            p1: AnarlogHumansCursorEntry(recordHash: expectedHash(
                AnarlogParticipant(personID: p1, displayName: "Contact A", email: "a@example.invalid", jobTitle: "Engineer"))),
            p2: AnarlogHumansCursorEntry(recordHash: expectedHash(
                AnarlogParticipant(personID: p2, displayName: "Contact B newer", email: "b@example.invalid", jobTitle: nil))),
            p3: AnarlogHumansCursorEntry(recordHash: expectedHash(
                AnarlogParticipant(personID: p3, displayName: "Contact C", email: nil, jobTitle: "Designer"))),
        ])
    }

    private func events(_ snapshot: MockTransport.Snapshot) throws -> [[String: Any]] {
        try snapshot.ingestBodies.flatMap { bytes in
            let body = try XCTUnwrap(JSONSerialization.jsonObject(with: bytes) as? [String: Any])
            return try XCTUnwrap(body["events"] as? [[String: Any]])
        }
    }

    private func outcomes(_ sink: SpyHealthSink) async -> [RecordedOutcome] {
        await sink.snapshot()
    }

    private func assertSingleCleanOutcome(
        _ rig: Rig,
        newest: Date?,
        file: StaticString = #filePath,
        line: UInt = #line
    ) async {
        let values = await outcomes(rig.healthSink)
        XCTAssertEqual(values.count, 1, file: file, line: line)
        XCTAssertEqual(values.first?.source, .anarlogHumans, file: file, line: line)
        XCTAssertEqual(values.first?.outcome, .clean(newestSessionCreatedAt: newest), file: file, line: line)
    }

    func testFirstRunSendsEligibleParticipantsExceptOperatorAndSentinel() async throws {
        let rig = try makeRig()
        try await rig.plugin.tick()
        let snapshot = rig.transport.snapshot()
        let parsed = try events(snapshot)
        XCTAssertEqual(parsed.count, 3)
        XCTAssertEqual(parsed.map { $0["kind"] as? String }, Array(repeating: "external_contact.upserted", count: 3))
        XCTAssertEqual(parsed.compactMap { ($0["payload"] as? [String: Any])?["entity_id"] as? String }, [p1, p2, p3])
        XCTAssertEqual((parsed[1]["payload"] as? [String: Any])?["display_name"] as? String, "Contact B newer")
        let participants = [
            AnarlogParticipant(personID: p1, displayName: "Contact A", email: "a@example.invalid", jobTitle: "Engineer"),
            AnarlogParticipant(personID: p2, displayName: "Contact B newer", email: "b@example.invalid", jobTitle: nil),
            AnarlogParticipant(personID: p3, displayName: "Contact C", email: nil, jobTitle: "Designer"),
        ]
        let expectedSourceIDs = try [p1, p2, p3].enumerated().map { index, id in
            "\(id)@\(try expectedHash(participants[index]))"
        }
        XCTAssertEqual(parsed.map { $0["source_id"] as? String }, expectedSourceIDs)
        let expectedHashes = try participants.map { try expectedHash($0) }
        XCTAssertEqual(AnarlogHumansCursorCodec.decodeOrNil(try XCTUnwrap(snapshot.committedCursor)), [
            p1: AnarlogHumansCursorEntry(recordHash: expectedHashes[0]),
            p2: AnarlogHumansCursorEntry(recordHash: expectedHashes[1]),
            p3: AnarlogHumansCursorEntry(recordHash: expectedHashes[2]),
        ])
        XCTAssertEqual(try rig.fake.invocations(), [
            FakeAnarlogCLI.listArgv(offset: 0),
            FakeAnarlogCLI.getArgv(id: s2),
            FakeAnarlogCLI.getArgv(id: s1),
        ])
        await assertSingleCleanOutcome(rig, newest: ISO8601DateFormatter().date(from: "2026-06-01T10:00:00Z"))
    }

    func testUnchangedPersonIsNotResent() async throws {
        let prior = try standardCursor()
        let rig = try makeRig(cursor: prior)
        try await rig.plugin.tick()
        let snapshot = rig.transport.snapshot()
        XCTAssertTrue(snapshot.ingestBodies.isEmpty)
        XCTAssertEqual(snapshot.committedCursor, prior)
    }

    func testChangedPersonIsResentWithoutSessionChange() async throws {
        let prior = try standardCursor()
        var records = standardRecords
        let session = records[s1]!
        let changedParticipants = session.participants.map { participant in
            participant.humanID == p1
                ? FakeAnarlogParticipant(humanID: p1, displayName: "Contact A", email: "a@example.invalid", jobTitle: "Lead")
                : participant
        }
        records[s1] = FakeAnarlogRecord(id: session.id, title: session.title, createdAt: session.createdAt,
            noteMarkdown: session.noteMarkdown, summaries: session.summaries, participants: changedParticipants)
        let rig = try makeRig(cursor: prior, cliScenario: scenario(records: records))
        try await rig.plugin.tick()
        let parsed = try events(rig.transport.snapshot())
        XCTAssertEqual(parsed.count, 1)
        XCTAssertEqual((parsed[0]["payload"] as? [String: Any])?["job_title"] as? String, "Lead")
        let changed = AnarlogParticipant(personID: p1, displayName: "Contact A", email: "a@example.invalid", jobTitle: "Lead")
        XCTAssertEqual(parsed[0]["source_id"] as? String, "\(p1)@\(try expectedHash(changed))")
        let committed = try XCTUnwrap(rig.transport.snapshot().committedCursor)
        XCTAssertEqual(AnarlogHumansCursorCodec.decodeOrNil(committed)?[p1], AnarlogHumansCursorEntry(recordHash: try expectedHash(changed)))
    }

    func testLegacyFileTreeCursorResendsEveryPerson() async throws {
        let cursor = #"{"bbbbbbbb-0000-4000-8000-000000000001":{"content_hash":"a","payload_hash":"b","mtime_epoch_ms":1}}"#
        let rig = try makeRig(cursor: cursor)
        try await rig.plugin.tick()
        let snapshot = rig.transport.snapshot()
        XCTAssertEqual(try events(snapshot).count, 3)
        XCTAssertFalse(snapshot.requests.contains { $0.hasSuffix("/known-ids") })
    }

    func testPersonLeavingEligibleSessionsIsNeverDeleted() async throws {
        var cursor = try XCTUnwrap(AnarlogHumansCursorCodec.decodeOrNil(standardCursor()))
        cursor[p9] = AnarlogHumansCursorEntry(recordHash: "x")
        let rig = try makeRig(cursor: try AnarlogHumansCursorCodec.encode(cursor))
        try await rig.plugin.tick()
        let snapshot = rig.transport.snapshot()
        XCTAssertTrue(snapshot.ingestBodies.isEmpty)
        XCTAssertEqual(Set(try XCTUnwrap(AnarlogHumansCursorCodec.decodeOrNil(try XCTUnwrap(snapshot.committedCursor))).keys), Set([p1, p2, p3]))
    }

    func testBootstrapWithPiKnownIdentitiesSendsNoDeletion() async throws {
        let rig = try makeRig(knownIDs: [KnownContactID(sourceID: "\(p9)@deadbeef", lastContentHash: "deadbeef")])
        try await rig.plugin.tick()
        let snapshot = rig.transport.snapshot()
        let parsed = try events(snapshot)
        XCTAssertEqual(parsed.count, 3)
        XCTAssertTrue(parsed.allSatisfy { $0["kind"] as? String == "external_contact.upserted" })
        XCTAssertFalse(snapshot.requests.contains { $0.hasSuffix("/known-ids") })
    }

    func testEmptyListingWithPriorPeopleDeletesNothingAndReportsNilNewest() async throws {
        var cursor = try XCTUnwrap(AnarlogHumansCursorCodec.decodeOrNil(standardCursor()))
        cursor[p9] = AnarlogHumansCursorEntry(recordHash: "x")
        let emptyScenario = scenario(listing: [])
        let rig = try makeRig(
            cursor: try AnarlogHumansCursorCodec.encode(cursor),
            knownIDs: [
                KnownContactID(sourceID: "\(p1)@deadbeef", lastContentHash: "deadbeef"),
                KnownContactID(sourceID: "\(p9)@deadbeef", lastContentHash: "deadbeef"),
            ],
            cliScenario: emptyScenario)
        try await rig.plugin.tick()
        let snapshot = rig.transport.snapshot()
        XCTAssertEqual(try rig.fake.invocations(), [FakeAnarlogCLI.listArgv(offset: 0)])
        XCTAssertTrue(snapshot.ingestBodies.isEmpty)
        XCTAssertFalse(snapshot.requests.contains { $0.hasSuffix("/known-ids") })
        XCTAssertEqual(snapshot.committedCursor, "{}")
        await assertSingleCleanOutcome(rig, newest: nil)
    }

    func testSessionDeletedBetweenListAndGetIsSkipped() async throws {
        let notFound = FakeAnarlogResponse(
            argv: FakeAnarlogCLI.getArgv(id: s2), exit: 2,
            stderr: FakeAnarlogCLI.errorStderr(code: "not_found", exitCode: 2))
        let scenario = self.scenario(overrides: [responseKey(notFound.argv): notFound])
        let rig = try makeRig(cliScenario: scenario)
        try await rig.plugin.tick()
        let parsed = try events(rig.transport.snapshot())
        XCTAssertEqual(parsed.compactMap { ($0["payload"] as? [String: Any])?["entity_id"] as? String }, [p1, p2])
        XCTAssertEqual((parsed[1]["payload"] as? [String: Any])?["display_name"] as? String, "Contact B older")
        await assertSingleCleanOutcome(rig, newest: ISO8601DateFormatter().date(from: "2026-06-01T10:00:00Z"))
    }

    func testOperatorUnsetSendsNothingAndReportsFailure() async throws {
        let config = AnarlogConfig(humansEnabled: true, sessionsEnabled: false)
        let rig = try makeRig(config: config)
        try await rig.plugin.tick()
        let values = await outcomes(rig.healthSink)
        XCTAssertEqual(values, [RecordedOutcome(source: .anarlogHumans, outcome: .failed(.operatorPersonIDUnset))])
        XCTAssertTrue(try rig.fake.invocations().isEmpty)
        XCTAssertTrue(rig.transport.snapshot().requests.isEmpty)
        let state = try await rig.mutator.read()
        XCTAssertEqual(state.sources["anarlog_humans"]?.lastError, "operator_person_id_unset")
    }

    func testListFailureReportsFailureAndMakesNoPiRequest() async throws {
        let argv = FakeAnarlogCLI.listArgv(offset: 0)
        let failure = FakeAnarlogResponse(
            argv: argv, exit: 3,
            stderr: FakeAnarlogCLI.errorStderr(code: "database_not_found", exitCode: 3))
        let rig = try makeRig(cliScenario: scenario(overrides: [responseKey(argv): failure]))
        try await rig.plugin.tick()
        let values = await outcomes(rig.healthSink)
        XCTAssertEqual(values, [RecordedOutcome(source: .anarlogHumans, outcome: .failed(.databaseNotFound))])
        XCTAssertTrue(rig.transport.snapshot().requests.isEmpty)
        let state = try await rig.mutator.read()
        XCTAssertEqual(state.sources["anarlog_humans"]?.lastError, "anarlog_cli_failed:databaseNotFound")
    }

    func testGetFailureReportsFailureAndSendsNothing() async throws {
        let argv = FakeAnarlogCLI.getArgv(id: s1)
        let failure = FakeAnarlogResponse(argv: argv, exit: 1, stderr: "boom")
        let rig = try makeRig(cliScenario: scenario(overrides: [responseKey(argv): failure]))
        try await rig.plugin.tick()
        let values = await outcomes(rig.healthSink)
        XCTAssertEqual(values, [RecordedOutcome(
            source: .anarlogHumans,
            outcome: .failed(.nonZeroExit(code: 1, errorCode: nil)))])
        XCTAssertTrue(rig.transport.snapshot().requests.isEmpty)
    }

    func testDisabledSourceRecordsNothingAndReadsNoCLI() async throws {
        let config = AnarlogConfig(humansEnabled: false, sessionsEnabled: true)
        let rig = try makeRig(config: config)
        try await rig.plugin.tick()
        let recorded = await outcomes(rig.healthSink)
        XCTAssertTrue(recorded.isEmpty)
        XCTAssertTrue(try rig.fake.invocations().isEmpty)
        let state = try await rig.mutator.read()
        XCTAssertEqual(state.sources["anarlog_humans"]?.lastError, "not_configured")
    }

    func testNilConfigRecordsNothing() async throws {
        let rig = try makeRig(missingConfig: true)
        try await rig.plugin.tick()
        let recorded = await outcomes(rig.healthSink)
        XCTAssertTrue(recorded.isEmpty)
        XCTAssertTrue(try rig.fake.invocations().isEmpty)
        let state = try await rig.mutator.read()
        XCTAssertEqual(state.sources["anarlog_humans"]?.lastError, "not_configured")
    }

    func testPeopleSyncWithSessionsSourceDisabled() async throws {
        let rig = try makeRig()
        try await rig.plugin.tick()
        XCTAssertEqual(try events(rig.transport.snapshot()).compactMap {
            ($0["payload"] as? [String: Any])?["entity_id"] as? String
        }, [p1, p2, p3])
    }

    func testHashMismatchSetsRecoveryFlag() async throws {
        let result = IngestEventsData(
            accepted: 0, duplicate: 0, rejected: 1,
            errors: [IngestEventError(index: 0, code: "EXTERNAL_CONTACT_HASH_MISMATCH", message: "synthetic")])
        let rig = try makeRig(ingestResult: result)
        try await rig.plugin.tick()
        let state = try await rig.mutator.read()
        XCTAssertTrue((state.sources["anarlog_humans"]?.lastError ?? "").contains("recovery_requested"))
        XCTAssertFalse(rig.transport.snapshot().commitWasAttempted)
    }

    func testRecoveryFlagResendsEveryPerson() async throws {
        let rig = try makeRig(cursor: standardCursor())
        try await rig.mutator.mutate { state in
            var source = state.sources["anarlog_humans"] ?? SourceState()
            source.lastError = "recovery_requested:hash_mismatch"
            state.sources["anarlog_humans"] = source
        }
        try await rig.plugin.tick()
        XCTAssertEqual(try events(rig.transport.snapshot()).compactMap {
            ($0["payload"] as? [String: Any])?["entity_id"] as? String
        }, [p1, p2, p3])
        let state = try await rig.mutator.read()
        XCTAssertNil(state.sources["anarlog_humans"]?.lastError)
    }

    func testOversizedPayloadCarriesPriorEntryAndRecordsAnomaly() async throws {
        var cursor = try XCTUnwrap(AnarlogHumansCursorCodec.decodeOrNil(standardCursor()))
        cursor[p1] = AnarlogHumansCursorEntry(recordHash: "prior")
        var records = standardRecords
        let session = records[s1]!
        let largeName = String(repeating: "x", count: CRMMacAnarlogSource.maxPayloadBytes + 1024)
        let changedParticipants = session.participants.map { participant in
            participant.humanID == p1
                ? FakeAnarlogParticipant(humanID: p1, displayName: largeName, email: participant.email, jobTitle: participant.jobTitle)
                : participant
        }
        records[s1] = FakeAnarlogRecord(id: session.id, title: session.title, createdAt: session.createdAt,
            noteMarkdown: session.noteMarkdown, summaries: session.summaries, participants: changedParticipants)
        let rig = try makeRig(cursor: try AnarlogHumansCursorCodec.encode(cursor), cliScenario: scenario(records: records))
        try await rig.plugin.tick()
        let snapshot = rig.transport.snapshot()
        XCTAssertTrue(try events(snapshot).isEmpty)
        let committed = try XCTUnwrap(snapshot.committedCursor)
        XCTAssertEqual(AnarlogHumansCursorCodec.decodeOrNil(committed)?[p1], AnarlogHumansCursorEntry(recordHash: "prior"))
        let state = try await rig.mutator.read()
        XCTAssertEqual(state.sources["anarlog_humans"]?.lastError,
            "anomalies encode_failed=0 payload_too_large=1")
    }

    func testCursorFetchFailureAfterCleanReadRecordsOneOutcome() async throws {
        let rig = try makeRig(cursorStatus: 400)
        try await rig.plugin.tick()
        await assertSingleCleanOutcome(rig, newest: ISO8601DateFormatter().date(from: "2026-06-01T10:00:00Z"))
        let state = try await rig.mutator.read()
        XCTAssertEqual(state.sources["anarlog_humans"]?.lastError, "cursor_fetch_failed")
        XCTAssertTrue(rig.transport.snapshot().ingestBodies.isEmpty)
    }

    func testDefaultTickIntervalIsThirtyMinutes() async throws {
        XCTAssertEqual(CRMMacAnarlogSource.humansTickInterval, 1800)
        let rig = try makeRig()
        XCTAssertEqual(rig.plugin.tickInterval, 1800)
    }

    func testClientFactoryReceivesConfiguredCLIPath() async throws {
        var config = try configuredConfig()
        try config.setCLIPath("/opt/synthetic/anarlog")
        let fake = try FakeAnarlogCLI()
        try fake.setScenario(scenario())
        let client = fake.makeClient()
        let recorder = PathRecorder()
        let factory: @Sendable (String?) -> any AnarlogCLIClient = { path in
            recorder.append(path)
            return client
        }
        let rig = try makeRig(config: config, makeClient: factory)
        try await rig.plugin.tick()
        XCTAssertEqual(recorder.values(), ["/opt/synthetic/anarlog"])
    }

    func testChangedNameIsResentWithoutSessionChange() async throws {
        let prior = try standardCursor()
        var records = standardRecords
        let session = records[s1]!
        let changedParticipants = session.participants.map { participant in
            participant.humanID == p1
                ? FakeAnarlogParticipant(humanID: p1, displayName: "Contact A renamed", email: participant.email, jobTitle: participant.jobTitle)
                : participant
        }
        records[s1] = FakeAnarlogRecord(id: session.id, title: session.title, createdAt: session.createdAt,
            noteMarkdown: session.noteMarkdown, summaries: session.summaries, participants: changedParticipants)
        let rig = try makeRig(cursor: prior, cliScenario: scenario(records: records))
        try await rig.plugin.tick()
        let parsed = try events(rig.transport.snapshot())
        XCTAssertEqual(parsed.count, 1)
        XCTAssertEqual((parsed[0]["payload"] as? [String: Any])?["display_name"] as? String, "Contact A renamed")
        let changed = AnarlogParticipant(personID: p1, displayName: "Contact A renamed", email: "a@example.invalid", jobTitle: "Engineer")
        XCTAssertEqual(parsed[0]["source_id"] as? String, "\(p1)@\(try expectedHash(changed))")
        let committed = try XCTUnwrap(rig.transport.snapshot().committedCursor)
        XCTAssertEqual(AnarlogHumansCursorCodec.decodeOrNil(committed)?[p1], AnarlogHumansCursorEntry(recordHash: try expectedHash(changed)))
    }

    func testChangedEmailIsResentWithoutSessionChange() async throws {
        let prior = try standardCursor()
        var records = standardRecords
        let session = records[s1]!
        let changedParticipants = session.participants.map { participant in
            participant.humanID == p1
                ? FakeAnarlogParticipant(humanID: p1, displayName: participant.displayName, email: "a.new@example.invalid", jobTitle: participant.jobTitle)
                : participant
        }
        records[s1] = FakeAnarlogRecord(id: session.id, title: session.title, createdAt: session.createdAt,
            noteMarkdown: session.noteMarkdown, summaries: session.summaries, participants: changedParticipants)
        let rig = try makeRig(cursor: prior, cliScenario: scenario(records: records))
        try await rig.plugin.tick()
        let parsed = try events(rig.transport.snapshot())
        XCTAssertEqual(parsed.count, 1)
        XCTAssertEqual((parsed[0]["payload"] as? [String: Any])?["emails"] as? [[String: String]], [["value": "a.new@example.invalid"]])
        let changed = AnarlogParticipant(personID: p1, displayName: "Contact A", email: "a.new@example.invalid", jobTitle: "Engineer")
        XCTAssertEqual(parsed[0]["source_id"] as? String, "\(p1)@\(try expectedHash(changed))")
        let committed = try XCTUnwrap(rig.transport.snapshot().committedCursor)
        XCTAssertEqual(AnarlogHumansCursorCodec.decodeOrNil(committed)?[p1], AnarlogHumansCursorEntry(recordHash: try expectedHash(changed)))
    }
}
