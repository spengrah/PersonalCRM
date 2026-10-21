// ReactionCaptureTests observe the `reaction` object a live tick publishes
// for each shape of chat.db reaction row, and that the rest of the payload
// is published as it was before reactions carried facts. Synthetic handles
// and guids only.
import XCTest
import Foundation
import GRDB
import CRMMacCore
import CRMMacPiClient
@testable import CRMMacMessagesSource

final class ReactionCaptureTests: XCTestCase {
    private let auth = PiAuth(
        hostID: UUID(uuidString: "11111111-2222-3333-4444-555555555555")!,
        apiKey: "k")
    private let sentUnix: TimeInterval = 1_778_686_938 // 2026-05-13
    private let handleA = "+15551110000"
    private let handleB = "+15552220000"
    private let target = "AAAAAAAA-1111-2222-3333-444444444444"
    private var tempDir: URL!

    override func setUpWithError() throws {
        try super.setUpWithError()
        tempDir = FileManager.default.temporaryDirectory
            .appendingPathComponent("crm-mac-reaction-capture-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: tempDir, withIntermediateDirectories: true)
    }

    override func tearDownWithError() throws {
        try? FileManager.default.removeItem(at: tempDir)
        try super.tearDownWithError()
    }

    // MARK: - fixture

    private enum Chat { case oneToOne, group }

    private struct Row {
        var guid: String
        var type: Int
        var associatedGUID: String?
        var emoji: String?
        var text: String? = "Loved a message"
        var outbound = false
        var chat = Chat.oneToOne
    }

    /// Handles 1 = A (known), 2 = B. Chat 10 is one-to-one with A; chat 20 is
    /// a group of A and B. Inbound rows come from A.
    private func seedChatDB(_ rows: [Row]) throws -> URL {
        let dbURL = tempDir.appendingPathComponent("chat.db")
        let schemaURL = try XCTUnwrap(Bundle.module.url(
            forResource: "chat_db_schema", withExtension: "sql", subdirectory: "Fixtures"))
        let script = try String(contentsOf: schemaURL, encoding: .utf8)
        let nanos = Int64((sentUnix - 978_307_200) * 1e9)
        let queue = try DatabaseQueue(path: dbURL.path)
        try queue.write { db in
            try db.execute(sql: script)
            for (i, h) in [handleA, handleB].enumerated() {
                try db.execute(sql:
                    "INSERT INTO handle (ROWID, id, service) VALUES (?, ?, 'iMessage')",
                    arguments: [i + 1, h])
            }
            for (rowID, guid, style, members) in [(10, "one-a", 45, [1]), (20, "group-ab", 43, [1, 2])] {
                try db.execute(sql:
                    "INSERT INTO chat (ROWID, guid, style, chat_identifier) VALUES (?, ?, ?, ?)",
                    arguments: [rowID, guid, style, guid])
                for m in members {
                    try db.execute(sql:
                        "INSERT INTO chat_handle_join (chat_id, handle_id) VALUES (?, ?)",
                        arguments: [rowID, m])
                }
            }
            for (i, row) in rows.enumerated() {
                let rowID = Int64(i + 1)
                try db.execute(sql: """
                    INSERT INTO message (ROWID, guid, text, handle_id, date, is_from_me, item_type,
                                         cache_has_attachments, associated_message_guid,
                                         associated_message_type, associated_message_emoji)
                    VALUES (?, ?, ?, ?, ?, ?, 0, 0, ?, ?, ?)
                    """, arguments: [
                        rowID, row.guid, row.text, row.outbound ? nil : 1, nanos,
                        row.outbound ? 1 : 0, row.associatedGUID, row.type, row.emoji,
                    ])
                try db.execute(sql:
                    "INSERT INTO chat_message_join (chat_id, message_id) VALUES (?, ?)",
                    arguments: [row.chat == .group ? 20 : 10, rowID])
            }
        }
        return dbURL
    }

    private actor Sink {
        private var events: [IngestEvent] = []
        func record(_ batch: [IngestEvent]) { events.append(contentsOf: batch) }
        func all() -> [IngestEvent] { events }
    }

    /// What a live tick published for one row: its envelope kind and payload.
    private struct Published {
        let kind: String
        let payload: [String: Any]
        var reaction: [String: Any]? { payload["reaction"] as? [String: Any] }
    }

    /// Seed `rows` on an established install whose live cursor sits below
    /// them, run one tick, and return what it published per guid.
    private func liveTick(_ rows: [Row]) async throws -> [String: Published] {
        let dbURL = try seedChatDB(rows)
        let established = MessagesCursor(
            backfillCursor: 0, liveCursor: 0, installMaxRowID: 0,
            backfillFloorSentAt: MessagesCursorWire.defaultBackfillFloor, backfillComplete: true,
            outboundBackfillDone: true)
        let transport = StatefulCursorTransport(
            initialCursor: try MessagesCursorCodec.encode(established))
        let store = StateStore(fileURL: tempDir.appendingPathComponent("state.json"))
        try store.save(DaemonState(schemaVersion: 1))
        let sink = Sink()
        let publisher = MessagesPublisher(
            sender: { _, body in
                await sink.record(body.events)
                return IngestEventsData(
                    accepted: body.events.count, duplicate: 0, rejected: 0, errors: [])
            },
            auth: auth, logger: NoopLogger())
        let plugin = MessagesSourcePlugin(
            tickInterval: 60,
            config: MessagesSourceConfig(
                chatDBPath: dbURL, backfillFloor: MessagesCursorWire.defaultBackfillFloor),
            piClient: PiClient(
                baseURL: URL(string: "https://pi.example.test")!,
                transport: transport.asTransport(), sleep: { _ in }),
            auth: auth,
            mutator: StateMutator(store: store),
            publisher: publisher,
            cache: KnownIdentifiersCache(initial: [handleA]),
            healthRegistry: SourceHealthRegistry(),
            logger: NoopLogger())
        try await plugin.tick()

        var out: [String: Published] = [:]
        for event in await sink.all() {
            let obj = try XCTUnwrap(
                try JSONSerialization.jsonObject(with: event.payload.bytes) as? [String: Any])
            out[event.sourceID] = Published(kind: event.kind, payload: obj)
        }
        return out
    }

    /// The payload keys a message carried before reactions carried facts.
    private let baseKeys: Set<String> = [
        "version", "host_id", "source", "guid", "chat_id", "peer_handle", "text",
        "message_type", "is_group", "sent_at", "reply_to_guid",
    ]

    /// Publish `row` alone on a live tick and return what was published for it.
    private func publishOne(_ row: Row) async throws -> Published {
        let published = try await liveTick([row])
        return try XCTUnwrap(published[row.guid], "\(row.guid) published")
    }

    /// Asserts `published` carries exactly these facts, and that the rest of
    /// its payload is what the row published before: the same keys, its body,
    /// a text message type, and the raw associated guid as its reply link.
    private func assertFacts(
        _ published: Published, of row: Row, kind: String, emoji: String? = nil,
        removal: Bool, file: StaticString = #filePath, line: UInt = #line
    ) throws {
        let reaction = try XCTUnwrap(published.reaction, "reaction object published", file: file, line: line)
        var expectedKeys: Set<String> = ["kind", "target_key", "removal"]
        if emoji != nil { expectedKeys.insert("emoji") }
        XCTAssertEqual(Set(reaction.keys), expectedKeys, file: file, line: line)
        XCTAssertEqual(reaction["kind"] as? String, kind, file: file, line: line)
        XCTAssertEqual(reaction["emoji"] as? String, emoji, file: file, line: line)
        XCTAssertEqual(reaction["target_key"] as? String, target, file: file, line: line)
        XCTAssertEqual(reaction["removal"] as? Bool, removal, file: file, line: line)
        assertPublishedAsBefore(published, of: row, extraKeys: ["reaction"], file: file, line: line)
    }

    private func assertPublishedAsBefore(
        _ published: Published, of row: Row, extraKeys: Set<String> = [],
        file: StaticString = #filePath, line: UInt = #line
    ) {
        var keys = baseKeys.union(extraKeys)
        if row.associatedGUID == nil { keys.remove("reply_to_guid") }
        XCTAssertEqual(Set(published.payload.keys), keys, file: file, line: line)
        XCTAssertEqual(published.payload["text"] as? String, row.text, file: file, line: line)
        XCTAssertEqual(published.payload["message_type"] as? String, "text", file: file, line: line)
        XCTAssertEqual(published.payload["reply_to_guid"] as? String, row.associatedGUID, file: file, line: line)
    }

    private func reaction(_ type: Int, emoji: String? = nil) -> Row {
        Row(guid: "r-\(type)", type: type, associatedGUID: "p:0/\(target)", emoji: emoji)
    }

    // MARK: - each reaction type is published with its kind

    func testType2000PublishedAsLove() async throws {
        let row = reaction(2000)
        try assertFacts(try await publishOne(row), of: row, kind: "love", removal: false)
    }

    func testType2001PublishedAsLike() async throws {
        let row = reaction(2001)
        try assertFacts(try await publishOne(row), of: row, kind: "like", removal: false)
    }

    func testType2002PublishedAsDislike() async throws {
        let row = reaction(2002)
        try assertFacts(try await publishOne(row), of: row, kind: "dislike", removal: false)
    }

    func testType2003PublishedAsLaugh() async throws {
        let row = reaction(2003)
        try assertFacts(try await publishOne(row), of: row, kind: "laugh", removal: false)
    }

    func testType2004PublishedAsEmphasize() async throws {
        let row = reaction(2004)
        try assertFacts(try await publishOne(row), of: row, kind: "emphasize", removal: false)
    }

    func testType2005PublishedAsQuestion() async throws {
        let row = reaction(2005)
        try assertFacts(try await publishOne(row), of: row, kind: "question", removal: false)
    }

    func testType2006PublishedAsEmojiWithItsEmoji() async throws {
        let row = reaction(2006, emoji: "🎉")
        try assertFacts(try await publishOne(row), of: row, kind: "emoji", emoji: "🎉", removal: false)
    }

    func testType2007PublishedAsSticker() async throws {
        let row = reaction(2007)
        try assertFacts(try await publishOne(row), of: row, kind: "sticker", removal: false)
    }

    // MARK: - each removal type is published with its kind and removal true

    func testType3000PublishedAsLoveRemoval() async throws {
        let row = reaction(3000)
        try assertFacts(try await publishOne(row), of: row, kind: "love", removal: true)
    }

    func testType3001PublishedAsLikeRemoval() async throws {
        let row = reaction(3001)
        try assertFacts(try await publishOne(row), of: row, kind: "like", removal: true)
    }

    func testType3002PublishedAsDislikeRemoval() async throws {
        let row = reaction(3002)
        try assertFacts(try await publishOne(row), of: row, kind: "dislike", removal: true)
    }

    func testType3003PublishedAsLaughRemoval() async throws {
        let row = reaction(3003)
        try assertFacts(try await publishOne(row), of: row, kind: "laugh", removal: true)
    }

    func testType3004PublishedAsEmphasizeRemoval() async throws {
        let row = reaction(3004)
        try assertFacts(try await publishOne(row), of: row, kind: "emphasize", removal: true)
    }

    func testType3005PublishedAsQuestionRemoval() async throws {
        let row = reaction(3005)
        try assertFacts(try await publishOne(row), of: row, kind: "question", removal: true)
    }

    func testType3006PublishedAsEmojiRemovalWithItsEmoji() async throws {
        let row = reaction(3006, emoji: "🎉")
        try assertFacts(try await publishOne(row), of: row, kind: "emoji", emoji: "🎉", removal: true)
    }

    func testType3007PublishedAsStickerRemoval() async throws {
        let row = reaction(3007)
        try assertFacts(try await publishOne(row), of: row, kind: "sticker", removal: true)
    }

    // MARK: - a sticker placed on a message

    func testType1000WithATargetPublishedAsSticker() async throws {
        var row = reaction(1000)
        row.text = nil
        let published = try await publishOne(row)
        let facts = try XCTUnwrap(published.reaction, "reaction object published")
        XCTAssertEqual(facts["kind"] as? String, "sticker")
        XCTAssertEqual(facts["target_key"] as? String, target)
        XCTAssertEqual(facts["removal"] as? Bool, false)
        XCTAssertNil(facts["emoji"])
        XCTAssertNil(published.payload["text"])
        XCTAssertEqual(published.payload["reply_to_guid"] as? String, row.associatedGUID)
    }

    // MARK: - the target key loses its prefix

    func testPartPrefixedTargetPublishedWithoutThePrefix() async throws {
        var row = reaction(2001)
        row.associatedGUID = "p:2/\(target)"
        try assertFacts(try await publishOne(row), of: row, kind: "like", removal: false)
    }

    func testBPPrefixedTargetPublishedWithoutThePrefix() async throws {
        var row = reaction(2001)
        row.associatedGUID = "bp:\(target)"
        try assertFacts(try await publishOne(row), of: row, kind: "like", removal: false)
    }

    func testBareTargetPublishedAsIs() async throws {
        var row = reaction(2001)
        row.associatedGUID = target
        try assertFacts(try await publishOne(row), of: row, kind: "like", removal: false)
    }

    // MARK: - direction and chat shape

    func testInboundOneToOneReactionPublishedAsReceivedWithFacts() async throws {
        let row = reaction(2000)
        let published = try await publishOne(row)
        XCTAssertEqual(published.kind, "raw_message.received")
        XCTAssertEqual(published.payload["is_group"] as? Bool, false)
        try assertFacts(published, of: row, kind: "love", removal: false)
    }

    func testOutboundOneToOneReactionPublishedAsSentWithFacts() async throws {
        var row = reaction(2000)
        row.outbound = true
        let published = try await publishOne(row)
        XCTAssertEqual(published.kind, "raw_message.sent")
        XCTAssertEqual(published.payload["peer_handle"] as? String, handleA)
        XCTAssertEqual(published.payload["is_group"] as? Bool, false)
        try assertFacts(published, of: row, kind: "love", removal: false)
    }

    func testInboundGroupReactionPublishedWithFacts() async throws {
        var row = reaction(2003)
        row.chat = .group
        let published = try await publishOne(row)
        XCTAssertEqual(published.kind, "raw_message.received")
        XCTAssertEqual(published.payload["is_group"] as? Bool, true)
        try assertFacts(published, of: row, kind: "laugh", removal: false)
    }

    func testOutboundGroupReactionPublishedWithFacts() async throws {
        var row = reaction(2003)
        row.chat = .group
        row.outbound = true
        let published = try await publishOne(row)
        XCTAssertEqual(published.kind, "raw_message.sent")
        XCTAssertEqual(published.payload["peer_handle"] as? String, "")
        XCTAssertEqual(published.payload["is_group"] as? Bool, true)
        try assertFacts(published, of: row, kind: "laugh", removal: false)
    }

    // MARK: - a row that is not a reaction carries no reaction object

    func testType0PublishedWithNoReaction() async throws {
        let row = Row(guid: "plain", type: 0, associatedGUID: nil, text: "hello")
        let published = try await publishOne(row)
        XCTAssertNil(published.payload["reaction"])
        assertPublishedAsBefore(published, of: row)
    }

    func testType1000WithNoTargetPublishedWithNoReaction() async throws {
        let row = Row(guid: "sticker-alone", type: 1000, associatedGUID: nil, text: "sticker")
        let published = try await publishOne(row)
        XCTAssertNil(published.payload["reaction"])
        assertPublishedAsBefore(published, of: row)
    }

    func testType2008PublishedWithNoReaction() async throws {
        let row = reaction(2008)
        let published = try await publishOne(row)
        XCTAssertNil(published.payload["reaction"])
        assertPublishedAsBefore(published, of: row)
    }

    func testType3008PublishedWithNoReaction() async throws {
        let row = reaction(3008)
        let published = try await publishOne(row)
        XCTAssertNil(published.payload["reaction"])
        assertPublishedAsBefore(published, of: row)
    }

    func testType2006WithNoStoredEmojiPublishedWithNoReaction() async throws {
        let row = reaction(2006)
        let published = try await publishOne(row)
        XCTAssertNil(published.payload["reaction"])
        assertPublishedAsBefore(published, of: row)
    }
}
