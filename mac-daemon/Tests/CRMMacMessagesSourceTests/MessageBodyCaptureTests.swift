// MessageBodyCaptureTests observe the body and message_type a live tick
// publishes for each shape of chat.db row: plain `message.text`,
// rich-text `message.attributedBody`, both, neither, and placeholder-only.
import XCTest
import Foundation
import GRDB
import CRMMacCore
import CRMMacPiClient
@testable import CRMMacMessagesSource

final class MessageBodyCaptureTests: XCTestCase {
    private let auth = PiAuth(
        hostID: UUID(uuidString: "11111111-2222-3333-4444-555555555555")!,
        apiKey: "k")
    private let unix2026: TimeInterval = 1_778_686_938
    private var tempDir: URL!

    override func setUpWithError() throws {
        try super.setUpWithError()
        tempDir = FileManager.default.temporaryDirectory
            .appendingPathComponent("crm-mac-body-capture-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: tempDir, withIntermediateDirectories: true)
    }

    override func tearDownWithError() throws {
        try? FileManager.default.removeItem(at: tempDir)
        try super.tearDownWithError()
    }

    /// One seeded message row. `guid` doubles as the published source_id.
    private struct SeedRow {
        var guid: String
        var text: String?
        var attributedBody: Data?
        var itemType: Int = 0
        var attachmentUTI: String?
        var associatedGUID: String?
    }

    /// What a tick published for one row.
    private struct Published {
        let kind: String
        let text: String?
        let messageType: String
    }

    /// Typedstream bytes shaped like the ones Messages.app stores in
    /// `message.attributedBody` for an NSAttributedString holding `body`.
    private func typedstream(_ body: String) -> Data {
        let utf8 = Array(body.utf8)
        var bytes: [UInt8] = [0x04, 0x0B]
        bytes += Array("streamtyped".utf8)
        bytes += [0x81, 0xE8, 0x03, 0x84, 0x01, 0x40, 0x84, 0x84, 0x84, 0x12]
        bytes += Array("NSAttributedString".utf8)
        bytes += [0x00, 0x84, 0x84, 0x08]
        bytes += Array("NSObject".utf8)
        bytes += [0x00, 0x85, 0x92, 0x84, 0x84, 0x84, 0x08]
        bytes += Array("NSString".utf8)
        bytes += [0x01, 0x94, 0x84, 0x01, 0x2B]
        if utf8.count < 0x80 {
            bytes.append(UInt8(utf8.count))
        } else {
            bytes += [0x81, UInt8(utf8.count & 0xFF), UInt8((utf8.count >> 8) & 0xFF)]
        }
        bytes += utf8
        bytes += [0x86, 0x84, 0x02]
        bytes += Array("iI".utf8)
        bytes += [0x01, UInt8(body.utf16.count), 0x92, 0x84, 0x84, 0x84, 0x0C]
        bytes += Array("NSDictionary".utf8)
        bytes += [0x00, 0x94, 0x84, 0x01, 0x69, 0x01, 0x92, 0x84, 0x96, 0x96, 0x1D]
        bytes += Array("__kIMMessagePartAttributeName".utf8)
        bytes += [0x86, 0x86, 0x86]
        return Data(bytes)
    }

    /// Sync so the GRDB write closure is not inferred `@Sendable`.
    private func seedChatDB(_ rows: [SeedRow]) throws -> URL {
        let dbURL = tempDir.appendingPathComponent("chat.db")
        let schemaURL = try XCTUnwrap(Bundle.module.url(
            forResource: "chat_db_schema", withExtension: "sql", subdirectory: "Fixtures"))
        let script = try String(contentsOf: schemaURL, encoding: .utf8)
        let queue = try DatabaseQueue(path: dbURL.path)
        let appleNanos = Int64((unix2026 - 978_307_200) * 1e9)
        try queue.write { db in
            try db.execute(sql: script)
            try db.execute(sql:
                "INSERT INTO handle (ROWID, id, service) VALUES (1, '+15551234567', 'iMessage')")
            try db.execute(sql:
                "INSERT INTO chat (ROWID, guid, style, chat_identifier) " +
                "VALUES (10, 'chat-1to1', 45, '+15551234567')")
            try db.execute(sql:
                "INSERT INTO chat_handle_join (chat_id, handle_id) VALUES (10, 1)")
            var rowID: Int64 = 0
            for row in rows {
                rowID += 1
                try db.execute(sql:
                    "INSERT INTO message (ROWID, guid, text, attributedBody, handle_id, date, " +
                    "is_from_me, item_type, cache_has_attachments, associated_message_guid) " +
                    "VALUES (?, ?, ?, ?, 1, ?, 0, ?, ?, ?)",
                    arguments: [rowID, row.guid, row.text, row.attributedBody, appleNanos,
                                row.itemType, row.attachmentUTI == nil ? 0 : 1,
                                row.associatedGUID])
                try db.execute(sql:
                    "INSERT INTO chat_message_join (chat_id, message_id) VALUES (10, ?)",
                    arguments: [rowID])
                if let uti = row.attachmentUTI {
                    try db.execute(sql:
                        "INSERT INTO attachment (ROWID, guid, uti) VALUES (?, ?, ?)",
                        arguments: [rowID, "att-\(row.guid)", uti])
                    try db.execute(sql:
                        "INSERT INTO message_attachment_join (message_id, attachment_id) VALUES (?, ?)",
                        arguments: [rowID, rowID])
                }
            }
            // Sentinel at the install max so every seeded row is backfilled.
            try db.execute(sql:
                "INSERT INTO message (ROWID, guid, text, handle_id, date, is_from_me) " +
                "VALUES (?, 'sentinel', 'sentinel', 1, ?, 0)",
                arguments: [rowID + 1, appleNanos])
        }
        return dbURL
    }

    /// Seed `rows` (backfilled, since a trailing sentinel row sits at the
    /// install max), run one tick, return what was published per guid.
    private func publish(_ rows: [SeedRow]) async throws -> [String: Published] {
        let dbURL = try seedChatDB(rows)
        let store = StateStore(fileURL: tempDir.appendingPathComponent("state.json"))
        try store.save(DaemonState(schemaVersion: 1))
        let sink = OutboundPublisherSink()
        let publisher = MessagesPublisher(
            sender: { _, body in
                await sink.record(body.events)
                return IngestEventsData(
                    accepted: body.events.count, duplicate: 0, rejected: 0, errors: [])
            },
            auth: auth, logger: NoopLogger())
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
        let plugin = MessagesSourcePlugin(
            tickInterval: 60,
            config: MessagesSourceConfig(
                chatDBPath: dbURL,
                backfillFloor: MessagesCursorWire.defaultBackfillFloor),
            piClient: piClient,
            auth: auth,
            mutator: StateMutator(store: store),
            publisher: publisher,
            cache: KnownIdentifiersCache(initial: ["+15551234567"]),
            healthRegistry: SourceHealthRegistry(),
            logger: NoopLogger())

        try await plugin.performTick()

        var out: [String: Published] = [:]
        for event in await sink.allEvents() {
            let obj = try XCTUnwrap(
                try JSONSerialization.jsonObject(with: event.payload.bytes) as? [String: Any])
            out[event.sourceID] = Published(
                kind: event.kind,
                text: obj["text"] as? String,
                messageType: try XCTUnwrap(obj["message_type"] as? String))
        }
        return out
    }

    func testBodyOnlyInAttributedBodyIsPublishedAsText() async throws {
        let published = try await publish([
            SeedRow(guid: "rich", text: nil, attributedBody: typedstream("hello from rich text")),
        ])
        let message = try XCTUnwrap(published["rich"], "row published")
        XCTAssertEqual(message.text, "hello from rich text")
        XCTAssertEqual(message.messageType, "text")
    }

    func testBodyOnlyInTextIsPublishedWithThatBody() async throws {
        let published = try await publish([
            SeedRow(guid: "plain", text: "plain body", attributedBody: nil),
        ])
        let message = try XCTUnwrap(published["plain"], "row published")
        XCTAssertEqual(message.text, "plain body")
        XCTAssertEqual(message.messageType, "text")
    }

    func testPlainTextWinsWhenBothColumnsHoldBodyText() async throws {
        let published = try await publish([
            SeedRow(guid: "both", text: "plain wins",
                    attributedBody: typedstream("rich loses")),
        ])
        XCTAssertEqual(try XCTUnwrap(published["both"], "row published").text, "plain wins")
    }

    func testAttachmentOnlyRichTextHoldingPlaceholderIsNoBodyAndTypedByAttachment() async throws {
        let published = try await publish([
            SeedRow(guid: "photo", text: nil,
                    attributedBody: typedstream("\u{FFFC}"),
                    attachmentUTI: "public.jpeg"),
        ])
        let message = try XCTUnwrap(published["photo"], "row published")
        XCTAssertNil(message.text)
        XCTAssertEqual(message.messageType, "photo")
    }

    func testReactionStoringNoBodyIsPublishedWithNoBody() async throws {
        let published = try await publish([
            SeedRow(guid: "reaction", text: nil, attributedBody: nil,
                    associatedGUID: "p:0/some-target"),
        ])
        let message = try XCTUnwrap(published["reaction"], "row published")
        XCTAssertNil(message.text)
    }

    func testUndecodableAttributedBodyWithNoTextIsStillPublishedWithNoBody() async throws {
        let published = try await publish([
            SeedRow(guid: "garbled", text: nil,
                    attributedBody: Data([0x00, 0x01, 0x02, 0x03])),
        ])
        let message = try XCTUnwrap(published["garbled"], "row published")
        XCTAssertNil(message.text)
    }

    func testNonMessageItemTypeRowWithOnlyRichTextIsNotPublished() async throws {
        let published = try await publish([
            SeedRow(guid: "system", text: nil,
                    attributedBody: typedstream("group renamed"), itemType: 2),
            SeedRow(guid: "real", text: "kept", attributedBody: nil),
        ])
        XCTAssertNil(published["system"], "non-message item-type row stays unpublished")
        XCTAssertNotNil(published["real"], "ordinary row alongside it is published")
    }
}
