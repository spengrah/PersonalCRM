// Test harness for the rescan-messages entry point: a temp home holding the
// daemon's config.json + api-key and a messages database, plus a fake Pi that
// answers the two enrichment routes at PiClient's transport boundary.
import XCTest
import Foundation
import GRDB
import CRMMacCore
import CRMMacPiClient
@testable import rescan_messages

let testHostID = UUID(uuidString: "11111111-2222-3333-4444-555555555555")!
let testAPIKey = "test-api-key"

/// One message row in the messages-database fixture.
struct SourceRow {
    var guid: String
    var text: String?
    var attributedBody: Data?
    var hasAttachment = false
    var associatedMessageType = 0
    /// `message.associated_message_guid`, the target of a reaction.
    var associatedGUID: String?
    var emoji: String?
    /// Set for a message of a chat: `chat` is the chat.guid, `handle` the
    /// sender's handle.id (inbound only), `sentAt` the sent time.
    var chat: String?
    var handle: String?
    var isFromMe = false
    var sentAt: Date?
}

/// Typedstream bytes shaped like the ones Messages.app stores in
/// `message.attributedBody` for an NSAttributedString holding `body`.
func typedstream(_ body: String) -> Data {
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
    return Data(bytes)
}

/// An `attributedBody` the extractor cannot decode.
let undecodableBlob = Data([0x04, 0x0B, 0x01, 0x02, 0x03])

/// A stored group-chat row on the fake Pi, beyond its body.
struct PiGroupRow {
    var chat: String
    var outbound: Bool
    /// Carries a member attribution (only an outbound row can lose it).
    var attributed: Bool
}

/// Fake Pi. Rows are guid -> stored body (nil/empty = body-less). Answers only
/// the enrichment routes for the paired host with its bearer key.
final class FakePi: @unchecked Sendable {
    enum Failure {
        case none, listing(Int), fill(Int), unreachable
        /// The group-chats listing answers 400, or cannot be reached.
        case groupListingRejected, groupListingUnreachable
        /// The nth group-item POST answers 400, or cannot be reached.
        case groupSubmission(Int), groupSubmissionUnreachable
        /// The without-reaction-facts listing answers 400, or cannot be reached.
        case reactionListingRejected, reactionListingUnreachable
        /// The nth set_reaction POST answers 400, or cannot be reached.
        case reactionSubmission(Int), reactionSubmissionUnreachable
    }

    private let lock = NSLock()
    private var bodies: [String: String]
    private let pageCap: Int
    private let failure: Failure
    private(set) var fillRequests: [[FillBodyItem]] = []
    private(set) var listRequests = 0
    /// Group chat -> the windows of its live messages interactions.
    private var groupChats: [String: [(start: Date, end: Date)]]
    private let groupPageCap: Int
    private var groupRows: [String: PiGroupRow]
    private(set) var groupListRequests = 0
    /// Raw JSON items of each POST that carried group ops, in request order.
    private(set) var groupRequests: [[[String: Any]]] = []
    /// guid -> the S1 reaction object stored on the row, for rows that carry facts.
    private var reactionFacts: [String: [String: Any]]
    private(set) var reactionListRequests = 0
    /// Raw JSON items of each POST that carried set_reaction ops, in request order.
    private(set) var reactionRequests: [[[String: Any]]] = []
    /// Runs under the Pi's lock right after a listing is answered, so a test
    /// can change Pi rows between the listing and the submission.
    var afterListing: (@Sendable (inout [String: String]) -> Void)?
    /// Runs under the Pi's lock right after a group-chats listing is answered,
    /// so a test can change the Pi's windows between listing and submission.
    var afterGroupListing: (@Sendable (inout [String: [(start: Date, end: Date)]]) -> Void)?

    init(rows: [String: String?] = [:], pageCap: Int = 500,
         failure: Failure = .none,
         groupChats: [String: [(start: Date, end: Date)]] = [:], groupPageCap: Int = 500,
         groupRows: [String: PiGroupRow] = [:],
         reactionFacts: [String: [String: Any]] = [:]) {
        self.bodies = rows.compactMapValues { $0 }
        self.reactionFacts = reactionFacts
        self.pageCap = pageCap
        self.failure = failure
        self.groupChats = groupChats
        self.groupPageCap = groupPageCap
        self.groupRows = groupRows
        for guid in groupRows.keys where bodies[guid] == nil { bodies[guid] = "stored" }
        for (guid, body) in rows where body == nil { bodies[guid] = "" }
    }

    func body(of guid: String) -> String? {
        lock.lock(); defer { lock.unlock() }
        return bodies[guid]
    }

    var submittedGuids: [String] { fillRequests.flatMap { $0.map(\.guid) } }
    var reactionItems: [[String: Any]] { reactionRequests.flatMap { $0 } }
    func facts(of guid: String) -> [String: Any]? {
        lock.lock(); defer { lock.unlock() }
        return reactionFacts[guid]
    }
    var groupItems: [[String: Any]] { groupRequests.flatMap { $0 } }
    func groupRow(_ guid: String) -> PiGroupRow? {
        lock.lock(); defer { lock.unlock() }
        return groupRows[guid]
    }

    func client(baseURL: URL) -> PiClient {
        PiClient(baseURL: baseURL, transport: { [self] request in try handle(request) },
                 policy: BackoffPolicy(maxRetries: 0), sleep: { _ in })
    }

    private func respond(_ request: URLRequest, _ status: Int, _ json: String) -> (Data, URLResponse) {
        (Data(json.utf8), HTTPURLResponse(url: request.url!, statusCode: status,
                                          httpVersion: "HTTP/1.1", headerFields: nil)!)
    }

    static func rfc3339(_ date: Date) -> String {
        ISO8601DateFormatter().string(from: date)
    }

    /// Answers a POST whose items include `add_group_message` /
    /// `clear_outbound_peer`, as the Pi's S1 group half does.
    private func handleGroupItems(_ request: URLRequest, _ raw: [[String: Any]]) throws -> (Data, URLResponse) {
        groupRequests.append(raw)
        if case .groupSubmissionUnreachable = failure { throw URLError(.cannotConnectToHost) }
        if case .groupSubmission(let n) = failure, groupRequests.count == n {
            return respond(request, 400, #"{"success":false,"error":{"code":"VALIDATION","message":"bad"}}"#)
        }
        var results: [String] = []
        for item in raw {
            let guid = item["guid"] as! String
            let outcome: String
            switch item["op"] as? String {
            case "add_group_message":
                let chat = item["chat_guid"] as! String
                let outbound = item["direction"] as? String == "outbound"
                XCTAssertTrue(["inbound", "outbound"].contains(item["direction"] as? String))
                if outbound {
                    XCTAssertTrue(item["sender"] is NSNull, "outbound sender must be null")
                } else {
                    XCTAssertTrue((item["sender"] as? String)?.isEmpty == false)
                }
                XCTAssertTrue(item["body"] is NSNull || (item["body"] as? String)?.isEmpty == false)
                let sentAt = ISO8601DateFormatter().date(from: item["sent_at"] as! String)
                XCTAssertNotNil(sentAt, "sent_at must be RFC 3339")
                if bodies[guid] != nil || groupRows[guid] != nil {
                    outcome = "unchanged"
                } else if let sentAt, groupChats[chat, default: []].contains(where: {
                    $0.start <= sentAt && sentAt <= $0.end
                }) {
                    bodies[guid] = (item["body"] as? String) ?? ""
                    groupRows[guid] = PiGroupRow(chat: chat, outbound: outbound, attributed: false)
                    outcome = "added"
                } else {
                    outcome = "outside_window"
                }
            case "clear_outbound_peer":
                if var row = groupRows[guid] {
                    if row.outbound && row.attributed {
                        row.attributed = false
                        groupRows[guid] = row
                        outcome = "cleared"
                    } else {
                        outcome = "unchanged"
                    }
                } else {
                    outcome = "unknown"
                }
            default:
                XCTFail("unexpected op \(String(describing: item["op"]))")
                outcome = "unknown"
            }
            results.append(#"{"guid":"\#(guid)","outcome":"\#(outcome)"}"#)
        }
        return respond(request, 200,
                       #"{"success":true,"data":{"results":[\#(results.joined(separator: ","))],"totals":{}}}"#)
    }

    /// Whether `reaction` is a valid S1 object, by the rule the Pi applies.
    private static func isValidReaction(_ reaction: Any?) -> Bool {
        guard let r = reaction as? [String: Any],
              let kind = r["kind"] as? String,
              ["love", "like", "dislike", "laugh", "emphasize", "question", "emoji", "sticker"].contains(kind),
              let target = r["target_key"] as? String, !target.isEmpty,
              r["removal"] is Bool else { return false }
        let emoji = r["emoji"] as? String
        return kind == "emoji" ? emoji?.isEmpty == false : r["emoji"] == nil
    }

    /// Answers a POST of `set_reaction` items, as the Pi's S2 does.
    private func handleReactionItems(_ request: URLRequest, _ raw: [[String: Any]]) throws -> (Data, URLResponse) {
        reactionRequests.append(raw)
        if case .reactionSubmissionUnreachable = failure { throw URLError(.cannotConnectToHost) }
        if case .reactionSubmission(let n) = failure, reactionRequests.count == n {
            return respond(request, 400, #"{"success":false,"error":{"code":"VALIDATION","message":"bad"}}"#)
        }
        guard raw.allSatisfy({ $0["op"] as? String == "set_reaction" && Self.isValidReaction($0["reaction"]) }) else {
            XCTFail("invalid set_reaction request")
            return respond(request, 400, #"{"success":false,"error":{"code":"VALIDATION","message":"bad"}}"#)
        }
        var results: [String] = []
        for item in raw {
            let guid = item["guid"] as! String
            let outcome: String
            if bodies[guid] == nil {
                outcome = "unknown"
            } else if reactionFacts[guid] != nil {
                outcome = "unchanged"
            } else {
                reactionFacts[guid] = item["reaction"] as? [String: Any]
                outcome = "marked"
            }
            results.append(#"{"guid":"\#(guid)","outcome":"\#(outcome)"}"#)
        }
        return respond(request, 200,
                       #"{"success":true,"data":{"results":[\#(results.joined(separator: ","))],"totals":{}}}"#)
    }

    private func handle(_ request: URLRequest) throws -> (Data, URLResponse) {
        lock.lock(); defer { lock.unlock() }
        let hostPath = "/api/v1/host/\(testHostID.uuidString.lowercased())/messages/enrichment"
        guard request.value(forHTTPHeaderField: "X-Mac-Host-ID") == testHostID.uuidString.lowercased(),
              request.value(forHTTPHeaderField: "Authorization") == "Bearer \(testAPIKey)" else {
            return respond(request, 401, #"{"success":false,"error":{"code":"AUTH","message":"no"}}"#)
        }
        let comps = URLComponents(url: request.url!, resolvingAgainstBaseURL: false)!
        if request.httpMethod == "GET", comps.path == hostPath + "/group-chats" {
            groupListRequests += 1
            if case .groupListingUnreachable = failure { throw URLError(.cannotConnectToHost) }
            if case .groupListingRejected = failure {
                return respond(request, 400, #"{"success":false,"error":{"code":"VALIDATION","message":"bad"}}"#)
            }
            let after = comps.queryItems?.first { $0.name == "after" }?.value
            let limit = Int(comps.queryItems?.first { $0.name == "limit" }?.value ?? "") ?? 500
            XCTAssertTrue((1...500).contains(limit), "limit \(limit) outside 1...500")
            let chats = groupChats.keys.sorted().filter { after == nil || $0 > after! }
            let page = Array(chats.prefix(min(limit, groupPageCap)))
            let next = chats.count > page.count ? "\"\(page.last!)\"" : "null"
            // Typed as String: GRDB's SQL literal otherwise claims the interpolation.
            let items: String = page.map { (chat: String) -> String in
                let windows: String = groupChats[chat]!.map { (w: (start: Date, end: Date)) -> String in
                    #"{"start":"\#(Self.rfc3339(w.start))","end":"\#(Self.rfc3339(w.end))"}"#
                }.joined(separator: ",")
                return #"{"chat_guid":"\#(chat)","windows":[\#(windows)]}"#
            }.joined(separator: ",")
            defer { afterGroupListing?(&groupChats) }
            return respond(request, 200,
                           #"{"success":true,"data":{"items":[\#(items)],"next_after":\#(next)}}"#)
        }
        if request.httpMethod == "GET", comps.path == hostPath + "/without-reaction-facts" {
            reactionListRequests += 1
            if case .reactionListingUnreachable = failure { throw URLError(.cannotConnectToHost) }
            if case .reactionListingRejected = failure {
                return respond(request, 400, #"{"success":false,"error":{"code":"VALIDATION","message":"bad"}}"#)
            }
            let after = comps.queryItems?.first { $0.name == "after" }?.value
            let limit = Int(comps.queryItems?.first { $0.name == "limit" }?.value ?? "") ?? 500
            XCTAssertTrue((1...500).contains(limit), "limit \(limit) outside 1...500")
            let unmarked = bodies.keys.filter { reactionFacts[$0] == nil }.sorted()
                .filter { after == nil || $0 > after! }
            let page = Array(unmarked.prefix(min(limit, pageCap)))
            let next = unmarked.count > page.count ? "\"\(page.last!)\"" : "null"
            let items = page.map { #"{"guid":"\#($0)"}"# }.joined(separator: ",")
            return respond(request, 200,
                           #"{"success":true,"data":{"items":[\#(items)],"next_after":\#(next)}}"#)
        }
        if request.httpMethod == "GET", comps.path == hostPath + "/missing-bodies" {
            listRequests += 1
            if case .unreachable = failure { throw URLError(.cannotConnectToHost) }
            if case .listing(let n) = failure, listRequests == n {
                return respond(request, 500, #"{"success":false,"error":{"code":"X","message":"boom"}}"#)
            }
            let after = comps.queryItems?.first { $0.name == "after" }?.value
            let limit = Int(comps.queryItems?.first { $0.name == "limit" }?.value ?? "") ?? 500
            XCTAssertTrue((1...500).contains(limit), "limit \(limit) outside 1...500")
            let missing = bodies.filter { $0.value.isEmpty }.keys.sorted().filter { after == nil || $0 > after! }
            let page = Array(missing.prefix(min(limit, pageCap)))
            let next = missing.count > page.count ? "\"\(page.last!)\"" : "null"
            let items = page.map { #"{"guid":"\#($0)"}"# }.joined(separator: ",")
            defer { afterListing?(&bodies) }
            return respond(request, 200,
                           #"{"success":true,"data":{"items":[\#(items)],"next_after":\#(next)}}"#)
        }
        if request.httpMethod == "POST", comps.path == hostPath {
            let json = try JSONSerialization.jsonObject(with: request.httpBody ?? Data()) as! [String: Any]
            let raw = json["items"] as! [[String: Any]]
            XCTAssertLessThanOrEqual(raw.count, 500)
            XCTAssertLessThan(request.httpBody?.count ?? 0, 8 << 20)
            if raw.contains(where: { $0["op"] as? String == "set_reaction" }) {
                return try handleReactionItems(request, raw)
            }
            if raw.contains(where: { $0["op"] as? String != "fill_body" }) {
                return try handleGroupItems(request, raw)
            }
            let items = raw.map { FillBodyItem(guid: $0["guid"] as! String, body: $0["body"] as! String) }
            XCTAssertTrue(raw.allSatisfy { $0["op"] as? String == "fill_body" })
            fillRequests.append(items)
            if case .unreachable = failure { throw URLError(.cannotConnectToHost) }
            if case .fill(let n) = failure, fillRequests.count == n {
                return respond(request, 400, #"{"success":false,"error":{"code":"VALIDATION","message":"bad"}}"#)
            }
            var results: [String] = []
            for item in items {
                let outcome: String
                if bodies[item.guid] == nil {
                    outcome = "unknown"
                } else if bodies[item.guid]!.isEmpty {
                    bodies[item.guid] = item.body
                    outcome = "filled"
                } else {
                    outcome = "unchanged"
                }
                results.append(#"{"guid":"\#(item.guid)","outcome":"\#(outcome)"}"#)
            }
            return respond(request, 200,
                           #"{"success":true,"data":{"results":[\#(results.joined(separator: ","))],"totals":{}}}"#)
        }
        return respond(request, 404, "")
    }
}

final class OutputCapture: @unchecked Sendable {
    private let lock = NSLock()
    private var out: [String] = []
    private var err: [String] = []
    func stdout(_ s: String) { lock.lock(); out.append(s); lock.unlock() }
    func stderr(_ s: String) { lock.lock(); err.append(s); lock.unlock() }
    var stdoutText: String { lock.lock(); defer { lock.unlock() }; return out.joined(separator: "\n") }
    var stderrText: String { lock.lock(); defer { lock.unlock() }; return err.joined(separator: "\n") }

    /// `key: value` lines of the report.
    var report: [String: String] {
        var r: [String: String] = [:]
        for line in stdoutText.split(separator: "\n") {
            let parts = line.split(separator: ":", maxSplits: 1).map { $0.trimmingCharacters(in: .whitespaces) }
            if parts.count == 2, parts[0] != "capture_failure_guid" { r[parts[0]] = parts[1] }
        }
        return r
    }

    var captureFailureGuids: [String] {
        stdoutText.split(separator: "\n").compactMap { line in
            line.hasPrefix("capture_failure_guid:")
                ? line.dropFirst("capture_failure_guid:".count).trimmingCharacters(in: .whitespaces) : nil
        }
    }
}

struct RunResult {
    let exitCode: Int32
    let output: OutputCapture
    var report: [String: String] { output.report }
}

/// A temp home holding the daemon's config.json, api-key and the messages database.
final class RescanFixture {
    let home: URL
    init(source rows: [SourceRow]) throws {
        home = FileManager.default.temporaryDirectory
            .appendingPathComponent("rescan-messages-\(UUID().uuidString)")
        let paths = home.appendingPathComponent("Library/Application Support/crm-mac")
        let messagesDir = home.appendingPathComponent("Library/Messages")
        try FileManager.default.createDirectory(at: paths, withIntermediateDirectories: true)
        try FileManager.default.createDirectory(at: messagesDir, withIntermediateDirectories: true)
        try ConfigStore(fileURL: paths.appendingPathComponent("config.json")).save(DaemonConfig(
            piURL: URL(string: "https://pi.example.test")!, hostID: testHostID,
            hostname: "test-mac", installedAt: Date(timeIntervalSince1970: 1_700_000_000)))
        try Data(testAPIKey.utf8).write(to: paths.appendingPathComponent("api-key"))
        try seedMessagesDB(rows, at: messagesDir.appendingPathComponent("chat.db"))
    }

    deinit { try? FileManager.default.removeItem(at: home) }

    private func seedMessagesDB(_ rows: [SourceRow], at url: URL) throws {
        let queue = try DatabaseQueue(path: url.path)
        try queue.write { db in
            try db.execute(sql: """
                CREATE TABLE message (
                    ROWID INTEGER PRIMARY KEY AUTOINCREMENT,
                    guid TEXT NOT NULL UNIQUE,
                    text TEXT,
                    attributedBody BLOB,
                    date INTEGER NOT NULL DEFAULT 0,
                    cache_has_attachments INTEGER NOT NULL DEFAULT 0,
                    associated_message_type INTEGER NOT NULL DEFAULT 0,
                    is_from_me INTEGER NOT NULL DEFAULT 0,
                    item_type INTEGER NOT NULL DEFAULT 0,
                    handle_id INTEGER,
                    associated_message_guid TEXT,
                    associated_message_emoji TEXT
                );
                CREATE TABLE handle (ROWID INTEGER PRIMARY KEY, id TEXT NOT NULL);
                CREATE TABLE chat (ROWID INTEGER PRIMARY KEY, guid TEXT NOT NULL UNIQUE, style INTEGER);
                CREATE TABLE chat_message_join (chat_id INTEGER NOT NULL, message_id INTEGER NOT NULL);
                CREATE TABLE attachment (
                    ROWID INTEGER PRIMARY KEY, uti TEXT, mime_type TEXT, transfer_name TEXT, total_bytes INTEGER
                );
                CREATE TABLE message_attachment_join (
                    ROWID INTEGER PRIMARY KEY AUTOINCREMENT,
                    message_id INTEGER NOT NULL,
                    attachment_id INTEGER NOT NULL
                );
                """)
            var handleIDs: [String: Int64] = [:]
            var chatIDs: [String: Int64] = [:]
            for (i, row) in rows.enumerated() {
                let rowID = Int64(i + 1)
                var handleID: Int64?
                if let handle = row.handle {
                    if handleIDs[handle] == nil {
                        handleIDs[handle] = Int64(handleIDs.count + 1)
                        try db.execute(sql: "INSERT INTO handle (ROWID, id) VALUES (?, ?)",
                                       arguments: [handleIDs[handle], handle])
                    }
                    handleID = handleIDs[handle]
                }
                let appleNanos = Int64(((row.sentAt?.timeIntervalSince1970 ?? 978_307_200) - 978_307_200) * 1e9)
                try db.execute(sql: """
                    INSERT INTO message (ROWID, guid, text, attributedBody, date, cache_has_attachments,
                                         associated_message_type, is_from_me, handle_id,
                                         associated_message_guid, associated_message_emoji)
                    VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
                    """, arguments: [rowID, row.guid, row.text, row.attributedBody, appleNanos,
                                     row.hasAttachment ? 1 : 0, row.associatedMessageType,
                                     row.isFromMe ? 1 : 0, handleID, row.associatedGUID, row.emoji])
                if let chat = row.chat {
                    if chatIDs[chat] == nil {
                        chatIDs[chat] = Int64(chatIDs.count + 1)
                        try db.execute(sql: "INSERT INTO chat (ROWID, guid, style) VALUES (?, ?, 43)",
                                       arguments: [chatIDs[chat], chat])
                    }
                    try db.execute(sql: "INSERT INTO chat_message_join (chat_id, message_id) VALUES (?, ?)",
                                   arguments: [chatIDs[chat], rowID])
                }
                if row.hasAttachment {
                    try db.execute(sql:
                        "INSERT INTO message_attachment_join (message_id, attachment_id) VALUES (?, ?)",
                        arguments: [rowID, rowID])
                }
            }
        }
    }

    func run(pi: FakePi) async -> RunResult {
        let capture = OutputCapture()
        let code = await rescanMessages(RescanEnvironment(
            home: home,
            makePiClient: { pi.client(baseURL: $0) },
            stdout: { capture.stdout($0) },
            stderr: { capture.stderr($0) }))
        return RunResult(exitCode: code, output: capture)
    }
}
