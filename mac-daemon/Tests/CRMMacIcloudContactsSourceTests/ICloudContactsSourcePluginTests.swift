// Tests for the ICloudContactsSourcePlugin per-tick orchestrator.
//
// Coverage: first-run path, delta path, recovery (token-invalid),
// container-allowlist filtering, .unknown event fail-closed,
// state-loss state-loss guard (cursor empty AND /known-ids non-empty),
// permission-denied unhealthy, no-containers unhealthy, cursor-commit
// failure path (no cursor advance, removals discarded), and the
// hash-mismatch rejection that sets the recovery flag.
//
// Mocks: StubContactStoreReader feeds canned ChangeHistoryResult /
// ContactRecord lists; URLProtocol-mocked PiClient via MockTransport-
// equivalent so we can script cursor + known-ids responses without
// network I/O.
import XCTest
import CRMMacCore
import CRMMacPiClient
@testable import CRMMacIcloudContactsSource

final class ICloudContactsSourcePluginTests: XCTestCase {
    private let testAuth = PiAuth(
        hostID: UUID(uuidString: "11111111-2222-3333-4444-555555555555")!,
        apiKey: "k")
    private let containerA = "AAAAAAAA-1111-2222-3333-444444444444"
    private let containerB = "BBBBBBBB-1111-2222-3333-444444444444"

    // MARK: - reader stub

    private final class StubReader: ContactStoreReader, @unchecked Sendable {
        let containers: [ContainerInfo]
        let fullFetchResult: [ContactRecord]
        let changeHistoryResult: Result<ChangeHistoryResult, Error>
        let currentTokenResult: Result<Data, Error>
        var fullFetchCallCount = 0
        var currentTokenCallCount = 0
        var currentTokenCalledBeforeFullFetch = true
        private var fullFetchEverCalled = false

        init(
            containers: [ContainerInfo] = [],
            fullFetchResult: [ContactRecord] = [],
            changeHistoryResult: Result<ChangeHistoryResult, Error> =
                .success(ChangeHistoryResult(events: [], newToken: Data([0x00]))),
            currentTokenResult: Result<Data, Error> = .success(Data([0x01]))
        ) {
            self.containers = containers
            self.fullFetchResult = fullFetchResult
            self.changeHistoryResult = changeHistoryResult
            self.currentTokenResult = currentTokenResult
        }

        func listContainers() throws -> [ContainerInfo] { containers }

        func fullFetch(containerIdentifiers: [String]) throws -> [ContactRecord] {
            fullFetchCallCount += 1
            fullFetchEverCalled = true
            return fullFetchResult
        }

        func changeHistory(from token: Data?) throws -> ChangeHistoryResult {
            switch changeHistoryResult {
            case .success(let v): return v
            case .failure(let e): throw e
            }
        }

        func currentToken() throws -> Data {
            currentTokenCallCount += 1
            if fullFetchEverCalled {
                currentTokenCalledBeforeFullFetch = false
            }
            switch currentTokenResult {
            case .success(let v): return v
            case .failure(let e): throw e
            }
        }
    }

    // MARK: - auth stub

    private final class StubAuthAdapter: ContactsAuthorizationAdapter, @unchecked Sendable {
        let status: ContactsAuthorizationStatus
        init(_ status: ContactsAuthorizationStatus = .authorized) {
            self.status = status
        }
        func authorizationStatus() -> ContactsAuthorizationStatus { status }
        func requestAccess() async throws -> Bool {
            XCTFail("daemon must NOT call requestAccess; install is the only caller")
            return false
        }
    }

    // MARK: - config stub

    private final class StubConfigSource: ICloudContactsConfigSource, @unchecked Sendable {
        let result: Result<ICloudContactsConfig?, Error>
        init(containers: [String]) {
            self.result = .success(ICloudContactsConfig(containers: containers))
        }
        init(failingWith err: Error) {
            self.result = .failure(err)
        }
        func load() throws -> ICloudContactsConfig? {
            switch result {
            case .success(let v): return v
            case .failure(let e): throw e
            }
        }
    }

    // MARK: - PiClient driver

    fileprivate struct ScriptedPi {
        let cursorGet: SourceCursorState
        let knownIDs: KnownIDsData
        let ingestResult: IngestEventsData
        let cursorCommitThrows: Error?
        let ingestThrows: Error?
    }

    private func buildPiClient(
        _ s: ScriptedPi
    ) -> (PiClient, MockScriptedTransport) {
        let mock = MockScriptedTransport(plan: s)
        let client = PiClient(
            baseURL: URL(string: "https://test.invalid")!,
            transport: mock.asTransport(),
            logger: NoopLogger())
        return (client, mock)
    }

    // Minimal URL-pattern-routing transport so we can script
    // responses by endpoint shape (GET cursor, GET known-ids, POST
    // ingest, POST cursor).
    final class MockScriptedTransport: @unchecked Sendable {
        fileprivate let plan: ScriptedPi
        var lastCommittedCursor: String?
        var commitWasAttempted = false
        var ingestWasAttempted = false

        fileprivate init(plan: ScriptedPi) {
            self.plan = plan
        }

        func asTransport() -> TransportFunc {
            return { request in
                let path = request.url?.path ?? ""
                let method = request.httpMethod ?? "GET"
                if path.hasSuffix("/cursor") && method == "GET" {
                    return (Self.encodeCursorEnvelope(self.plan.cursorGet),
                            Self.ok(url: request.url!))
                }
                if path.hasSuffix("/known-ids") && method == "GET" {
                    return (Self.encodeKnownIDsEnvelope(self.plan.knownIDs),
                            Self.ok(url: request.url!))
                }
                if path.hasSuffix("/ingest/events") && method == "POST" {
                    self.ingestWasAttempted = true
                    if let e = self.plan.ingestThrows { throw e }
                    return (Self.encodeIngestEventsData(self.plan.ingestResult),
                            Self.ok(url: request.url!))
                }
                if path.hasSuffix("/cursor") && method == "POST" {
                    self.commitWasAttempted = true
                    if let body = request.httpBody,
                       let parsed = try? JSONSerialization.jsonObject(with: body) as? [String: Any],
                       let cur = parsed["cursor"] as? String {
                        self.lastCommittedCursor = cur
                    }
                    if let e = self.plan.cursorCommitThrows { throw e }
                    let ok = Data(#"{"success":true,"data":{"ok":true}}"#.utf8)
                    return (ok, Self.ok(url: request.url!))
                }
                throw URLError(.unsupportedURL)
            }
        }

        private static func encodeCursorEnvelope(_ s: SourceCursorState) -> Data {
            let dict: [String: Any] = [
                "success": true,
                "data": [
                    "cursor": s.cursor,
                    "cursor_epoch": s.cursorEpoch,
                    "backfill_complete": s.backfillComplete,
                ],
            ]
            return try! JSONSerialization.data(withJSONObject: dict)
        }

        private static func encodeKnownIDsEnvelope(_ k: KnownIDsData) -> Data {
            let ids: [[String: Any]] = k.ids.map { e in
                var d: [String: Any] = ["source_id": e.sourceID]
                if let h = e.lastContentHash { d["last_content_hash"] = h }
                else { d["last_content_hash"] = NSNull() }
                return d
            }
            let dict: [String: Any] = [
                "success": true,
                "data": ["ids": ids],
            ]
            return try! JSONSerialization.data(withJSONObject: dict)
        }

        private static func encodeIngestEventsData(_ i: IngestEventsData) -> Data {
            let errs: [[String: Any]] = i.errors.map { e in
                ["index": e.index, "code": e.code, "message": e.message]
            }
            let dict: [String: Any] = [
                "accepted": i.accepted,
                "duplicate": i.duplicate,
                "rejected": i.rejected,
                "errors": errs,
            ]
            return try! JSONSerialization.data(withJSONObject: dict)
        }

        private static func ok(url: URL) -> HTTPURLResponse {
            HTTPURLResponse(url: url, statusCode: 200,
                            httpVersion: "HTTP/1.1",
                            headerFields: ["Content-Type": "application/json"])!
        }
    }

    // MARK: - helpers

    private func tempDir() -> URL {
        let url = URL(fileURLWithPath: NSTemporaryDirectory())
            .appendingPathComponent("icloud-plugin-tests-\(UUID().uuidString)")
        try? FileManager.default.createDirectory(at: url, withIntermediateDirectories: true)
        return url
    }

    private func makeRecord(id: String, container: String) -> ContactRecord {
        ContactRecord(
            identifier: id,
            containerIdentifier: container,
            displayName: "Contact \(id)",
            firstName: "Contact",
            lastName: id,
            emails: [],
            phones: [],
            addresses: [])
    }

    private func makePlugin(
        reader: ContactStoreReader,
        authStatus: ContactsAuthorizationStatus = .authorized,
        config: ICloudContactsConfigSource? = nil,
        pi: ScriptedPi,
        cacheURL: URL,
        stateURL: URL,
        publisher: ICloudContactsPublisher? = nil
    ) -> (
        plugin: ICloudContactsSourcePlugin,
        cache: ContactHashCache,
        mutator: StateMutator,
        registry: SourceHealthRegistry,
        transport: MockScriptedTransport
    ) {
        let (piClient, mock) = buildPiClient(pi)
        let cache = ContactHashCache(fileURL: cacheURL)
        let stateStore = StateStore(fileURL: stateURL)
        // Initialize the state file if missing — the plugin's mutator
        // requires an existing state.json (Installer is the production
        // initializer; tests must replicate that prerequisite).
        if !FileManager.default.fileExists(atPath: stateURL.path) {
            try? stateStore.initializeIfMissing()
        }
        let mutator = StateMutator(store: stateStore)
        let registry = SourceHealthRegistry()
        let cfg = config ?? StubConfigSource(containers: [containerA])
        let pub = publisher ?? ICloudContactsPublisher(
            sender: { auth, body in
                try await piClient.ingestEvents(auth: auth, body: body)
            },
            auth: testAuth, logger: NoopLogger())
        let plugin = ICloudContactsSourcePlugin(
            tickInterval: 900,
            piClient: piClient,
            auth: testAuth,
            mutator: mutator,
            publisher: pub,
            cache: cache,
            reader: reader,
            authAdapter: StubAuthAdapter(authStatus),
            configSource: cfg,
            healthRegistry: registry,
            logger: NoopLogger(),
            clock: { Date(timeIntervalSince1970: 1_750_000_000) })
        return (plugin, cache, mutator, registry, mock)
    }

    // MARK: - auth + config edge cases

    func testPermissionDeniedMarksUnhealthyAndNoIngest() async throws {
        let dir = tempDir()
        defer { try? FileManager.default.removeItem(at: dir) }
        let reader = StubReader()
        let pi = ScriptedPi(
            cursorGet: SourceCursorState(cursor: "", cursorEpoch: 0, backfillComplete: false),
            knownIDs: KnownIDsData(ids: []),
            ingestResult: IngestEventsData(accepted: 0, duplicate: 0, rejected: 0, errors: []),
            cursorCommitThrows: nil, ingestThrows: nil)
        let (plugin, _, _, registry, transport) = makePlugin(
            reader: reader, authStatus: .denied, pi: pi,
            cacheURL: dir.appendingPathComponent("cache.json"),
            stateURL: dir.appendingPathComponent("state.json"))
        try await plugin.tick()
        let snap = await registry.read(.icloudContacts)
        XCTAssertEqual(snap?.enabled, false)
        XCTAssertEqual(snap?.lastError, "contacts_permission:denied")
        XCTAssertFalse(transport.ingestWasAttempted)
        XCTAssertFalse(transport.commitWasAttempted)
    }

    func testNotDeterminedMarksUnhealthy() async throws {
        let dir = tempDir()
        defer { try? FileManager.default.removeItem(at: dir) }
        let pi = ScriptedPi(
            cursorGet: SourceCursorState(cursor: "", cursorEpoch: 0, backfillComplete: false),
            knownIDs: KnownIDsData(ids: []),
            ingestResult: IngestEventsData(accepted: 0, duplicate: 0, rejected: 0, errors: []),
            cursorCommitThrows: nil, ingestThrows: nil)
        let (plugin, _, _, registry, _) = makePlugin(
            reader: StubReader(), authStatus: .notDetermined, pi: pi,
            cacheURL: dir.appendingPathComponent("cache.json"),
            stateURL: dir.appendingPathComponent("state.json"))
        try await plugin.tick()
        let snap = await registry.read(.icloudContacts)
        XCTAssertEqual(snap?.lastError, "contacts_permission:notDetermined")
    }

    func testNoContainersMarksUnhealthy() async throws {
        let dir = tempDir()
        defer { try? FileManager.default.removeItem(at: dir) }
        let pi = ScriptedPi(
            cursorGet: SourceCursorState(cursor: "", cursorEpoch: 0, backfillComplete: false),
            knownIDs: KnownIDsData(ids: []),
            ingestResult: IngestEventsData(accepted: 0, duplicate: 0, rejected: 0, errors: []),
            cursorCommitThrows: nil, ingestThrows: nil)
        let (plugin, _, _, registry, _) = makePlugin(
            reader: StubReader(),
            config: StubConfigSource(containers: []),
            pi: pi,
            cacheURL: dir.appendingPathComponent("cache.json"),
            stateURL: dir.appendingPathComponent("state.json"))
        try await plugin.tick()
        let snap = await registry.read(.icloudContacts)
        XCTAssertEqual(snap?.lastError, "no_containers_configured")
    }

    // MARK: - first-run path

    func testFirstRunCapturesTokenBeforeSnapshot() async throws {
        // Critical invariant: currentToken() must be called BEFORE
        // fullFetch() so any contact edited mid-snapshot is caught
        // by the next delta tick.
        let dir = tempDir()
        defer { try? FileManager.default.removeItem(at: dir) }
        let reader = StubReader(
            fullFetchResult: [makeRecord(id: "id-1", container: containerA)],
            currentTokenResult: .success(Data([0xAB, 0xCD])))
        let pi = ScriptedPi(
            cursorGet: SourceCursorState(cursor: "", cursorEpoch: 0, backfillComplete: false),
            knownIDs: KnownIDsData(ids: []),
            ingestResult: IngestEventsData(accepted: 1, duplicate: 0, rejected: 0, errors: []),
            cursorCommitThrows: nil, ingestThrows: nil)
        let (plugin, _, _, _, transport) = makePlugin(
            reader: reader, pi: pi,
            cacheURL: dir.appendingPathComponent("cache.json"),
            stateURL: dir.appendingPathComponent("state.json"))
        try await plugin.tick()
        XCTAssertTrue(reader.currentTokenCalledBeforeFullFetch,
                      "token must be captured BEFORE full fetch")
        XCTAssertTrue(transport.ingestWasAttempted)
        XCTAssertTrue(transport.commitWasAttempted)
        XCTAssertEqual(transport.lastCommittedCursor,
                       Data([0xAB, 0xCD]).base64EncodedString())
    }

    func testFirstRunFiltersByContainerAllowlist() async throws {
        let dir = tempDir()
        defer { try? FileManager.default.removeItem(at: dir) }
        let allowedRecord = makeRecord(id: "id-1", container: containerA)
        let blockedRecord = makeRecord(id: "id-2", container: containerB)
        let reader = StubReader(
            fullFetchResult: [allowedRecord, blockedRecord])
        let pi = ScriptedPi(
            cursorGet: SourceCursorState(cursor: "", cursorEpoch: 0, backfillComplete: false),
            knownIDs: KnownIDsData(ids: []),
            ingestResult: IngestEventsData(accepted: 1, duplicate: 0, rejected: 0, errors: []),
            cursorCommitThrows: nil, ingestThrows: nil)
        actor BodyCapture {
            var bodies: [IngestEventsBody] = []
            func record(_ b: IngestEventsBody) { bodies.append(b) }
            func snapshot() -> [IngestEventsBody] { bodies }
        }
        let capture = BodyCapture()
        let pub = ICloudContactsPublisher(
            sender: { _, body in
                await capture.record(body)
                return IngestEventsData(
                    accepted: body.events.count, duplicate: 0,
                    rejected: 0, errors: [])
            },
            auth: testAuth, logger: NoopLogger())
        let (plugin, _, _, _, _) = makePlugin(
            reader: reader,
            config: StubConfigSource(containers: [containerA]),
            pi: pi,
            cacheURL: dir.appendingPathComponent("cache.json"),
            stateURL: dir.appendingPathComponent("state.json"),
            publisher: pub)
        try await plugin.tick()
        let bodies = await capture.snapshot()
        XCTAssertEqual(bodies.count, 1)
        XCTAssertEqual(bodies.first?.events.count, 1,
                       "only the allowed-container record should publish")
        XCTAssertEqual(bodies.first?.events.first?.sourceID.hasPrefix("id-1@"), true)
    }

    func testFirstRunPopulatesCacheForUpserts() async throws {
        let dir = tempDir()
        defer { try? FileManager.default.removeItem(at: dir) }
        let reader = StubReader(
            fullFetchResult: [makeRecord(id: "id-1", container: containerA)])
        let pi = ScriptedPi(
            cursorGet: SourceCursorState(cursor: "", cursorEpoch: 0, backfillComplete: false),
            knownIDs: KnownIDsData(ids: []),
            ingestResult: IngestEventsData(accepted: 1, duplicate: 0, rejected: 0, errors: []),
            cursorCommitThrows: nil, ingestThrows: nil)
        let cacheURL = dir.appendingPathComponent("cache.json")
        let (plugin, cache, _, _, _) = makePlugin(
            reader: reader, pi: pi,
            cacheURL: cacheURL,
            stateURL: dir.appendingPathComponent("state.json"))
        try await plugin.tick()
        let priorHash = await cache.get("id-1")
        XCTAssertNotNil(priorHash, "cache should remember the upsert hash")
    }

    func testFirstRunStateLossGuardRoutesToRecovery() async throws {
        // Cursor empty AND /known-ids non-empty → state-loss; route
        // to recovery so tombstones get reconciled.
        let dir = tempDir()
        defer { try? FileManager.default.removeItem(at: dir) }
        // Scan returns id-1 only; Pi has id-2 in known-ids → tombstone id-2.
        let reader = StubReader(
            fullFetchResult: [makeRecord(id: "id-1", container: containerA)])
        let pi = ScriptedPi(
            cursorGet: SourceCursorState(cursor: "", cursorEpoch: 0, backfillComplete: false),
            knownIDs: KnownIDsData(ids: [
                KnownContactID(sourceID: "id-2@oldhash", lastContentHash: "oldhash"),
            ]),
            ingestResult: IngestEventsData(accepted: 2, duplicate: 0, rejected: 0, errors: []),
            cursorCommitThrows: nil, ingestThrows: nil)
        actor BodyCapture {
            var bodies: [IngestEventsBody] = []
            func record(_ b: IngestEventsBody) { bodies.append(b) }
            func snapshot() -> [IngestEventsBody] { bodies }
        }
        let capture = BodyCapture()
        let pub = ICloudContactsPublisher(
            sender: { _, body in
                await capture.record(body)
                return IngestEventsData(
                    accepted: body.events.count, duplicate: 0,
                    rejected: 0, errors: [])
            },
            auth: testAuth, logger: NoopLogger())
        let (plugin, _, _, _, _) = makePlugin(
            reader: reader, pi: pi,
            cacheURL: dir.appendingPathComponent("cache.json"),
            stateURL: dir.appendingPathComponent("state.json"),
            publisher: pub)
        try await plugin.tick()
        let bodies = await capture.snapshot()
        let kinds = bodies.flatMap { $0.events.map(\.kind) }
        XCTAssertTrue(kinds.contains("external_contact.deleted"),
                      "state-loss path must emit tombstone for id-2")
        XCTAssertTrue(kinds.contains("external_contact.upserted"),
                      "state-loss path must emit upsert for id-1")
    }

    // MARK: - delta path

    func testDeltaUnknownEventFailsClosedAndSetsRecoveryFlag() async throws {
        let dir = tempDir()
        defer { try? FileManager.default.removeItem(at: dir) }
        let reader = StubReader(
            changeHistoryResult: .success(ChangeHistoryResult(
                events: [.unknown(rawEventDescription: "WeirdEvent")],
                newToken: Data([0xFF]))))
        let pi = ScriptedPi(
            cursorGet: SourceCursorState(
                cursor: Data([0xAA]).base64EncodedString(),
                cursorEpoch: 0, backfillComplete: true),
            knownIDs: KnownIDsData(ids: []),
            ingestResult: IngestEventsData(accepted: 0, duplicate: 0, rejected: 0, errors: []),
            cursorCommitThrows: nil, ingestThrows: nil)
        let stateURL = dir.appendingPathComponent("state.json")
        let (plugin, _, mutator, registry, transport) = makePlugin(
            reader: reader, pi: pi,
            cacheURL: dir.appendingPathComponent("cache.json"),
            stateURL: stateURL)
        try await plugin.tick()
        XCTAssertFalse(transport.ingestWasAttempted,
                       "unknown event must short-circuit before publish")
        XCTAssertFalse(transport.commitWasAttempted,
                       "unknown event must not advance cursor")
        let snap = await registry.read(.icloudContacts)
        XCTAssertEqual(snap?.lastError, "unknown_change_event")
        let state = try await mutator.read()
        XCTAssertTrue(
            state.sources["icloud_contacts"]?.lastError?
                .hasPrefix("recovery_requested:") == true,
            "unknown event must set the recovery flag")
    }

    func testDeltaFiltersChangesByContainerAllowlist() async throws {
        let dir = tempDir()
        defer { try? FileManager.default.removeItem(at: dir) }
        let allowed = makeRecord(id: "id-1", container: containerA)
        let blocked = makeRecord(id: "id-2", container: containerB)
        let reader = StubReader(
            changeHistoryResult: .success(ChangeHistoryResult(
                events: [.add(allowed), .add(blocked)],
                newToken: Data([0xDE, 0xAD]))))
        let pi = ScriptedPi(
            cursorGet: SourceCursorState(
                cursor: Data([0xAA]).base64EncodedString(),
                cursorEpoch: 0, backfillComplete: true),
            knownIDs: KnownIDsData(ids: []),
            ingestResult: IngestEventsData(accepted: 1, duplicate: 0, rejected: 0, errors: []),
            cursorCommitThrows: nil, ingestThrows: nil)
        actor BodyCapture {
            var bodies: [IngestEventsBody] = []
            func record(_ b: IngestEventsBody) { bodies.append(b) }
            func snapshot() -> [IngestEventsBody] { bodies }
        }
        let capture = BodyCapture()
        let pub = ICloudContactsPublisher(
            sender: { _, body in
                await capture.record(body)
                return IngestEventsData(
                    accepted: body.events.count, duplicate: 0,
                    rejected: 0, errors: [])
            },
            auth: testAuth, logger: NoopLogger())
        let (plugin, _, _, _, _) = makePlugin(
            reader: reader, pi: pi,
            cacheURL: dir.appendingPathComponent("cache.json"),
            stateURL: dir.appendingPathComponent("state.json"),
            publisher: pub)
        try await plugin.tick()
        let bodies = await capture.snapshot()
        XCTAssertEqual(bodies.first?.events.count, 1,
                       "only the allowlisted container's change event should publish")
        XCTAssertTrue(bodies.first?.events.first?.sourceID.hasPrefix("id-1@") ?? false)
    }

    func testDeltaDeleteEmitsUnconditionallyEvenWithoutContainerInfo() async throws {
        // .delete events don't carry container info; emit
        // unconditionally per the plan. Pi tombstones by source_id
        // and no-ops if the entity was never allowlisted.
        let dir = tempDir()
        defer { try? FileManager.default.removeItem(at: dir) }
        let reader = StubReader(
            changeHistoryResult: .success(ChangeHistoryResult(
                events: [.delete(identifier: "id-99")],
                newToken: Data([0xDE]))))
        let pi = ScriptedPi(
            cursorGet: SourceCursorState(
                cursor: Data([0xAA]).base64EncodedString(),
                cursorEpoch: 0, backfillComplete: true),
            knownIDs: KnownIDsData(ids: []),
            ingestResult: IngestEventsData(accepted: 1, duplicate: 0, rejected: 0, errors: []),
            cursorCommitThrows: nil, ingestThrows: nil)
        actor BodyCapture {
            var bodies: [IngestEventsBody] = []
            func record(_ b: IngestEventsBody) { bodies.append(b) }
            func snapshot() -> [IngestEventsBody] { bodies }
        }
        let capture = BodyCapture()
        let pub = ICloudContactsPublisher(
            sender: { _, body in
                await capture.record(body)
                return IngestEventsData(
                    accepted: body.events.count, duplicate: 0,
                    rejected: 0, errors: [])
            },
            auth: testAuth, logger: NoopLogger())
        let (plugin, _, _, _, _) = makePlugin(
            reader: reader, pi: pi,
            cacheURL: dir.appendingPathComponent("cache.json"),
            stateURL: dir.appendingPathComponent("state.json"),
            publisher: pub)
        try await plugin.tick()
        let bodies = await capture.snapshot()
        XCTAssertEqual(bodies.first?.events.first?.kind,
                       "external_contact.deleted",
                       ".delete must emit despite unknown container")
        XCTAssertEqual(bodies.first?.events.first?.sourceID,
                       "id-99@deleted@unknown",
                       ".delete without prior hash uses @unknown sentinel")
    }

    // MARK: - recovery path triggered by recovery flag

    func testRecoveryFlagRoutesToRecoveryAndClearsOnSuccess() async throws {
        let dir = tempDir()
        defer { try? FileManager.default.removeItem(at: dir) }
        // Set recovery flag in state.
        let stateURL = dir.appendingPathComponent("state.json")
        let store = StateStore(fileURL: stateURL)
        var preState = DaemonState()
        preState.sources["icloud_contacts"] = SourceState(
            lastError: "recovery_requested:test_seed")
        try store.save(preState)

        let reader = StubReader(
            fullFetchResult: [makeRecord(id: "id-1", container: containerA)])
        let pi = ScriptedPi(
            // Cursor is non-empty; recovery flag overrides cursor.
            cursorGet: SourceCursorState(
                cursor: Data([0xAA]).base64EncodedString(),
                cursorEpoch: 0, backfillComplete: true),
            knownIDs: KnownIDsData(ids: []),
            ingestResult: IngestEventsData(accepted: 1, duplicate: 0, rejected: 0, errors: []),
            cursorCommitThrows: nil, ingestThrows: nil)
        let (plugin, _, mutator, _, _) = makePlugin(
            reader: reader, pi: pi,
            cacheURL: dir.appendingPathComponent("cache.json"),
            stateURL: stateURL)
        try await plugin.tick()
        let state = try await mutator.read()
        XCTAssertNil(state.sources["icloud_contacts"]?.lastError,
                     "successful recovery clears the flag")
    }

    // MARK: - cursor commit failure path

    func testCursorCommitFailureDoesNotAdvanceCursorOrCommitRemovals() async throws {
        let dir = tempDir()
        defer { try? FileManager.default.removeItem(at: dir) }
        let cacheURL = dir.appendingPathComponent("cache.json")
        // Seed the cache so the test can verify NO removal happened.
        let cache = ContactHashCache(fileURL: cacheURL)
        try await cache.applyUpdates(["id-1": "seedhash"])

        let reader = StubReader(
            changeHistoryResult: .success(ChangeHistoryResult(
                events: [.delete(identifier: "id-1")],
                newToken: Data([0xDE]))))
        let pi = ScriptedPi(
            cursorGet: SourceCursorState(
                cursor: Data([0xAA]).base64EncodedString(),
                cursorEpoch: 0, backfillComplete: true),
            knownIDs: KnownIDsData(ids: []),
            ingestResult: IngestEventsData(accepted: 1, duplicate: 0, rejected: 0, errors: []),
            cursorCommitThrows: URLError(.networkConnectionLost),
            ingestThrows: nil)
        let (piClient, transport) = buildPiClient(pi)
        let stateURL = dir.appendingPathComponent("state.json")
        let stateStore = StateStore(fileURL: stateURL)
        try stateStore.initializeIfMissing()
        let mutator = StateMutator(store: stateStore)
        let registry = SourceHealthRegistry()
        let pub = ICloudContactsPublisher(
            sender: { auth, body in
                try await piClient.ingestEvents(auth: auth, body: body)
            },
            auth: testAuth, logger: NoopLogger())
        let plugin = ICloudContactsSourcePlugin(
            tickInterval: 900,
            piClient: piClient, auth: testAuth, mutator: mutator,
            publisher: pub, cache: cache,
            reader: reader, authAdapter: StubAuthAdapter(),
            configSource: StubConfigSource(containers: [containerA]),
            healthRegistry: registry, logger: NoopLogger(),
            clock: { Date(timeIntervalSince1970: 1_750_000_000) })
        try await plugin.tick()
        XCTAssertTrue(transport.commitWasAttempted,
                      "publish succeeded; cursor commit was attempted")
        // The seed hash for id-1 must STILL be present (stage was
        // discarded because cursor commit threw).
        let stillThere = await cache.get("id-1")
        XCTAssertEqual(stillThere, "seedhash",
                       "removal must NOT be finalized when cursor commit fails")
    }

    // MARK: - rejection sets recovery flag

    // MARK: - token-invalid recovery

    func testTokenInvalidRoutesToRecoveryAndCommits() async throws {
        let dir = tempDir()
        defer { try? FileManager.default.removeItem(at: dir) }
        // Reader throws .tokenInvalid on the delta walk → plugin
        // routes to recovery (which calls /known-ids + fullFetch).
        let reader = StubReader(
            fullFetchResult: [makeRecord(id: "id-1", container: containerA)],
            changeHistoryResult: .failure(CNContactStoreReaderError.tokenInvalid(
                underlying: "synthetic")))
        let pi = ScriptedPi(
            cursorGet: SourceCursorState(
                cursor: Data([0xAA]).base64EncodedString(),
                cursorEpoch: 0, backfillComplete: true),
            knownIDs: KnownIDsData(ids: [
                KnownContactID(sourceID: "id-2@oldhash", lastContentHash: "oldhash"),
            ]),
            ingestResult: IngestEventsData(accepted: 2, duplicate: 0, rejected: 0, errors: []),
            cursorCommitThrows: nil, ingestThrows: nil)
        let (plugin, _, _, _, transport) = makePlugin(
            reader: reader, pi: pi,
            cacheURL: dir.appendingPathComponent("cache.json"),
            stateURL: dir.appendingPathComponent("state.json"))
        try await plugin.tick()
        XCTAssertTrue(transport.ingestWasAttempted,
                      "token-invalid recovery still publishes the scan + tombstones")
        XCTAssertTrue(transport.commitWasAttempted)
    }

    // MARK: - mixed delta event sequence

    func testDeltaMixedAddUpdateDeleteAllEmit() async throws {
        let dir = tempDir()
        defer { try? FileManager.default.removeItem(at: dir) }
        let r1 = makeRecord(id: "id-1", container: containerA)
        let r2 = makeRecord(id: "id-2", container: containerA)
        let reader = StubReader(
            changeHistoryResult: .success(ChangeHistoryResult(
                events: [.add(r1), .update(r2), .delete(identifier: "id-3")],
                newToken: Data([0xDE]))))
        let pi = ScriptedPi(
            cursorGet: SourceCursorState(
                cursor: Data([0xAA]).base64EncodedString(),
                cursorEpoch: 0, backfillComplete: true),
            knownIDs: KnownIDsData(ids: []),
            ingestResult: IngestEventsData(accepted: 3, duplicate: 0, rejected: 0, errors: []),
            cursorCommitThrows: nil, ingestThrows: nil)
        actor BodyCapture {
            var bodies: [IngestEventsBody] = []
            func record(_ b: IngestEventsBody) { bodies.append(b) }
            func snapshot() -> [IngestEventsBody] { bodies }
        }
        let capture = BodyCapture()
        let pub = ICloudContactsPublisher(
            sender: { _, body in
                await capture.record(body)
                return IngestEventsData(
                    accepted: body.events.count, duplicate: 0,
                    rejected: 0, errors: [])
            },
            auth: testAuth, logger: NoopLogger())
        let (plugin, _, _, _, _) = makePlugin(
            reader: reader, pi: pi,
            cacheURL: dir.appendingPathComponent("cache.json"),
            stateURL: dir.appendingPathComponent("state.json"),
            publisher: pub)
        try await plugin.tick()
        let bodies = await capture.snapshot()
        XCTAssertEqual(bodies.first?.events.count, 3)
        let kinds = bodies.first?.events.map(\.kind) ?? []
        XCTAssertEqual(kinds.filter { $0 == "external_contact.upserted" }.count, 2)
        XCTAssertEqual(kinds.filter { $0 == "external_contact.deleted" }.count, 1)
    }

    // MARK: - same-tick delete then add

    func testSameTickDeleteThenAddPreservesUpsertInCache() async throws {
        // Event order: [.delete("id-X"), .add(id-X)] — the cache
        // must end this tick with the NEW hash for id-X (the add)
        // and NOT drop the entry on commit. This exercises the
        // plugin's event-order processing AND the cache actor's
        // applyUpdates-cancels-stagedRemoval contract.
        let dir = tempDir()
        defer { try? FileManager.default.removeItem(at: dir) }
        let record = makeRecord(id: "id-X", container: containerA)
        let reader = StubReader(
            changeHistoryResult: .success(ChangeHistoryResult(
                events: [.delete(identifier: "id-X"), .add(record)],
                newToken: Data([0xAB]))))
        let pi = ScriptedPi(
            cursorGet: SourceCursorState(
                cursor: Data([0xAA]).base64EncodedString(),
                cursorEpoch: 0, backfillComplete: true),
            knownIDs: KnownIDsData(ids: []),
            ingestResult: IngestEventsData(accepted: 2, duplicate: 0, rejected: 0, errors: []),
            cursorCommitThrows: nil, ingestThrows: nil)
        let (plugin, cache, _, _, transport) = makePlugin(
            reader: reader, pi: pi,
            cacheURL: dir.appendingPathComponent("cache.json"),
            stateURL: dir.appendingPathComponent("state.json"))
        try await plugin.tick()
        XCTAssertTrue(transport.commitWasAttempted)
        // The add wrote a new hash for id-X via applyUpdates, which
        // also cancels the staged removal. After commit, the live
        // map still has id-X with its new hash.
        let still = await cache.get("id-X")
        XCTAssertNotNil(still, "same-tick delete→add must leave id-X in the cache")
    }

    // MARK: - cache write fails

    func testCacheWriteFailureAbortsBeforeCursorCommit() async throws {
        // Failure mode: applyUpdates(_:) throws (e.g. disk full).
        // The plugin must mark unhealthy + abort BEFORE attempting
        // the cursor commit; the next tick replays from the unmoved
        // cursor.
        let dir = tempDir()
        defer { try? FileManager.default.removeItem(at: dir) }
        // Force the cache file path to be unwritable by making the
        // parent path a regular file.
        let blockedParent = dir.appendingPathComponent("blocked")
        try Data("blocker".utf8).write(to: blockedParent)
        let cacheURL = blockedParent.appendingPathComponent("nested/cache.json")
        let reader = StubReader(
            changeHistoryResult: .success(ChangeHistoryResult(
                events: [.add(makeRecord(id: "id-1", container: containerA))],
                newToken: Data([0xDE]))))
        let pi = ScriptedPi(
            cursorGet: SourceCursorState(
                cursor: Data([0xAA]).base64EncodedString(),
                cursorEpoch: 0, backfillComplete: true),
            knownIDs: KnownIDsData(ids: []),
            ingestResult: IngestEventsData(accepted: 1, duplicate: 0, rejected: 0, errors: []),
            cursorCommitThrows: nil, ingestThrows: nil)
        let (plugin, _, _, registry, transport) = makePlugin(
            reader: reader, pi: pi,
            cacheURL: cacheURL,
            stateURL: dir.appendingPathComponent("state.json"))
        try await plugin.tick()
        XCTAssertFalse(transport.ingestWasAttempted,
                       "publish must not run when the cache write upstream of it fails")
        XCTAssertFalse(transport.commitWasAttempted,
                       "cursor commit must not run when the cache write upstream of it fails")
        let snap = await registry.read(.icloudContacts)
        XCTAssertEqual(snap?.lastError, "cache_write_failed")
    }

    // MARK: - hash-mismatch rejection

    func testHashMismatchRejectionSetsRecoveryFlagAndHoldsCursor() async throws {
        let dir = tempDir()
        defer { try? FileManager.default.removeItem(at: dir) }
        let reader = StubReader(
            changeHistoryResult: .success(ChangeHistoryResult(
                events: [.add(makeRecord(id: "id-1", container: containerA))],
                newToken: Data([0xDE]))))
        let pi = ScriptedPi(
            cursorGet: SourceCursorState(
                cursor: Data([0xAA]).base64EncodedString(),
                cursorEpoch: 0, backfillComplete: true),
            knownIDs: KnownIDsData(ids: []),
            ingestResult: IngestEventsData(
                accepted: 0, duplicate: 0, rejected: 1,
                errors: [IngestEventError(
                    index: 0, code: "EXTERNAL_CONTACT_HASH_MISMATCH",
                    message: "bad")]),
            cursorCommitThrows: nil, ingestThrows: nil)
        let stateURL = dir.appendingPathComponent("state.json")
        let (plugin, _, mutator, _, transport) = makePlugin(
            reader: reader, pi: pi,
            cacheURL: dir.appendingPathComponent("cache.json"),
            stateURL: stateURL)
        try await plugin.tick()
        XCTAssertFalse(transport.commitWasAttempted,
                       "per-event rejection holds cursor")
        let state = try await mutator.read()
        XCTAssertTrue(
            state.sources["icloud_contacts"]?.lastError?
                .hasPrefix("recovery_requested:") == true,
            "hash mismatch must set recovery flag for next tick")
    }
}
