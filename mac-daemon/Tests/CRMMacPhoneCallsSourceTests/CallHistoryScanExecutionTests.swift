// CallHistoryScanExecutionTests — Phase A (durable scan enqueue) +
// Phase B (resumable scan execution) end-to-end against a fake
// PiClient, an on-disk CallHistoryDB the plugin opens itself, and a
// real StateMutator over a temp StateStore.
//
// Synthetic handles only (+15550000001, test@example.com); no real PII.
import XCTest
import Foundation
import GRDB
import CRMMacCore
import CRMMacPiClient
@testable import CRMMacPhoneCallsSource

final class CallHistoryScanExecutionTests: XCTestCase {
    private let auth = PiAuth(
        hostID: UUID(uuidString: "11111111-2222-3333-4444-555555555555")!,
        apiKey: "k")
    private let backfillFloor = PhoneCallsCursorWire.defaultBackfillFloor
    // A fixed clock so the 30-day window is deterministic. 2026-05-20.
    private let fixedNow = Date(timeIntervalSince1970: 1_779_000_000)
    private let scannedUnix: TimeInterval = 1_777_680_000 // 2026-05-02
    private var tempDir: URL!

    /// Production-parity canonicalizer injected into the plugin + the
    /// scan reader (phone -> E.164, email -> lowercased).
    private let canonicalize: @Sendable (String) -> String = { raw in
        let trimmed = raw.trimmingCharacters(in: .whitespacesAndNewlines)
        if trimmed.isEmpty { return "" }
        if trimmed.contains("@") {
            return NormalizationParity.normalizeEmail(trimmed)
        }
        return NormalizationParity.normalizePhoneE164(trimmed)
    }

    override func setUpWithError() throws {
        try super.setUpWithError()
        tempDir = FileManager.default.temporaryDirectory
            .appendingPathComponent("crm-mac-phone-scan-exec-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: tempDir, withIntermediateDirectories: true)
    }

    override func tearDownWithError() throws {
        try? FileManager.default.removeItem(at: tempDir)
        try super.tearDownWithError()
    }

    private func makeStateStore() -> StateStore {
        StateStore(fileURL: tempDir.appendingPathComponent("state.json"))
    }

    /// A cursor with backfill complete + live cursor past every seeded
    /// call, so the row-emitting batches are no-ops and ONLY the scan
    /// publishes. Lets the scan-execution assertions isolate Phase B.
    private func inactiveBatchCursor(
        pendingScans: [PhoneCallsCursorPendingScan] = []
    ) -> PhoneCallsCursor {
        let highZDate = InMemoryCallHistoryDB.appleEpochSeconds(unix: fixedNow.timeIntervalSince1970)
        return PhoneCallsCursor(
            backfillCursorZDate: 0,
            backfillCursorZPK: 0,
            liveCursorZDate: highZDate,
            liveCursorZPK: 1_000_000,
            installMaxZDate: highZDate,
            installMaxZPK: 1_000_000,
            backfillFloorSentAt: backfillFloor,
            backfillComplete: true,
            pendingScans: pendingScans)
    }

    /// Build an on-disk CallHistoryDB (full fixture schema, so the
    /// plugin's schema validator passes) with `count` calls under one
    /// handle at consecutive Z_PKs starting at 100, all at `scannedUnix`
    /// + i seconds.
    private func makeCallHistoryDB(callCount: Int, handle: String = "+15550000001") throws -> URL {
        let dbURL = tempDir.appendingPathComponent("CallHistory.storedata")
        let bundle = Bundle.module
        guard let scriptURL = bundle.url(forResource: "call_history_db_schema",
                                          withExtension: "sql",
                                          subdirectory: "Fixtures") else {
            throw XCTSkip("call_history_db_schema.sql not in test bundle")
        }
        let script = try String(contentsOf: scriptURL, encoding: .utf8)
        let queue = try DatabaseQueue(path: dbURL.path)
        try queue.write { db in
            try db.execute(sql: script)
            for i in 0..<callCount {
                let zPK = 100 + i
                let zdate = InMemoryCallHistoryDB.appleEpochSeconds(
                    unix: scannedUnix + TimeInterval(i))
                try db.execute(sql: """
                    INSERT INTO ZCALLRECORD (
                        Z_PK, ZUNIQUE_ID, ZDATE, ZADDRESS,
                        ZORIGINATED, ZANSWERED, ZDURATION,
                        ZSERVICE_PROVIDER, ZCALLTYPE, ZHASMESSAGE)
                    VALUES (?, ?, ?, ?, 0, 1, 30, 'com.apple.Telephony', 0, 0)
                    """,
                    arguments: [zPK, "u\(zPK)", zdate, handle])
            }
        }
        return dbURL
    }

    private func makePlugin(
        dbURL: URL,
        store: StateStore,
        cache: KnownIdentifiersCache,
        transport: PhoneStatefulCursorTransport,
        publisherSink: PhonePublisherSink,
        maxRowsPerTick: Int = 500
    ) -> PhoneCallsSourcePlugin {
        let now = fixedNow
        let piClient = PiClient(
            baseURL: URL(string: "https://pi.example.test")!,
            transport: transport.asTransport(),
            sleep: { _ in })
        let publisher = PhoneCallsPublisher(
            sender: { _, body in
                await publisherSink.record(body.events)
                return IngestEventsData(
                    accepted: body.events.count, duplicate: 0, rejected: 0, errors: [])
            },
            auth: auth, logger: NoopLogger())
        return PhoneCallsSourcePlugin(
            tickInterval: 60,
            config: PhoneCallsSourceConfig(
                callHistoryDBPath: dbURL,
                backfillFloor: backfillFloor,
                maxRowsPerTick: maxRowsPerTick),
            piClient: piClient,
            auth: auth,
            mutator: StateMutator(store: store),
            publisher: publisher,
            cache: cache,
            canonicalizer: canonicalize,
            heartbeatStateProvider: InMemoryHeartbeatStateProvider(initial: 2),
            healthRegistry: SourceHealthRegistry(),
            logger: NoopLogger(),
            clock: { now })
    }

    // MARK: - Phase A durability + scan execution

    func testNewlyKnownEnqueuesDurablyThenScansAndPublishes() async throws {
        let dbURL = try makeCallHistoryDB(callCount: 1)
        let store = makeStateStore()
        let seeded = inactiveBatchCursor()
        try store.save(DaemonState(schemaVersion: 1))
        let cache = KnownIdentifiersCache(
            baselines: [.phoneCalls: []], consumers: [.phoneCalls])
        // A heartbeat fetch surfaces the new identifier.
        await cache.replace(with: ["+15550000001"])

        let transport = PhoneStatefulCursorTransport(
            initialCursor: try PhoneCallsCursorCodec.encode(seeded))
        let sink = PhonePublisherSink()
        let plugin = makePlugin(dbURL: dbURL, store: store, cache: cache,
                                transport: transport, publisherSink: sink)

        try await plugin.tick()

        // The cursor was committed with the pendingScans entry BEFORE any
        // scan row published (Phase A durability).
        let firstEnqueueIndex = transport.firstCommitIndexContaining("+15550000001")
        XCTAssertNotNil(firstEnqueueIndex, "a committed cursor must carry the enqueued scan")

        // The scanned call published with the right kind/source_id.
        let events = await sink.allEvents()
        XCTAssertTrue(events.contains { $0.sourceID == "u100" && $0.kind == "call.received" },
                      "scanned call published")

        // The source tick did NOT write the persisted baseline.
        let state = try store.load()
        XCTAssertNil(state.knownIdentifierBaselines,
                     "source tick must not write knownIdentifierBaselines")

        // After a fully-exhausted scan, the final committed cursor has an
        // empty pendingScans (the entry dequeued).
        let finalCursor = transport.currentDecodedCursor()
        XCTAssertEqual(finalCursor?.pendingScans.count, 0,
                       "exhausted scan dequeued")
    }

    // MARK: - Phase A failure rolls back

    func testPhaseAFailureReturnsIdentifierForReEnqueue() async throws {
        let dbURL = try makeCallHistoryDB(callCount: 1)
        let store = makeStateStore()
        try store.save(DaemonState(schemaVersion: 1))
        let cache = KnownIdentifiersCache(
            baselines: [.phoneCalls: []], consumers: [.phoneCalls])
        await cache.replace(with: ["+15550000001"])

        // Transport fails every commit → Phase A cannot persist.
        let transport = PhoneStatefulCursorTransport(failCommits: true)
        let sink = PhonePublisherSink()
        let plugin = makePlugin(dbURL: dbURL, store: store, cache: cache,
                                transport: transport, publisherSink: sink)

        try await plugin.tick()

        // No scan executed (Phase A aborted before Phase B).
        let events = await sink.allEvents()
        XCTAssertTrue(events.isEmpty, "no scan rows published when Phase A fails")

        // The identifier was returned to the cache → a subsequent drain
        // re-returns it for re-enqueue.
        let reDrained = await cache.drainNewlyAdded(for: .phoneCalls)
        XCTAssertEqual(reDrained, ["+15550000001"], "identifier returned for re-enqueue")
    }

    // MARK: - resumable / larger-than-budget

    func testResumableScanAcrossTicksDropsNoRows() async throws {
        // 5 matching calls, budget 2 → 3 ticks to fully walk.
        let dbURL = try makeCallHistoryDB(callCount: 5)
        let store = makeStateStore()
        let seeded = inactiveBatchCursor()
        try store.save(DaemonState(schemaVersion: 1))
        let cache = KnownIdentifiersCache(
            baselines: [.phoneCalls: []], consumers: [.phoneCalls])
        await cache.replace(with: ["+15550000001"])

        let transport = PhoneStatefulCursorTransport(
            initialCursor: try PhoneCallsCursorCodec.encode(seeded))
        let sink = PhonePublisherSink()
        let plugin = makePlugin(dbURL: dbURL, store: store, cache: cache,
                                transport: transport, publisherSink: sink,
                                maxRowsPerTick: 2)

        for _ in 0..<3 {
            try await plugin.tick()
        }

        let scanned = await sink.scannedSourceIDs()
        XCTAssertEqual(scanned, Set((100...104).map { "u\($0)" }),
                       "every scanned row published across the multi-tick walk")
        let finalCursor = transport.currentDecodedCursor()
        XCTAssertEqual(finalCursor?.pendingScans.count, 0, "scan dequeued once exhausted")
    }

    // MARK: - membership gate (hasFetched vs isPopulated)

    func testUnknownHandleDroppedWhenFetched() async throws {
        let dbURL = try makeCallHistoryDB(callCount: 1, handle: "+15550000001")
        let store = makeStateStore()
        let seededCursor = inactiveBatchCursor(pendingScans: [
            PhoneCallsCursorPendingScan(
                normalizedHandle: "+15557777777",
                since: Date(timeIntervalSince1970: scannedUnix - 86_400)),
        ])
        var st = DaemonState(schemaVersion: 1)
        st.sources["phone_calls"] = SourceState(cursor: try PhoneCallsCursorCodec.encode(seededCursor))
        try store.save(st)

        let cache = KnownIdentifiersCache(
            baselines: [.phoneCalls: []], consumers: [.phoneCalls])
        // Empty CRM but FETCHED.
        await cache.replace(with: [])

        let transport = PhoneStatefulCursorTransport(
            initialCursor: try PhoneCallsCursorCodec.encode(seededCursor))
        let sink = PhonePublisherSink()
        let plugin = makePlugin(dbURL: dbURL, store: store, cache: cache,
                                transport: transport, publisherSink: sink)

        try await plugin.tick()

        let events = await sink.allEvents()
        XCTAssertTrue(events.isEmpty, "unknown-handle scan publishes nothing")
        let finalCursor = transport.currentDecodedCursor()
        XCTAssertEqual(finalCursor?.pendingScans.count, 0, "unknown-handle entry dropped")
    }

    func testUnknownHandleDeferredWhenNotFetched() async throws {
        let dbURL = try makeCallHistoryDB(callCount: 1, handle: "+15550000001")
        let store = makeStateStore()
        let seededCursor = inactiveBatchCursor(pendingScans: [
            PhoneCallsCursorPendingScan(
                normalizedHandle: "+15557777777",
                since: Date(timeIntervalSince1970: scannedUnix - 86_400)),
        ])
        var st = DaemonState(schemaVersion: 1)
        st.sources["phone_calls"] = SourceState(cursor: try PhoneCallsCursorCodec.encode(seededCursor))
        try store.save(st)

        // Cache never fetched (hasFetched == false). isPopulated false too.
        let cache = KnownIdentifiersCache(
            baselines: [.phoneCalls: []], consumers: [.phoneCalls])

        let transport = PhoneStatefulCursorTransport(
            initialCursor: try PhoneCallsCursorCodec.encode(seededCursor))
        let sink = PhonePublisherSink()
        let plugin = makePlugin(dbURL: dbURL, store: store, cache: cache,
                                transport: transport, publisherSink: sink)

        try await plugin.tick()

        let events = await sink.allEvents()
        XCTAssertTrue(events.isEmpty, "no publish on a not-yet-fetched cache")
        let finalCursor = transport.currentDecodedCursor()
        XCTAssertEqual(finalCursor?.pendingScans.count, 1,
                       "durable scan preserved on a startup-race tick")
    }

    // MARK: - coverage-dedup widens window

    func testCoverageDedupWidensNarrowOperatorEntry() async throws {
        // A narrow operator entry (since = now-2d, progress advanced past
        // the row) for the handle, then an auto 30-day enqueue for the
        // same handle → ONE merged entry widened to 30 days, progress
        // reset, so the older row is re-walked and published.
        let dbURL = try makeCallHistoryDB(callCount: 1)
        let store = makeStateStore()
        let narrowSince = fixedNow.addingTimeInterval(-2 * 86_400)
        // Progress coordinate strictly ABOVE the seeded call so the
        // narrow entry alone would match nothing.
        let aboveZDate = InMemoryCallHistoryDB.appleEpochSeconds(unix: fixedNow.timeIntervalSince1970)
        let seededCursor = inactiveBatchCursor(pendingScans: [
            PhoneCallsCursorPendingScan(
                normalizedHandle: "+15550000001",
                since: narrowSince,
                progressBelowZDate: aboveZDate,
                progressBelowZPK: 50),
        ])
        var st = DaemonState(schemaVersion: 1)
        st.sources["phone_calls"] = SourceState(cursor: try PhoneCallsCursorCodec.encode(seededCursor))
        try store.save(st)

        let cache = KnownIdentifiersCache(
            baselines: [.phoneCalls: []], consumers: [.phoneCalls])
        await cache.replace(with: ["+15550000001"])

        let transport = PhoneStatefulCursorTransport(
            initialCursor: try PhoneCallsCursorCodec.encode(seededCursor))
        let sink = PhonePublisherSink()
        let plugin = makePlugin(dbURL: dbURL, store: store, cache: cache,
                                transport: transport, publisherSink: sink)

        try await plugin.tick()

        // The auto 30-day enqueue coverage-merges into the same handle,
        // widening the window (now-30d covers 2026-05-02) and resetting
        // progress to nil, so the call is re-walked and published.
        let scanned = await sink.scannedSourceIDs()
        XCTAssertTrue(scanned.contains("u100"),
                      "window-widen reset progress so the row is re-walked")
        let finalCursor = transport.currentDecodedCursor()
        XCTAssertEqual(finalCursor?.pendingScans.count, 0)
    }

    // MARK: - operator-queued scan executes identically

    func testOperatorQueuedScanExecutesForKnownHandle() async throws {
        // A pre-seeded operator scan (no progress) for a KNOWN handle
        // executes identically to an auto-queued one.
        let dbURL = try makeCallHistoryDB(callCount: 1)
        let store = makeStateStore()
        let seededCursor = inactiveBatchCursor(pendingScans: [
            PhoneCallsCursorPendingScan(
                normalizedHandle: "+15550000001",
                since: Date(timeIntervalSince1970: scannedUnix - 86_400)),
        ])
        var st = DaemonState(schemaVersion: 1)
        st.sources["phone_calls"] = SourceState(cursor: try PhoneCallsCursorCodec.encode(seededCursor))
        try store.save(st)

        let cache = KnownIdentifiersCache(
            baselines: [.phoneCalls: ["+15550000001"]], consumers: [.phoneCalls])
        await cache.replace(with: ["+15550000001"])

        let transport = PhoneStatefulCursorTransport(
            initialCursor: try PhoneCallsCursorCodec.encode(seededCursor))
        let sink = PhonePublisherSink()
        let plugin = makePlugin(dbURL: dbURL, store: store, cache: cache,
                                transport: transport, publisherSink: sink)

        try await plugin.tick()

        let scanned = await sink.scannedSourceIDs()
        XCTAssertTrue(scanned.contains("u100"), "operator-queued scan published the call")
        let finalCursor = transport.currentDecodedCursor()
        XCTAssertEqual(finalCursor?.pendingScans.count, 0, "operator scan dequeued on exhaustion")
    }

    // MARK: - Phase B conflict aborts the tick

    func testPhaseBConflictAbortsTickWithoutOverwriting() async throws {
        // 5 calls, budget 2. Commit #1 = Phase A enqueue (succeeds).
        // Commit #2 = Phase B progress advance → forced 409 conflict.
        // ONLY commit #2 conflicts; a later commit would SUCCEED — so a
        // regressed plugin that continued past the conflict would land a
        // STALE final commit (#3) carrying the advanced Phase-B progress,
        // failing the nil-progress assertion below. The fixed plugin
        // aborts the tick, so commit #3 never happens.
        let dbURL = try makeCallHistoryDB(callCount: 5)
        let store = makeStateStore()
        let seeded = inactiveBatchCursor()
        try store.save(DaemonState(schemaVersion: 1))
        let cache = KnownIdentifiersCache(
            baselines: [.phoneCalls: []], consumers: [.phoneCalls])
        await cache.replace(with: ["+15550000001"])

        let transport = PhoneStatefulCursorTransport(
            initialCursor: try PhoneCallsCursorCodec.encode(seeded),
            conflictOnlyAt: 2)
        let sink = PhonePublisherSink()
        let plugin = makePlugin(dbURL: dbURL, store: store, cache: cache,
                                transport: transport, publisherSink: sink,
                                maxRowsPerTick: 2)

        try await plugin.tick()

        // The persisted cursor reflects ONLY the Phase-A enqueue commit:
        // one pending scan, nil progress (the rejected Phase-B commit
        // never advanced it, AND no stale final commit landed).
        let finalCursor = transport.currentDecodedCursor()
        XCTAssertEqual(finalCursor?.pendingScans.count, 1, "scan entry still queued after abort")
        XCTAssertNil(finalCursor?.pendingScans.first?.progressBelowZDate,
                     "Phase-B progress NOT durably committed under conflict; no stale overwrite")
    }
}

/// Records the IngestEvents the publisher sends, in order.
actor PhonePublisherSink {
    private var events: [IngestEvent] = []
    func record(_ batch: [IngestEvent]) { events.append(contentsOf: batch) }
    func allEvents() -> [IngestEvent] { events }
    func scannedSourceIDs() -> Set<String> { Set(events.map(\.sourceID)) }
}

/// Stateful fake transport for the phone_calls cursor GET/commit flow.
/// Holds the current cursor JSON; GET returns it, POST commit updates it
/// and records the committed cursor. Ingest events are NOT routed here —
/// the publisher uses its own injected sender.
final class PhoneStatefulCursorTransport: @unchecked Sendable {
    private let lock = NSLock()
    private var currentCursor: String
    private var committedCursors: [String] = []
    private var commitAttempts = 0
    private let failCommits: Bool
    /// When set, ONLY the (1-based) commit at exactly this index returns
    /// a 409 cursor-conflict; the cursor is NOT updated for it but later
    /// commits succeed normally. Forcing the conflict on a SINGLE commit
    /// (not "at-or-after") is deliberate: it lets the Phase-B abort test
    /// detect a regression where a buggy plugin continues after the
    /// conflict and lands a STALE final commit — that later commit would
    /// SUCCEED and overwrite, failing the assertion.
    private let conflictOnlyAt: Int?

    init(initialCursor: String = "", failCommits: Bool = false, conflictOnlyAt: Int? = nil) {
        self.currentCursor = initialCursor
        self.failCommits = failCommits
        self.conflictOnlyAt = conflictOnlyAt
    }

    func asTransport() -> TransportFunc {
        return { [self] request in
            let url = request.url ?? URL(string: "https://test.invalid")!
            let method = request.httpMethod ?? "GET"
            func http(_ status: Int) -> HTTPURLResponse {
                HTTPURLResponse(url: url, statusCode: status, httpVersion: "HTTP/1.1",
                                headerFields: ["Content-Type": "application/json"])!
            }
            if method == "GET", url.path.hasSuffix("/cursor") {
                let cur = lock.withLock { currentCursor }
                let body = """
                    {"success":true,"data":{"cursor":\(Self.jsonString(cur)),"cursor_epoch":0,"backfill_complete":false}}
                    """
                return (Data(body.utf8), http(200))
            }
            if method == "POST", url.path.hasSuffix("/cursor") {
                if failCommits {
                    return (Data(#"{"error":{"code":"boom","message":"forced"}}"#.utf8), http(500))
                }
                let attempt = lock.withLock { () -> Int in
                    commitAttempts += 1
                    return commitAttempts
                }
                if let target = conflictOnlyAt, attempt == target {
                    // 409 cursor-conflict: cursor NOT updated (the daemon
                    // refreshes its base + aborts the tick).
                    let body = """
                        {"success":false,
                         "error":{"code":"EPOCH_MISMATCH","message":"epoch mismatch"},
                         "data":{"current_cursor":\(Self.jsonString(currentCursor)),"current_epoch":9}}
                        """
                    return (Data(body.utf8), http(409))
                }
                if let bodyData = request.httpBody,
                   let obj = try? JSONSerialization.jsonObject(with: bodyData) as? [String: Any],
                   let cursor = obj["cursor"] as? String {
                    lock.withLock {
                        currentCursor = cursor
                        committedCursors.append(cursor)
                    }
                }
                return (Data(#"{"success":true,"data":{"ok":true}}"#.utf8), http(200))
            }
            return (Data(#"{"success":true,"data":{}}"#.utf8), http(200))
        }
    }

    func currentDecodedCursor() -> PhoneCallsCursorWire? {
        lock.lock(); let cur = currentCursor; lock.unlock()
        return try? PhoneCallsCursorWireCodec.decode(cur)
    }

    /// Index of the first committed cursor whose pendingScans carries
    /// `handle`, or nil.
    func firstCommitIndexContaining(_ handle: String) -> Int? {
        lock.lock(); let commits = committedCursors; lock.unlock()
        for (i, json) in commits.enumerated() {
            if let decoded = try? PhoneCallsCursorWireCodec.decode(json),
               decoded.pendingScans.contains(where: { $0.normalizedHandle == handle }) {
                return i
            }
        }
        return nil
    }

    private static func jsonString(_ s: String) -> String {
        let data = try? JSONEncoder().encode(s)
        return data.map { String(decoding: $0, as: UTF8.self) } ?? "\"\""
    }
}
