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
