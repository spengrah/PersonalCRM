// GroupChatCaptureTests observe which group-chat messages a tick publishes
// and which peer each carries, through the ingest payload, over a chat.db
// fixture. Synthetic handles only.
import XCTest
import Foundation
import GRDB
import CRMMacCore
import CRMMacPiClient
@testable import CRMMacMessagesSource

final class GroupChatCaptureTests: XCTestCase {
    private let auth = PiAuth(
        hostID: UUID(uuidString: "11111111-2222-3333-4444-555555555555")!,
        apiKey: "k")
    private let backfillFloor = MessagesCursorWire.defaultBackfillFloor
    private let sentUnix: TimeInterval = 1_778_686_938 // 2026-05-13
    private let dayInSeconds: TimeInterval = 24 * 60 * 60
    private var tempDir: URL!

    // Known contact A, non-contacts B and C, and A under another spelling.
    private let handleA = "+15551110000"
    private let handleB = "+15552220000"
    private let handleC = "+15553330000"
    private let handleAAlt = "5551110000"

    override func setUpWithError() throws {
        try super.setUpWithError()
        tempDir = FileManager.default.temporaryDirectory
            .appendingPathComponent("crm-mac-group-capture-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: tempDir, withIntermediateDirectories: true)
    }

    override func tearDownWithError() throws {
        try? FileManager.default.removeItem(at: tempDir)
        try super.tearDownWithError()
    }

    // MARK: - fixture

    private struct Row {
        var rowID: Int64
        var guid: String
        var chatRowID: Int64
        var senderHandleRowID: Int64?   // nil = outbound
        var offset: TimeInterval = 0    // seconds from sentUnix
    }

    /// Handles 1..4 = A, B, C, A-alt. Chats: 20 group {A,B}; 30 group {B,C};
    /// 40 group {A-alt,C}; 10 one-to-one {A}; 11 one-to-one {B}.
    private func seedChatDB(_ rows: [Row]) throws -> URL {
        let dbURL = tempDir.appendingPathComponent("chat.db")
        let schemaURL = try XCTUnwrap(Bundle.module.url(
            forResource: "chat_db_schema", withExtension: "sql", subdirectory: "Fixtures"))
        let script = try String(contentsOf: schemaURL, encoding: .utf8)
        let handles = [handleA, handleB, handleC, handleAAlt]
        let chats: [(Int64, String, Int, [Int64])] = [
            (20, "group-known", 43, [1, 2]),
            (30, "group-none", 43, [2, 3]),
            (40, "group-alt", 43, [4, 3]),
            (10, "one-a", 45, [1]),
            (11, "one-b", 45, [2]),
        ]
        let queue = try DatabaseQueue(path: dbURL.path)
        let base = sentUnix
        try queue.write { db in
            try db.execute(sql: script)
            for (i, h) in handles.enumerated() {
                try db.execute(sql:
                    "INSERT INTO handle (ROWID, id, service) VALUES (?, ?, 'iMessage')",
                    arguments: [i + 1, h])
            }
            for (rowID, guid, style, members) in chats {
                try db.execute(sql:
                    "INSERT INTO chat (ROWID, guid, style, chat_identifier) VALUES (?, ?, ?, ?)",
                    arguments: [rowID, guid, style, guid])
                for m in members {
                    try db.execute(sql:
                        "INSERT INTO chat_handle_join (chat_id, handle_id) VALUES (?, ?)",
                        arguments: [rowID, m])
                }
            }
            for row in rows {
                let nanos = Int64((base + row.offset - 978_307_200) * 1e9)
                try db.execute(sql: """
                    INSERT INTO message (ROWID, guid, text, handle_id, date, is_from_me,
                                         item_type, cache_has_attachments, associated_message_guid)
                    VALUES (?, ?, 'hello', ?, ?, ?, 0, 0, NULL)
                    """, arguments: [
                        row.rowID, row.guid,
                        row.senderHandleRowID as DatabaseValueConvertible? ?? DatabaseValue.null,
                        nanos, row.senderHandleRowID == nil ? 1 : 0,
                    ])
                try db.execute(sql:
                    "INSERT INTO chat_message_join (chat_id, message_id) VALUES (?, ?)",
                    arguments: [row.chatRowID, row.rowID])
            }
        }
        return dbURL
    }

    /// What a tick published for one row.
    private struct Published {
        let kind: String
        let peerHandle: String
        let isGroup: Bool
        let chatID: String
    }

    private actor Sink {
        private var events: [IngestEvent] = []
        func record(_ batch: [IngestEvent]) { events.append(contentsOf: batch) }
        func published() -> [String: Published] {
            var out: [String: Published] = [:]
            for e in events {
                guard let obj = (try? JSONSerialization.jsonObject(with: e.payload.bytes))
                        as? [String: Any] else { continue }
                out[e.sourceID] = Published(
                    kind: e.kind,
                    peerHandle: obj["peer_handle"] as? String ?? "<absent>",
                    isGroup: obj["is_group"] as? Bool ?? false,
                    chatID: obj["chat_id"] as? String ?? "")
            }
            return out
        }
    }

    private func makePlugin(
        dbURL: URL, cache: KnownIdentifiersCache, transport: StatefulCursorTransport,
        sink: Sink, clock: @escaping @Sendable () -> Date = { Date() }
    ) -> MessagesSourcePlugin {
        let store = StateStore(fileURL: tempDir.appendingPathComponent("state.json"))
        try? store.save(DaemonState(schemaVersion: 1))
        let piClient = PiClient(
            baseURL: URL(string: "https://pi.example.test")!,
            transport: transport.asTransport(),
            sleep: { _ in })
        let publisher = MessagesPublisher(
            sender: { _, body in
                await sink.record(body.events)
                return IngestEventsData(
                    accepted: body.events.count, duplicate: 0, rejected: 0, errors: [])
            },
            auth: auth, logger: NoopLogger())
        return MessagesSourcePlugin(
            tickInterval: 60,
            config: MessagesSourceConfig(chatDBPath: dbURL, backfillFloor: backfillFloor),
            piClient: piClient,
            auth: auth,
            mutator: StateMutator(store: store),
            publisher: publisher,
            cache: cache,
            healthRegistry: SourceHealthRegistry(),
            logger: NoopLogger(),
            clock: clock)
    }

    /// Fresh-install tick: the sentinel at the highest ROWID becomes the
    /// install max, so every other row is published by the backfill walk.
    private func backfillTick(rows: [Row], known: Set<String>) async throws -> [String: Published] {
        let sentinel = Row(rowID: 999, guid: "sentinel", chatRowID: 10, senderHandleRowID: 1)
        let dbURL = try seedChatDB(rows + [sentinel])
        let sink = Sink()
        let plugin = makePlugin(
            dbURL: dbURL, cache: KnownIdentifiersCache(initial: known),
            transport: StatefulCursorTransport(), sink: sink)
        try await plugin.tick()
        return await sink.published()
    }

    // MARK: - c1: tick publishes group messages of chats with a known member

    func testInboundGroupMessageFromNonContactPublishedWithSenderHandle() async throws {
        let out = try await backfillTick(
            rows: [Row(rowID: 1, guid: "gk-in-b", chatRowID: 20, senderHandleRowID: 2)],
            known: [handleA])
        let p = try XCTUnwrap(out["gk-in-b"], "non-contact group message published")
        XCTAssertEqual(p.kind, "raw_message.received")
        XCTAssertEqual(p.peerHandle, handleB)
        XCTAssertTrue(p.isGroup)
        XCTAssertEqual(p.chatID, "group-known")
    }

    func testInboundGroupMessageFromKnownContactPublishedAsAtBase() async throws {
        let out = try await backfillTick(
            rows: [Row(rowID: 1, guid: "gk-in-a", chatRowID: 20, senderHandleRowID: 1)],
            known: [handleA])
        let p = try XCTUnwrap(out["gk-in-a"])
        XCTAssertEqual(p.kind, "raw_message.received")
        XCTAssertEqual(p.peerHandle, handleA)
        XCTAssertTrue(p.isGroup)
    }

    func testGroupWithNoKnownMemberPublishesNothing() async throws {
        let out = try await backfillTick(
            rows: [
                Row(rowID: 1, guid: "gn-in-b", chatRowID: 30, senderHandleRowID: 2),
                Row(rowID: 2, guid: "gn-out", chatRowID: 30, senderHandleRowID: nil),
            ],
            known: [handleA])
        XCTAssertNil(out["gn-in-b"])
        XCTAssertNil(out["gn-out"])
    }

    func testMemberUnderAnotherSpellingCountsAsKnown() async throws {
        let out = try await backfillTick(
            rows: [
                Row(rowID: 1, guid: "ga-in-c", chatRowID: 40, senderHandleRowID: 3),
                Row(rowID: 2, guid: "ga-out", chatRowID: 40, senderHandleRowID: nil),
            ],
            known: [handleA])
        XCTAssertEqual(out["ga-in-c"]?.peerHandle, handleC)
        XCTAssertEqual(out["ga-out"]?.kind, "raw_message.sent")
    }

    func testOutboundGroupMessagePublishedWithNoMemberPeer() async throws {
        let out = try await backfillTick(
            rows: [Row(rowID: 1, guid: "gk-out", chatRowID: 20, senderHandleRowID: nil)],
            known: [handleA])
        let p = try XCTUnwrap(out["gk-out"])
        XCTAssertEqual(p.kind, "raw_message.sent")
        XCTAssertTrue(p.isGroup)
        XCTAssertEqual(p.chatID, "group-known")
        XCTAssertEqual(p.peerHandle, "", "an outbound group message names no member")
    }

    func testOutboundOneToOneToKnownContactNamesThatPeer() async throws {
        let out = try await backfillTick(
            rows: [Row(rowID: 1, guid: "one-out-a", chatRowID: 10, senderHandleRowID: nil)],
            known: [handleA])
        let p = try XCTUnwrap(out["one-out-a"])
        XCTAssertEqual(p.kind, "raw_message.sent")
        XCTAssertEqual(p.peerHandle, handleA)
        XCTAssertFalse(p.isGroup)
    }

    func testOneToOneWithUnknownPeerIsNotPublished() async throws {
        let out = try await backfillTick(
            rows: [
                Row(rowID: 1, guid: "one-out-b", chatRowID: 11, senderHandleRowID: nil),
                Row(rowID: 2, guid: "one-in-b", chatRowID: 11, senderHandleRowID: 2),
            ],
            known: [handleA])
        XCTAssertNil(out["one-out-b"])
        XCTAssertNil(out["one-in-b"])
    }

    func testLiveTickPublishesGroupMessagesUnderTheSameRule() async throws {
        // Established install: the live cursor sits below both group rows.
        let dbURL = try seedChatDB([
            Row(rowID: 1, guid: "old", chatRowID: 10, senderHandleRowID: 1),
            Row(rowID: 2, guid: "live-in-b", chatRowID: 20, senderHandleRowID: 2),
            Row(rowID: 3, guid: "live-out", chatRowID: 20, senderHandleRowID: nil),
            Row(rowID: 4, guid: "live-none", chatRowID: 30, senderHandleRowID: 2),
        ])
        let established = MessagesCursor(
            backfillCursor: 0, liveCursor: 1, installMaxRowID: 1,
            backfillFloorSentAt: backfillFloor, backfillComplete: true,
            outboundBackfillDone: true)
        let sink = Sink()
        let plugin = makePlugin(
            dbURL: dbURL, cache: KnownIdentifiersCache(initial: [handleA]),
            transport: StatefulCursorTransport(initialCursor: try MessagesCursorCodec.encode(established)),
            sink: sink)
        try await plugin.tick()
        let out = await sink.published()
        XCTAssertEqual(out["live-in-b"]?.peerHandle, handleB)
        XCTAssertEqual(out["live-in-b"]?.kind, "raw_message.received")
        XCTAssertEqual(out["live-out"]?.peerHandle, "")
        XCTAssertEqual(out["live-out"]?.kind, "raw_message.sent")
        XCTAssertNil(out["live-none"])
    }

    // MARK: - c1: a newly-known contact's scan publishes its group chats' messages

    private func scanTick(rows: [Row]) async throws -> [String: Published] {
        let dbURL = try seedChatDB(rows)
        let now = Date(timeIntervalSince1970: sentUnix + 10 * dayInSeconds)
        // Backfill complete and live cursor past every row, so only the
        // scan publishes.
        let inactive = MessagesCursor(
            backfillCursor: 0, liveCursor: 100_000, installMaxRowID: 100_000,
            backfillFloorSentAt: backfillFloor, backfillComplete: true,
            outboundBackfillDone: true)
        let cache = KnownIdentifiersCache(baselines: [.messages: []], consumers: [.messages])
        await cache.replace(with: [handleA])
        let sink = Sink()
        let plugin = makePlugin(
            dbURL: dbURL, cache: cache,
            transport: StatefulCursorTransport(initialCursor: try MessagesCursorCodec.encode(inactive)),
            sink: sink, clock: { now })
        try await plugin.tick()
        return await sink.published()
    }

    func testNewlyKnownScanPublishesNonContactAndOutboundGroupMessagesInWindow() async throws {
        let out = try await scanTick(rows: [
            Row(rowID: 1, guid: "scan-in-b", chatRowID: 20, senderHandleRowID: 2),
            Row(rowID: 2, guid: "scan-out", chatRowID: 20, senderHandleRowID: nil),
        ])
        let inbound = try XCTUnwrap(out["scan-in-b"], "non-contact group message in the window")
        XCTAssertEqual(inbound.kind, "raw_message.received")
        XCTAssertEqual(inbound.peerHandle, handleB)
        let outbound = try XCTUnwrap(out["scan-out"])
        XCTAssertEqual(outbound.kind, "raw_message.sent")
        XCTAssertEqual(outbound.peerHandle, "")
    }

    func testNewlyKnownScanSkipsGroupMessagesBeforeTheWindow() async throws {
        let out = try await scanTick(rows: [
            Row(rowID: 1, guid: "old-in-b", chatRowID: 20, senderHandleRowID: 2,
                offset: -60 * dayInSeconds),
            Row(rowID: 2, guid: "old-out", chatRowID: 20, senderHandleRowID: nil,
                offset: -60 * dayInSeconds),
            Row(rowID: 3, guid: "new-in-b", chatRowID: 20, senderHandleRowID: 2),
        ])
        XCTAssertNil(out["old-in-b"])
        XCTAssertNil(out["old-out"])
        XCTAssertNotNil(out["new-in-b"])
    }
}
