// MessagesSourcePluginTests exercise the per-tick orchestration with
// a fake PiClient, an in-memory chat.db, and a real StateMutator
// against a temp-dir StateStore.
//
// These are integration-flavoured tests — they instantiate the plugin
// + every collaborator and observe behavior end-to-end.  Pure-logic
// tests for individual components live in the per-file test files.
import XCTest
import Foundation
import GRDB
import CRMMacCore
import CRMMacPiClient
@testable import CRMMacMessagesSource

final class MessagesSourcePluginTests: XCTestCase {
    private let auth = PiAuth(
        hostID: UUID(uuidString: "11111111-2222-3333-4444-555555555555")!,
        apiKey: "k")
    private let backfillFloor = MessagesCursorWire.defaultBackfillFloor
    private let unix2026: TimeInterval = 1_778_686_938 // 2026-05-13T15:42:18Z
    private var tempDir: URL!

    override func setUpWithError() throws {
        try super.setUpWithError()
        tempDir = FileManager.default.temporaryDirectory
            .appendingPathComponent("crm-mac-mesg-plugin-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: tempDir, withIntermediateDirectories: true)
    }

    override func tearDownWithError() throws {
        try? FileManager.default.removeItem(at: tempDir)
        try super.tearDownWithError()
    }

    /// Build an on-disk chat.db at a temp path, write the schema,
    /// then return the URL.
    private func makeChatDB() throws -> URL {
        let dbURL = tempDir.appendingPathComponent("chat.db")
        let bundle = Bundle.module
        guard let scriptURL = bundle.url(forResource: "chat_db_schema",
                                          withExtension: "sql",
                                          subdirectory: "Fixtures") else {
            throw XCTSkip("chat_db_schema.sql not in test bundle")
        }
        let script = try String(contentsOf: scriptURL, encoding: .utf8)
        let config = Configuration()
        let queue = try DatabaseQueue(path: dbURL.path, configuration: config)
        try queue.write { db in
            try db.execute(sql: script)
            // Seed handle + chat + one inbound message at 2026-05-13.
            try db.execute(sql:
                "INSERT INTO handle (ROWID, id, service) VALUES (1, '+15551234567', 'iMessage')")
            try db.execute(sql:
                "INSERT INTO chat (ROWID, guid, style, chat_identifier) " +
                "VALUES (10, 'iMessage;-;+15551234567', 45, '+15551234567')")
            try db.execute(sql:
                "INSERT INTO chat_handle_join (chat_id, handle_id) VALUES (10, 1)")
            let appleNanos = Int64((self.unix2026 - 978_307_200) * 1e9)
            try db.execute(sql:
                "INSERT INTO message (ROWID, guid, text, handle_id, date, " +
                "is_from_me, cache_has_attachments, associated_message_guid) " +
                "VALUES (1, 'g1', 'hi', 1, ?, 0, 0, NULL)",
                arguments: [appleNanos])
            try db.execute(sql:
                "INSERT INTO chat_message_join (chat_id, message_id) VALUES (10, 1)")
        }
        return dbURL
    }

    private func makeStateStore() -> StateStore {
        StateStore(fileURL: tempDir.appendingPathComponent("state.json"))
    }

    /// Sync helper that adds a second seeded message at ROWID=2 to a
    /// chat.db produced by `makeChatDB()`. Lives in a sync method so
    /// the GRDB write closure isn't inferred as `@Sendable` (which
    /// would block `self.unix2026` capture from an async test).
    private func seedSecondMessage(at dbURL: URL) throws {
        let queue = try DatabaseQueue(path: dbURL.path)
        let appleNanos = Int64((unix2026 - 978_307_200) * 1e9)
        try queue.write { db in
            try db.execute(sql:
                "INSERT INTO message (ROWID, guid, text, handle_id, date, " +
                "is_from_me, cache_has_attachments, associated_message_guid) " +
                "VALUES (2, 'g2', 'bye', 1, ?, 0, 0, NULL)",
                arguments: [appleNanos])
            try db.execute(sql:
                "INSERT INTO chat_message_join (chat_id, message_id) VALUES (10, 2)")
        }
    }

    // MARK: - smoke: cache empty -> tick is no-op

    func testTickSkipsWhenKnownIdentifiersCacheEmpty() async throws {
        let dbURL = try makeChatDB()
        let store = makeStateStore()
        try store.save(DaemonState(schemaVersion: 1))

        // PiClient that would fail on any call; we want to verify the
        // tick short-circuits BEFORE the cursor GET.
        nonisolated(unsafe) var piCalled = false
        let publisher = MessagesPublisher(
            sender: { _, _ in
                piCalled = true
                return IngestEventsData(accepted: 0, duplicate: 0,
                                         rejected: 0, errors: [])
            },
            auth: auth, logger: NoopLogger())
        // Build the plugin but with an EMPTY known-identifiers cache.
        // The tick MUST short-circuit before touching the publisher.
        let cache = KnownIdentifiersCache(initial: [])
        let plugin = MessagesSourcePlugin(
            tickInterval: 60,
            config: MessagesSourceConfig(
                chatDBPath: dbURL,
                backfillFloor: backfillFloor),
            piClient: makePiClientThatNeverFiresIngestPath(),
            auth: auth,
            mutator: StateMutator(store: store),
            publisher: publisher,
            cache: cache,
            healthRegistry: SourceHealthRegistry(),
            logger: NoopLogger())

        // Tick: should fetch cursor, decide cache is empty, and skip.
        try await plugin.tick()
        XCTAssertFalse(piCalled, "publisher must not be invoked when cache is empty")
    }

    // MARK: - lastScheduledAt persistence

    func testTickPersistsLastScheduledAtToState() async throws {
        // The plugin's tick() writes `lastScheduledAt` to state.json at
        // the very top — even a tick that short-circuits on an empty
        // known-identifiers cache must persist the field. Without this
        // persistence, Doctor / debugging tools have no reliable
        // cross-source liveness signal — the in-memory
        // SourceHealthRegistry is heartbeat-payload-only.
        let dbURL = try makeChatDB()
        let store = makeStateStore()
        try store.save(DaemonState(schemaVersion: 1))
        let mutator = StateMutator(store: store)
        let cache = KnownIdentifiersCache(initial: [])
        let plugin = MessagesSourcePlugin(
            tickInterval: 60,
            config: MessagesSourceConfig(
                chatDBPath: dbURL,
                backfillFloor: backfillFloor),
            piClient: makePiClientThatNeverFiresIngestPath(),
            auth: auth,
            mutator: mutator,
            publisher: MessagesPublisher(
                sender: { _, _ in
                    IngestEventsData(accepted: 0, duplicate: 0,
                                      rejected: 0, errors: [])
                },
                auth: auth, logger: NoopLogger()),
            cache: cache,
            healthRegistry: SourceHealthRegistry(),
            logger: NoopLogger())

        let beforeTick = Date()
        try await plugin.tick()
        let state = try await mutator.read()
        let scheduled = state.sources[SourceID.messages.rawValue]?.lastScheduledAt
        XCTAssertNotNil(scheduled, "tick() must persist lastScheduledAt to state.json")
        if let scheduled {
            XCTAssertGreaterThanOrEqual(
                scheduled, beforeTick.addingTimeInterval(-1),
                "lastScheduledAt must be set to a clock value near the tick start")
        }
    }

    // MARK: - smoke: full tick flow with populated cache

    /// End-to-end orchestration: cache pre-populated with the seeded
    /// handle, scripted Pi transport for GET cursor + POST commit
    /// cursor, capturing publisher. Verifies the seeded inbound
    /// message flows through publish and the post-tick state writer
    /// records lastPushedAt.
    func testTickEmitsAndCommitsOnPopulatedCache() async throws {
        let dbURL = try makeChatDB()
        // Seed a second row at ROWID=2 so the install-max capture sets
        // installMaxRowID=2; backfill then scans `< 2` and picks up the
        // ROWID=1 row that makeChatDB() seeded.
        try seedSecondMessage(at: dbURL)

        let store = makeStateStore()
        try store.save(DaemonState(schemaVersion: 1))

        // Capture publisher invocations.
        nonisolated(unsafe) var publishCallCount = 0
        nonisolated(unsafe) var lastPublishedEvents: [IngestEvent] = []
        let publisher = MessagesPublisher(
            sender: { _, body in
                publishCallCount += 1
                lastPublishedEvents = body.events
                return IngestEventsData(
                    accepted: body.events.count,
                    duplicate: 0, rejected: 0, errors: [])
            },
            auth: auth, logger: NoopLogger())

        // Scripted PiClient: GET cursor (fresh install) -> POST commit cursor (ok).
        let script = MessagesSourcePluginTestScript([
            .respond(status: 200, data: Data(
                #"{"success":true,"data":{"cursor":"","cursor_epoch":0,"backfill_complete":false}}"#.utf8)),
            .respond(status: 200, data: Data(
                #"{"success":true,"data":{"ok":true}}"#.utf8)),
        ])
        let piClient = PiClient(
            baseURL: URL(string: "https://pi.example.test")!,
            transport: script.asTransport(),
            sleep: { _ in })

        // Cache pre-populated with the canonical form of the seeded handle.
        let cache = KnownIdentifiersCache(initial: ["+15551234567"])
        let plugin = MessagesSourcePlugin(
            tickInterval: 60,
            config: MessagesSourceConfig(
                chatDBPath: dbURL,
                backfillFloor: backfillFloor),
            piClient: piClient,
            auth: auth,
            mutator: StateMutator(store: store),
            publisher: publisher,
            cache: cache,
            healthRegistry: SourceHealthRegistry(),
            logger: NoopLogger())

        try await plugin.tick()

        // Publisher invoked with the seeded row.
        XCTAssertEqual(publishCallCount, 1,
                       "publisher invoked once with the seeded row")
        XCTAssertEqual(lastPublishedEvents.count, 1,
                       "exactly one event emitted")
        XCTAssertEqual(lastPublishedEvents.first?.kind, "raw_message.received")
        XCTAssertEqual(lastPublishedEvents.first?.sourceID, "g1",
                       "event source_id is the chat.db guid")

        // State-file updated post-tick (lastPushedAt populated, cursor non-empty).
        let state = try store.load()
        let sourceState = state.sources["messages"]
        XCTAssertNotNil(sourceState,
                        "messages source state written after tick")
        XCTAssertNotNil(sourceState?.lastPushedAt,
                        "lastPushedAt set after successful tick")
        XCTAssertFalse(sourceState?.cursor.isEmpty ?? true,
                       "cursor JSON committed locally")
    }

    // MARK: - outbound emission

    /// Build a chat.db with one OUTBOUND (is_from_me=1, NULL handle) row
    /// in a 1:1 chat with `peer`, at ROWID 1, plus a second inbound row
    /// at ROWID 2 so install-max capture sets installMaxRowID=2 and the
    /// backfill scans `< 2` to pick up the outbound row.
    private func makeChatDBWithOutbound1to1(peer: String = "+15551234567") throws -> URL {
        let dbURL = tempDir.appendingPathComponent("chat.db")
        let script = try loadSchemaScript()
        let queue = try DatabaseQueue(path: dbURL.path)
        let appleNanos = Int64((unix2026 - 978_307_200) * 1e9)
        try queue.write { db in
            try db.execute(sql: script)
            try db.execute(sql:
                "INSERT INTO handle (ROWID, id, service) VALUES (1, ?, 'iMessage')",
                arguments: [peer])
            try db.execute(sql:
                "INSERT INTO chat (ROWID, guid, style, chat_identifier) VALUES (10, 'chat-1to1', 45, ?)",
                arguments: [peer])
            try db.execute(sql:
                "INSERT INTO chat_handle_join (chat_id, handle_id) VALUES (10, 1)")
            // ROWID 1: outbound, NULL handle.
            try db.execute(sql:
                "INSERT INTO message (ROWID, guid, text, handle_id, date, " +
                "is_from_me, item_type, cache_has_attachments, associated_message_guid) " +
                "VALUES (1, 'out1', 'sent you a note', NULL, ?, 1, 0, 0, NULL)",
                arguments: [appleNanos])
            try db.execute(sql:
                "INSERT INTO chat_message_join (chat_id, message_id) VALUES (10, 1)")
            // ROWID 2: inbound, so install-max is 2 and backfill walks below it.
            try db.execute(sql:
                "INSERT INTO message (ROWID, guid, text, handle_id, date, " +
                "is_from_me, item_type, cache_has_attachments, associated_message_guid) " +
                "VALUES (2, 'in2', 'hi back', 1, ?, 0, 0, 0, NULL)",
                arguments: [appleNanos])
            try db.execute(sql:
                "INSERT INTO chat_message_join (chat_id, message_id) VALUES (10, 2)")
        }
        return dbURL
    }

    /// Build a chat.db with one OUTBOUND row in a GROUP chat whose first
    /// chat_handle_join member is `firstMember`, plus a second inbound
    /// row at ROWID 2 (from firstMember) so install-max is 2.
    private func makeChatDBWithOutboundGroup(
        firstMember: String = "+15551110000",
        secondMember: String = "+15552220000"
    ) throws -> URL {
        let dbURL = tempDir.appendingPathComponent("chat.db")
        let script = try loadSchemaScript()
        let queue = try DatabaseQueue(path: dbURL.path)
        let appleNanos = Int64((unix2026 - 978_307_200) * 1e9)
        try queue.write { db in
            try db.execute(sql: script)
            try db.execute(sql:
                "INSERT INTO handle (ROWID, id, service) VALUES (1, ?, 'iMessage')",
                arguments: [firstMember])
            try db.execute(sql:
                "INSERT INTO handle (ROWID, id, service) VALUES (2, ?, 'iMessage')",
                arguments: [secondMember])
            try db.execute(sql:
                "INSERT INTO chat (ROWID, guid, style, chat_identifier) VALUES (20, 'group-1', 43, 'group-1')")
            // firstMember joined first (lower chat_handle_join ROWID).
            try db.execute(sql:
                "INSERT INTO chat_handle_join (chat_id, handle_id) VALUES (20, 1)")
            try db.execute(sql:
                "INSERT INTO chat_handle_join (chat_id, handle_id) VALUES (20, 2)")
            // ROWID 1: outbound group row, NULL handle.
            try db.execute(sql:
                "INSERT INTO message (ROWID, guid, text, handle_id, date, " +
                "is_from_me, item_type, cache_has_attachments, associated_message_guid) " +
                "VALUES (1, 'gout1', 'group hello', NULL, ?, 1, 0, 0, NULL)",
                arguments: [appleNanos])
            try db.execute(sql:
                "INSERT INTO chat_message_join (chat_id, message_id) VALUES (20, 1)")
            // ROWID 2: inbound from member 2, so install-max is 2.
            try db.execute(sql:
                "INSERT INTO message (ROWID, guid, text, handle_id, date, " +
                "is_from_me, item_type, cache_has_attachments, associated_message_guid) " +
                "VALUES (2, 'gin2', 'reply', 2, ?, 0, 0, 0, NULL)",
                arguments: [appleNanos])
            try db.execute(sql:
                "INSERT INTO chat_message_join (chat_id, message_id) VALUES (20, 2)")
        }
        return dbURL
    }

    /// Build a chat.db with one OUTBOUND row in a 1:1 chat that has NO
    /// chat_handle_join rows (unresolvable peer), plus an inbound row at
    /// ROWID 2 so install-max is 2.
    private func makeChatDBWithUnresolvableOutbound() throws -> URL {
        let dbURL = tempDir.appendingPathComponent("chat.db")
        let script = try loadSchemaScript()
        let queue = try DatabaseQueue(path: dbURL.path)
        let appleNanos = Int64((unix2026 - 978_307_200) * 1e9)
        try queue.write { db in
            try db.execute(sql: script)
            try db.execute(sql:
                "INSERT INTO handle (ROWID, id, service) VALUES (1, '+15551234567', 'iMessage')")
            // Chat exists (so chat.guid is non-NULL) but has no membership.
            try db.execute(sql:
                "INSERT INTO chat (ROWID, guid, style, chat_identifier) VALUES (10, 'orphan-chat', 45, 'orphan')")
            try db.execute(sql:
                "INSERT INTO message (ROWID, guid, text, handle_id, date, " +
                "is_from_me, item_type, cache_has_attachments, associated_message_guid) " +
                "VALUES (1, 'orph1', 'into the void', NULL, ?, 1, 0, 0, NULL)",
                arguments: [appleNanos])
            try db.execute(sql:
                "INSERT INTO chat_message_join (chat_id, message_id) VALUES (10, 1)")
            // ROWID 2 inbound so install-max is 2.
            try db.execute(sql:
                "INSERT INTO message (ROWID, guid, text, handle_id, date, " +
                "is_from_me, item_type, cache_has_attachments, associated_message_guid) " +
                "VALUES (2, 'orph-in2', 'hi', 1, ?, 0, 0, 0, NULL)",
                arguments: [appleNanos])
            try db.execute(sql:
                "INSERT INTO chat_message_join (chat_id, message_id) VALUES (10, 2)")
        }
        return dbURL
    }

    /// Build a chat.db with only contentless system rows (item_type=2,
    /// no text, no attachment) so a tick inspects them, skips them all,
    /// yet still consumes budget.
    private func makeChatDBWithSystemRowsOnly(count: Int) throws -> URL {
        let dbURL = tempDir.appendingPathComponent("chat.db")
        let script = try loadSchemaScript()
        let queue = try DatabaseQueue(path: dbURL.path)
        let appleNanos = Int64((unix2026 - 978_307_200) * 1e9)
        try queue.write { db in
            try db.execute(sql: script)
            try db.execute(sql:
                "INSERT INTO handle (ROWID, id, service) VALUES (1, '+15551234567', 'iMessage')")
            try db.execute(sql:
                "INSERT INTO chat (ROWID, guid, style, chat_identifier) VALUES (10, 'sys-chat', 45, 'x')")
            try db.execute(sql:
                "INSERT INTO chat_handle_join (chat_id, handle_id) VALUES (10, 1)")
            for i in 1...count {
                try db.execute(sql:
                    "INSERT INTO message (ROWID, guid, text, handle_id, date, " +
                    "is_from_me, item_type, cache_has_attachments, associated_message_guid) " +
                    "VALUES (?, ?, NULL, 1, ?, 0, 2, 0, NULL)",
                    arguments: [i, "sys\(i)", appleNanos])
                try db.execute(sql:
                    "INSERT INTO chat_message_join (chat_id, message_id) VALUES (10, ?)",
                    arguments: [i])
            }
        }
        return dbURL
    }

    private func loadSchemaScript() throws -> String {
        let bundle = Bundle.module
        guard let scriptURL = bundle.url(forResource: "chat_db_schema",
                                          withExtension: "sql",
                                          subdirectory: "Fixtures") else {
            throw XCTSkip("chat_db_schema.sql not in test bundle")
        }
        return try String(contentsOf: scriptURL, encoding: .utf8)
    }

    /// Build a plugin + capturing publisher over a scripted Pi that
    /// answers GET cursor (fresh install) then OK for every commit.
    /// Returns the plugin, a sink capturing published events, and the
    /// state store for post-tick assertions.
    private func makeOutboundPlugin(
        dbURL: URL,
        known: Set<String>,
        resolver: (@Sendable (GRDB.Database, [ChatDBMessage]) throws -> [String: String])? = nil
    ) throws -> (plugin: MessagesSourcePlugin, sink: OutboundPublisherSink, store: StateStore) {
        let store = makeStateStore()
        try store.save(DaemonState(schemaVersion: 1))
        let sink = OutboundPublisherSink()
        let publisher = MessagesPublisher(
            sender: { _, body in
                await sink.record(body.events)
                return IngestEventsData(
                    accepted: body.events.count, duplicate: 0, rejected: 0, errors: [])
            },
            auth: auth, logger: NoopLogger())
        // GET cursor + many OK commits (one per scan/backfill/live commit).
        var steps: [MessagesSourcePluginTestScript.Step] = [
            .respond(status: 200, data: Data(
                #"{"success":true,"data":{"cursor":"","cursor_epoch":0,"backfill_complete":false}}"#.utf8)),
        ]
        for _ in 0..<10 {
            steps.append(.respond(status: 200, data: Data(
                #"{"success":true,"data":{"ok":true}}"#.utf8)))
        }
        let piClient = PiClient(
            baseURL: URL(string: "https://pi.example.test")!,
            transport: MessagesSourcePluginTestScript(steps).asTransport(),
            sleep: { _ in })
        let cache = KnownIdentifiersCache(initial: known)
        let plugin = MessagesSourcePlugin(
            tickInterval: 60,
            config: MessagesSourceConfig(chatDBPath: dbURL, backfillFloor: backfillFloor),
            piClient: piClient,
            auth: auth,
            mutator: StateMutator(store: store),
            publisher: publisher,
            cache: cache,
            healthRegistry: SourceHealthRegistry(),
            logger: NoopLogger(),
            outboundPeerResolver: resolver ?? ChatDBReader.resolveOutboundPeers)
        return (plugin, sink, store)
    }

    func testTickEmitsOutboundAsSent() async throws {
        let dbURL = try makeChatDBWithOutbound1to1(peer: "+15551234567")
        let (plugin, sink, _) = try makeOutboundPlugin(dbURL: dbURL, known: ["+15551234567"])
        try await plugin.tick()
        let events = await sink.allEvents()
        let sent = events.first { $0.sourceID == "out1" }
        let outEvent = try XCTUnwrap(sent, "outbound row published")
        XCTAssertEqual(outEvent.kind, "raw_message.sent")
        let peer = await sink.peerHandle(forSourceID: "out1")
        XCTAssertEqual(peer, "+15551234567",
                       "outbound peer resolved from chat_handle_join")
    }

    func testTickEmitsOutboundGroupAttributedToFirstMember() async throws {
        let dbURL = try makeChatDBWithOutboundGroup(
            firstMember: "+15551110000", secondMember: "+15552220000")
        // Only the first member is known.
        let (plugin, sink, _) = try makeOutboundPlugin(dbURL: dbURL, known: ["+15551110000"])
        try await plugin.tick()
        let peer = await sink.peerHandle(forSourceID: "gout1")
        XCTAssertEqual(peer, "+15551110000",
                       "group outbound attributed to first chat_handle_join member")
    }

    func testTickDropsOutboundToUnknownRecipient() async throws {
        let dbURL = try makeChatDBWithOutbound1to1(peer: "+15551234567")
        // The recipient is NOT in the known set; an unrelated handle is.
        let (plugin, sink, store) = try makeOutboundPlugin(dbURL: dbURL, known: ["+15559998888"])
        try await plugin.tick()
        let events = await sink.allEvents()
        XCTAssertFalse(events.contains { $0.sourceID == "out1" },
                       "outbound to unknown recipient is not published")
        // Cursor still commits past the row.
        let state = try store.load()
        XCTAssertFalse(state.sources["messages"]?.cursor.isEmpty ?? true,
                       "cursor committed even though the row was dropped")
    }

    func testTickDropsUnresolvableOutboundAndAdvances() async throws {
        let dbURL = try makeChatDBWithUnresolvableOutbound()
        let (plugin, sink, store) = try makeOutboundPlugin(dbURL: dbURL, known: ["+15551234567"])
        try await plugin.tick()
        let events = await sink.allEvents()
        XCTAssertFalse(events.contains { $0.sourceID == "orph1" },
                       "unresolvable outbound row not published")
        let state = try store.load()
        let cursor = try XCTUnwrap(MessagesCursorCodec.decode(state.sources["messages"]?.cursor ?? ""))
        XCTAssertTrue(cursor.backfillComplete || (cursor.backfillCursor ?? 1) == 0,
                      "backfill walked past the unresolvable row (cursor advanced)")
    }

    func testResolverFailureHoldsCursor() async throws {
        let dbURL = try makeChatDBWithOutbound1to1(peer: "+15551234567")
        // Seed an established (non-fresh) cursor: install-max already
        // captured, backfill mid-walk just above the outbound row at
        // ROWID 1. The ONLY thing that could advance is the backfill
        // row-walk over ROWID 1 — which the throwing resolver blocks.
        let seeded = MessagesCursor(
            backfillCursor: 2,
            liveCursor: 2,
            installMaxRowID: 2,
            backfillFloorSentAt: backfillFloor,
            backfillComplete: false)
        let seededJSON = try MessagesCursorCodec.encode(seeded)

        struct ResolverBoom: Error {}
        let throwing: @Sendable (GRDB.Database, [ChatDBMessage]) throws -> [String: String] = { _, _ in
            throw ResolverBoom()
        }
        let store = makeStateStore()
        try store.save(DaemonState(schemaVersion: 1))
        let sink = OutboundPublisherSink()
        let publisher = MessagesPublisher(
            sender: { _, body in
                await sink.record(body.events)
                return IngestEventsData(accepted: body.events.count, duplicate: 0, rejected: 0, errors: [])
            },
            auth: auth, logger: NoopLogger())
        let transport = StatefulCursorTransport(initialCursor: seededJSON)
        let piClient = PiClient(
            baseURL: URL(string: "https://pi.example.test")!,
            transport: transport.asTransport(),
            sleep: { _ in })
        let plugin = MessagesSourcePlugin(
            tickInterval: 60,
            config: MessagesSourceConfig(chatDBPath: dbURL, backfillFloor: backfillFloor),
            piClient: piClient,
            auth: auth,
            mutator: StateMutator(store: store),
            publisher: publisher,
            cache: KnownIdentifiersCache(initial: ["+15551234567"]),
            healthRegistry: SourceHealthRegistry(),
            logger: NoopLogger(),
            outboundPeerResolver: throwing)

        try await plugin.tick()

        let events = await sink.allEvents()
        XCTAssertTrue(events.isEmpty, "no publishes when resolution throws")
        // The backfill cursor was NOT advanced past the outbound row:
        // the throwing read returns without advancing, so the held row
        // is retried next tick.
        let held = transport.currentDecodedCursor()
        XCTAssertEqual(held?.backfillCursor, 2,
                       "backfill cursor held at the pre-read coordinate")
        XCTAssertEqual(held?.backfillComplete, false)

        // Recovery: a SECOND plugin over the same state store + a fresh
        // Pi script with the default resolver (≈ the next tick once the
        // transient condition clears) emits the held row and commits.
        let sink2 = OutboundPublisherSink()
        let publisher2 = MessagesPublisher(
            sender: { _, body in
                await sink2.record(body.events)
                return IngestEventsData(accepted: body.events.count, duplicate: 0, rejected: 0, errors: [])
            },
            auth: auth, logger: NoopLogger())
        let transport2 = StatefulCursorTransport(initialCursor: seededJSON)
        let piClient2 = PiClient(
            baseURL: URL(string: "https://pi.example.test")!,
            transport: transport2.asTransport(),
            sleep: { _ in })
        let plugin2 = MessagesSourcePlugin(
            tickInterval: 60,
            config: MessagesSourceConfig(chatDBPath: dbURL, backfillFloor: backfillFloor),
            piClient: piClient2,
            auth: auth,
            mutator: StateMutator(store: store),
            publisher: publisher2,
            cache: KnownIdentifiersCache(initial: ["+15551234567"]),
            healthRegistry: SourceHealthRegistry(),
            logger: NoopLogger())
        try await plugin2.tick()
        let recovered = await sink2.allEvents()
        XCTAssertTrue(recovered.contains { $0.sourceID == "out1" },
                      "the held outbound row is emitted once the transient condition clears")
    }

    func testAllSkippedPageConsumesBudget() async throws {
        // A page of contentless system rows: zero publishes, but the
        // budget is consumed on inspected rows so the backfill walks
        // past them and completes rather than re-reading forever.
        let dbURL = try makeChatDBWithSystemRowsOnly(count: 3)
        let (plugin, sink, store) = try makeOutboundPlugin(dbURL: dbURL, known: ["+15551234567"])
        try await plugin.tick()
        let events = await sink.allEvents()
        XCTAssertTrue(events.isEmpty, "system rows produce no events")
        let state = try store.load()
        let cursor = try XCTUnwrap(MessagesCursorCodec.decode(state.sources["messages"]?.cursor ?? ""))
        XCTAssertTrue(cursor.backfillComplete,
                      "all-skipped page consumed budget and the backfill walked to completion")
    }

    // MARK: - helpers

    /// Pi client whose only configured response is a fresh-install
    /// cursor GET (empty cursor, epoch 0, not complete). Used in the
    /// no-op tick smoke test where the rest of the path is irrelevant.
    private func makePiClientThatNeverFiresIngestPath() -> PiClient {
        let script = MessagesSourcePluginTestScript([
            .respond(status: 200, data: Data(
                #"{"success":true,"data":{"cursor":"","cursor_epoch":0,"backfill_complete":false}}"#.utf8)),
        ])
        return PiClient(
            baseURL: URL(string: "https://pi.example.test")!,
            transport: script.asTransport(),
            sleep: { _ in })
    }
}

/// Records the IngestEvents the publisher sent, in order, and decodes
/// individual payloads by source_id for outbound assertions.
actor OutboundPublisherSink {
    private var events: [IngestEvent] = []
    func record(_ batch: [IngestEvent]) { events.append(contentsOf: batch) }
    func allEvents() -> [IngestEvent] { events }

    /// The `peer_handle` field of the event with the given source_id,
    /// parsed from its raw payload JSON. Returns a Sendable String so it
    /// can cross the actor boundary.
    func peerHandle(forSourceID id: String) -> String? {
        guard let event = events.first(where: { $0.sourceID == id }),
              let obj = (try? JSONSerialization.jsonObject(with: event.payload.bytes)) as? [String: Any] else {
            return nil
        }
        return obj["peer_handle"] as? String
    }
}

/// Local mini-mock — the CRMMacPiClientTests MockTransportScript is in
/// another test target and SwiftPM doesn't share test code across
/// targets.  We re-declare a minimal version here.
final class MessagesSourcePluginTestScript: @unchecked Sendable {
    enum Step: Sendable {
        case respond(status: Int, data: Data)
    }
    private var steps: [Step]
    init(_ steps: [Step]) { self.steps = steps }

    func asTransport() -> TransportFunc {
        return { request in
            guard !self.steps.isEmpty else {
                throw URLError(.unknown)
            }
            let step = self.steps.removeFirst()
            switch step {
            case .respond(let status, let data):
                let url = request.url ?? URL(string: "https://test.invalid")!
                let response = HTTPURLResponse(
                    url: url,
                    statusCode: status,
                    httpVersion: "HTTP/1.1",
                    headerFields: ["Content-Type": "application/json"])!
                return (data, response)
            }
        }
    }
}
