// MessagesScanExecutionTests — Phase A (durable scan enqueue) + Phase B
// (resumable scan execution) end-to-end against a fake PiClient, an
// in-memory chat.db, and a real StateMutator over a temp StateStore.
//
// Synthetic handles only (+15550000001, test@example.com); no real PII.
import XCTest
import Foundation
import GRDB
import CRMMacCore
import CRMMacPiClient
@testable import CRMMacMessagesSource

final class MessagesScanExecutionTests: XCTestCase {
    private let auth = PiAuth(
        hostID: UUID(uuidString: "11111111-2222-3333-4444-555555555555")!,
        apiKey: "k")
    private let backfillFloor = MessagesCursorWire.defaultBackfillFloor
    // A fixed clock so the 30-day window is deterministic. 2026-05-20.
    private let fixedNow = Date(timeIntervalSince1970: 1_779_000_000)
    private let scannedUnix: TimeInterval = 1_777_680_000 // 2026-05-02
    private var tempDir: URL!

    override func setUpWithError() throws {
        try super.setUpWithError()
        tempDir = FileManager.default.temporaryDirectory
            .appendingPathComponent("crm-mac-scan-exec-\(UUID().uuidString)")
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
    /// message, so the row-emitting batches are no-ops and ONLY the scan
    /// publishes. Lets the scan-execution assertions isolate Phase B.
    private func inactiveBatchCursor(
        pendingScans: [PendingScan] = []
    ) -> MessagesCursor {
        MessagesCursor(
            backfillCursor: 0,
            liveCursor: 100_000,
            installMaxRowID: 100_000,
            backfillFloorSentAt: backfillFloor,
            backfillComplete: true,
            pendingScans: pendingScans)
    }

    /// Build an on-disk chat.db with one handle + `count` messages under
    /// it at consecutive ROWIDs starting at 100.
    private func makeChatDB(messageCount: Int, handle: String = "+15550000001") throws -> URL {
        let dbURL = tempDir.appendingPathComponent("chat.db")
        let bundle = Bundle.module
        guard let scriptURL = bundle.url(forResource: "chat_db_schema",
                                          withExtension: "sql",
                                          subdirectory: "Fixtures") else {
            throw XCTSkip("chat_db_schema.sql not in test bundle")
        }
        let script = try String(contentsOf: scriptURL, encoding: .utf8)
        let queue = try DatabaseQueue(path: dbURL.path)
        let appleNanos = Int64((scannedUnix - 978_307_200) * 1e9)
        try queue.write { db in
            try db.execute(sql: script)
            try db.execute(sql:
                "INSERT INTO handle (ROWID, id, service) VALUES (1, ?, 'iMessage')",
                arguments: [handle])
            try db.execute(sql:
                "INSERT INTO chat (ROWID, guid, style, chat_identifier) VALUES (10, 'c', 45, ?)",
                arguments: [handle])
            for i in 0..<messageCount {
                let rowID = 100 + i
                try db.execute(sql:
                    "INSERT INTO message (ROWID, guid, text, handle_id, date, " +
                    "is_from_me, cache_has_attachments, associated_message_guid) " +
                    "VALUES (?, ?, 'hi', 1, ?, 0, 0, NULL)",
                    arguments: [rowID, "g\(rowID)", appleNanos])
                try db.execute(sql:
                    "INSERT INTO chat_message_join (chat_id, message_id) VALUES (10, ?)",
                    arguments: [rowID])
            }
        }
        return dbURL
    }

    /// Build an on-disk chat.db where the scanned handle is a member of
    /// chat 10 (chat_handle_join), with one OUTBOUND row (is_from_me=1,
    /// NULL handle) in that chat at ROWID 100. Exercises the scan's
    /// outbound branch end-to-end.
    private func makeChatDBWithOutbound(handle: String = "+15550000001") throws -> URL {
        let dbURL = tempDir.appendingPathComponent("chat.db")
        let bundle = Bundle.module
        guard let scriptURL = bundle.url(forResource: "chat_db_schema",
                                          withExtension: "sql",
                                          subdirectory: "Fixtures") else {
            throw XCTSkip("chat_db_schema.sql not in test bundle")
        }
        let script = try String(contentsOf: scriptURL, encoding: .utf8)
        let queue = try DatabaseQueue(path: dbURL.path)
        let appleNanos = Int64((scannedUnix - 978_307_200) * 1e9)
        try queue.write { db in
            try db.execute(sql: script)
            try db.execute(sql:
                "INSERT INTO handle (ROWID, id, service) VALUES (1, ?, 'iMessage')",
                arguments: [handle])
            try db.execute(sql:
                "INSERT INTO chat (ROWID, guid, style, chat_identifier) VALUES (10, 'c-out', 45, ?)",
                arguments: [handle])
            try db.execute(sql:
                "INSERT INTO chat_handle_join (chat_id, handle_id) VALUES (10, 1)")
            try db.execute(sql:
                "INSERT INTO message (ROWID, guid, text, handle_id, date, " +
                "is_from_me, item_type, cache_has_attachments, associated_message_guid) " +
                "VALUES (100, 'out100', 'i texted them', NULL, ?, 1, 0, 0, NULL)",
                arguments: [appleNanos])
            try db.execute(sql:
                "INSERT INTO chat_message_join (chat_id, message_id) VALUES (10, 100)")
        }
        return dbURL
    }

    private func makePlugin(
        dbURL: URL,
        store: StateStore,
        cache: KnownIdentifiersCache,
        transport: StatefulCursorTransport,
        publisherSink: PublisherSink,
        maxRowsPerTick: Int = 500
    ) -> MessagesSourcePlugin {
        let now = fixedNow
        let piClient = PiClient(
            baseURL: URL(string: "https://pi.example.test")!,
            transport: transport.asTransport(),
            sleep: { _ in })
        let publisher = MessagesPublisher(
            sender: { _, body in
                await publisherSink.record(body.events)
                return IngestEventsData(
                    accepted: body.events.count, duplicate: 0, rejected: 0, errors: [])
            },
            auth: auth, logger: NoopLogger())
        return MessagesSourcePlugin(
            tickInterval: 60,
            config: MessagesSourceConfig(
                chatDBPath: dbURL,
                backfillFloor: backfillFloor,
                maxRowsPerTick: maxRowsPerTick),
            piClient: piClient,
            auth: auth,
            mutator: StateMutator(store: store),
            publisher: publisher,
            cache: cache,
            healthRegistry: SourceHealthRegistry(),
            logger: NoopLogger(),
            clock: { now })
    }

    // MARK: - Phase A durability + scan execution

    func testNewlyKnownEnqueuesDurablyThenScansAndPublishes() async throws {
        let dbURL = try makeChatDB(messageCount: 1)
        let store = makeStateStore()
        let seeded = inactiveBatchCursor()
        try store.save(DaemonState(schemaVersion: 1))
        let cache = KnownIdentifiersCache(
            baselines: [.messages: []], consumers: [.messages])
        // A heartbeat fetch surfaces the new identifier.
        await cache.replace(with: ["+15550000001"])

        let transport = StatefulCursorTransport(
            initialCursor: try MessagesCursorCodec.encode(seeded))
        let sink = PublisherSink()
        let plugin = makePlugin(dbURL: dbURL, store: store, cache: cache,
                                transport: transport, publisherSink: sink)

        try await plugin.tick()

        // The cursor was committed with the pendingScans entry BEFORE any
        // scan row published (Phase A durability): the first committed
        // cursor that carried the handle was committed before the first
        // ingest event.
        let firstEnqueueIndex = transport.firstCommitIndexContaining("+15550000001")
        XCTAssertNotNil(firstEnqueueIndex, "a committed cursor must carry the enqueued scan")

        // The scanned message published with the right kind/source_id.
        let events = await sink.allEvents()
        XCTAssertTrue(events.contains { $0.sourceID == "g100" && $0.kind == "raw_message.received" },
                      "scanned message published")

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

    // MARK: - newly-known scan publishes outbound rows

    func testNewlyKnownScanPublishesOutboundAsSent() async throws {
        let dbURL = try makeChatDBWithOutbound()
        let store = makeStateStore()
        let seeded = inactiveBatchCursor()
        try store.save(DaemonState(schemaVersion: 1))
        let cache = KnownIdentifiersCache(
            baselines: [.messages: []], consumers: [.messages])
        // The recipient becomes newly-known via a heartbeat fetch.
        await cache.replace(with: ["+15550000001"])

        let transport = StatefulCursorTransport(
            initialCursor: try MessagesCursorCodec.encode(seeded))
        let sink = PublisherSink()
        let plugin = makePlugin(dbURL: dbURL, store: store, cache: cache,
                                transport: transport, publisherSink: sink)

        try await plugin.tick()

        let events = await sink.allEvents()
        let outbound = events.first { $0.sourceID == "out100" }
        let event = try XCTUnwrap(outbound, "the pending scan published the outbound row")
        XCTAssertEqual(event.kind, "raw_message.sent",
                       "newly-known contact's outbound history backfilled as raw_message.sent")
        // Scan dequeued after exhaustion.
        let finalCursor = transport.currentDecodedCursor()
        XCTAssertEqual(finalCursor?.pendingScans.count, 0)
    }

    // MARK: - Phase A failure rolls back

    func testPhaseAFailureReturnsIdentifierForReEnqueue() async throws {
        let dbURL = try makeChatDB(messageCount: 1)
        let store = makeStateStore()
        try store.save(DaemonState(schemaVersion: 1))
        let cache = KnownIdentifiersCache(
            baselines: [.messages: []], consumers: [.messages])
        await cache.replace(with: ["+15550000001"])

        // Transport fails every commit → Phase A cannot persist.
        let transport = StatefulCursorTransport(failCommits: true)
        let sink = PublisherSink()
        let plugin = makePlugin(dbURL: dbURL, store: store, cache: cache,
                                transport: transport, publisherSink: sink)

        try await plugin.tick()

        // No scan executed (Phase A aborted before Phase B).
        let events = await sink.allEvents()
        XCTAssertTrue(events.isEmpty, "no scan rows published when Phase A fails")

        // The identifier was returned to the cache → a subsequent drain
        // re-returns it for re-enqueue.
        let reDrained = await cache.drainNewlyAdded(for: .messages)
        XCTAssertEqual(reDrained, ["+15550000001"], "identifier returned for re-enqueue")
    }

    // MARK: - resumable / larger-than-budget

    func testResumableScanAcrossTicksDropsNoRows() async throws {
        // 5 matching messages, budget 2 → 3 ticks to fully walk.
        let dbURL = try makeChatDB(messageCount: 5)
        let store = makeStateStore()
        let seeded = inactiveBatchCursor()
        try store.save(DaemonState(schemaVersion: 1))
        let cache = KnownIdentifiersCache(
            baselines: [.messages: []], consumers: [.messages])
        await cache.replace(with: ["+15550000001"])

        let transport = StatefulCursorTransport(
            initialCursor: try MessagesCursorCodec.encode(seeded))
        let sink = PublisherSink()
        // Small budget forces a multi-page scan. The row-emitting
        // batches are inactive (backfill complete + live past every row),
        // so ONLY the scan publishes.
        let plugin = makePlugin(dbURL: dbURL, store: store, cache: cache,
                                transport: transport, publisherSink: sink,
                                maxRowsPerTick: 2)

        // Three ticks: subsequent ticks have an empty drain but resume
        // the persisted pendingScans entry.
        for _ in 0..<3 {
            try await plugin.tick()
        }

        let scanned = await sink.scannedSourceIDs()
        XCTAssertEqual(scanned, Set((100...104).map { "g\($0)" }),
                       "every scanned row published across the multi-tick walk")
        let finalCursor = transport.currentDecodedCursor()
        XCTAssertEqual(finalCursor?.pendingScans.count, 0, "scan dequeued once exhausted")
    }

    // MARK: - Phase B conflict aborts the tick

    func testPhaseBConflictAbortsTickWithoutOverwriting() async throws {
        // 5 messages, budget 2. Commit #1 = Phase A enqueue (succeeds).
        // Commit #2 = Phase B progress advance → forced 409 conflict
        // (ONLY commit #2; a later commit would succeed). A regressed
        // plugin that continued past the conflict would land a STALE
        // final commit carrying advanced progress, failing the assertion.
        let dbURL = try makeChatDB(messageCount: 5)
        let store = makeStateStore()
        let seeded = inactiveBatchCursor()
        try store.save(DaemonState(schemaVersion: 1))
        let cache = KnownIdentifiersCache(
            baselines: [.messages: []], consumers: [.messages])
        await cache.replace(with: ["+15550000001"])

        let transport = StatefulCursorTransport(
            initialCursor: try MessagesCursorCodec.encode(seeded),
            conflictOnlyAt: 2)
        let sink = PublisherSink()
        let plugin = makePlugin(dbURL: dbURL, store: store, cache: cache,
                                transport: transport, publisherSink: sink,
                                maxRowsPerTick: 2)

        try await plugin.tick()

        let finalCursor = transport.currentDecodedCursor()
        XCTAssertEqual(finalCursor?.pendingScans.count, 1, "scan entry still queued after abort")
        XCTAssertNil(finalCursor?.pendingScans.first?.progressBelowRowID,
                     "Phase-B progress NOT durably committed under conflict; no stale overwrite")
    }

    // MARK: - membership gate (hasFetched vs isPopulated)

    func testUnknownHandleDroppedWhenFetched() async throws {
        // An operator pre-seeded a scan for a handle NOT in the known
        // set. With hasFetched true (even on an empty CRM), Phase B drops
        // it without publishing.
        let dbURL = try makeChatDB(messageCount: 1, handle: "+15550000001")
        let store = makeStateStore()
        // Seed the cursor with a pending scan for an unknown handle.
        let seededCursor = MessagesCursor(
            backfillFloorSentAt: backfillFloor,
            pendingScans: [
                PendingScan(normalizedHandle: "+15557777777",
                            since: Date(timeIntervalSince1970: scannedUnix - 86_400)),
            ])
        var st = DaemonState(schemaVersion: 1)
        st.sources["messages"] = SourceState(cursor: try MessagesCursorCodec.encode(seededCursor))
        try store.save(st)

        let cache = KnownIdentifiersCache(
            baselines: [.messages: []], consumers: [.messages])
        // Empty CRM but FETCHED.
        await cache.replace(with: [])

        let transport = StatefulCursorTransport(
            initialCursor: try MessagesCursorCodec.encode(seededCursor))
        let sink = PublisherSink()
        let plugin = makePlugin(dbURL: dbURL, store: store, cache: cache,
                                transport: transport, publisherSink: sink)

        try await plugin.tick()

        let events = await sink.allEvents()
        XCTAssertTrue(events.isEmpty, "unknown-handle scan publishes nothing")
        let finalCursor = transport.currentDecodedCursor()
        XCTAssertEqual(finalCursor?.pendingScans.count, 0, "unknown-handle entry dropped")
    }

    func testUnknownHandleDeferredWhenNotFetched() async throws {
        // Same seeded scan, but the cache has NOT fetched yet (no
        // heartbeat). Phase B must NOT drop it — it survives for a later
        // tick.
        let dbURL = try makeChatDB(messageCount: 1, handle: "+15550000001")
        let store = makeStateStore()
        let seededCursor = MessagesCursor(
            backfillFloorSentAt: backfillFloor,
            pendingScans: [
                PendingScan(normalizedHandle: "+15557777777",
                            since: Date(timeIntervalSince1970: scannedUnix - 86_400)),
            ])
        var st = DaemonState(schemaVersion: 1)
        st.sources["messages"] = SourceState(cursor: try MessagesCursorCodec.encode(seededCursor))
        try store.save(st)

        // Cache never fetched (hasFetched == false). isPopulated false too.
        let cache = KnownIdentifiersCache(
            baselines: [.messages: []], consumers: [.messages])

        let transport = StatefulCursorTransport(
            initialCursor: try MessagesCursorCodec.encode(seededCursor))
        let sink = PublisherSink()
        let plugin = makePlugin(dbURL: dbURL, store: store, cache: cache,
                                transport: transport, publisherSink: sink)

        try await plugin.tick()

        let events = await sink.allEvents()
        XCTAssertTrue(events.isEmpty, "no publish on a not-yet-fetched cache")
        // The entry survives — the seeded cursor is unchanged in the
        // transport (no commit removed it).
        let finalCursor = transport.currentDecodedCursor()
        XCTAssertEqual(finalCursor?.pendingScans.count, 1,
                       "durable scan preserved on a startup-race tick")
    }

    // MARK: - coverage-dedup widens window

    func testCoverageDedupWidensNarrowOperatorEntry() async throws {
        // A narrow operator entry (since = now-2d, progress advanced)
        // for the handle, then an auto 30-day enqueue for the same handle
        // → ONE merged entry widened to 30 days with progress reset.
        let dbURL = try makeChatDB(messageCount: 1)
        let store = makeStateStore()
        let narrowSince = fixedNow.addingTimeInterval(-2 * 86_400)
        let seededCursor = MessagesCursor(
            backfillFloorSentAt: backfillFloor,
            pendingScans: [
                PendingScan(normalizedHandle: "+15550000001",
                            since: narrowSince,
                            progressBelowRowID: 50),
            ])
        var st = DaemonState(schemaVersion: 1)
        st.sources["messages"] = SourceState(cursor: try MessagesCursorCodec.encode(seededCursor))
        try store.save(st)

        let cache = KnownIdentifiersCache(
            baselines: [.messages: []], consumers: [.messages])
        await cache.replace(with: ["+15550000001"])

        let transport = StatefulCursorTransport(
            initialCursor: try MessagesCursorCodec.encode(seededCursor))
        let sink = PublisherSink()
        let plugin = makePlugin(dbURL: dbURL, store: store, cache: cache,
                                transport: transport, publisherSink: sink)

        try await plugin.tick()

        // The narrow seeded entry (since = now-2d, progress < 50) would
        // match nothing: the 2026-05-02 message is older than 2 days AND
        // its ROWID 100 is above the progress bound. The auto 30-day
        // enqueue coverage-merges into the same handle, widening the
        // window and resetting progress to nil, so ROWID 100 is re-walked
        // and published.
        let scanned = await sink.scannedSourceIDs()
        XCTAssertTrue(scanned.contains("g100"),
                      "window-widen reset progress so the row is re-walked")
        // Exactly one merged entry existed (no duplicate); it dequeued
        // after exhaustion.
        let finalCursor = transport.currentDecodedCursor()
        XCTAssertEqual(finalCursor?.pendingScans.count, 0)
    }
}

/// Records the IngestEvents the publisher sends, in order.
actor PublisherSink {
    private var events: [IngestEvent] = []
    func record(_ batch: [IngestEvent]) { events.append(contentsOf: batch) }
    func allEvents() -> [IngestEvent] { events }
    func scannedSourceIDs() -> Set<String> { Set(events.map(\.sourceID)) }
}

/// Stateful fake transport for the cursor GET/commit flow. Holds the
/// current cursor JSON; GET returns it, POST commit updates it and
/// records the committed cursor. Ingest events are NOT routed here —
/// the publisher uses its own injected sender.
final class StatefulCursorTransport: @unchecked Sendable {
    private let lock = NSLock()
    private var currentCursor: String
    private var committedCursors: [String] = []
    private var commitAttempts = 0
    private let failCommits: Bool
    /// When set, ONLY the (1-based) commit at exactly this index returns
    /// a 409 cursor-conflict (cursor NOT updated); later commits succeed.
    /// Forcing the conflict on a SINGLE commit lets the Phase-B abort
    /// test detect a regression where a buggy plugin continues past the
    /// conflict and lands a STALE final commit that would SUCCEED.
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
            // Cursor GET.
            if method == "GET", url.path.hasSuffix("/cursor") {
                let cur = lock.withLock { currentCursor }
                let body = """
                    {"success":true,"data":{"cursor":\(Self.jsonString(cur)),"cursor_epoch":0,"backfill_complete":false}}
                    """
                return (Data(body.utf8), http(200))
            }
            // Cursor commit.
            if method == "POST", url.path.hasSuffix("/cursor") {
                if failCommits {
                    return (Data(#"{"error":{"code":"boom","message":"forced"}}"#.utf8), http(500))
                }
                let attempt = lock.withLock { () -> Int in
                    commitAttempts += 1
                    return commitAttempts
                }
                if let target = conflictOnlyAt, attempt == target {
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
            // Any other path (shouldn't happen in these tests).
            return (Data(#"{"success":true,"data":{}}"#.utf8), http(200))
        }
    }

    func currentDecodedCursor() -> MessagesCursorWire? {
        lock.lock(); let cur = currentCursor; lock.unlock()
        return try? MessagesCursorWireCodec.decode(cur)
    }

    /// Index of the first committed cursor whose pendingScans carries
    /// `handle`, or nil.
    func firstCommitIndexContaining(_ handle: String) -> Int? {
        lock.lock(); let commits = committedCursors; lock.unlock()
        for (i, json) in commits.enumerated() {
            if let decoded = try? MessagesCursorWireCodec.decode(json),
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
