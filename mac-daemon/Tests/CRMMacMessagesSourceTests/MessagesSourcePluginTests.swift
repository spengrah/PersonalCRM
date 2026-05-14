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
    private let backfillFloor = Date(timeIntervalSince1970: 1_767_225_600) // 2026-01-01
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
        var config = Configuration()
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
        config = config // appease 'used' warning
        return dbURL
    }

    private func makeStateStore() -> StateStore {
        StateStore(fileURL: tempDir.appendingPathComponent("state.json"))
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
