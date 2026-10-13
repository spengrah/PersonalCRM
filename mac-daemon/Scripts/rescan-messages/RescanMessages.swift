// rescan-messages — one-time operator script that fills the message bodies the
// Pi lacks, from the Mac's messages database.
//
// It walks the Pi's missing-bodies listing, reads each listed guid's source
// record, decodes the body through `MessageBodyExtractor` and submits it as a
// `fill_body` enrichment item. Every listed row it does not submit gets one
// content-free classification. It reads the daemon's config.json and api-key
// file and opens the messages database read-only; it writes no file, and a
// body exists only in memory and in the request that carries it to the Pi.
import Foundation
import GRDB
import CRMMacCore
import CRMMacPiClient
import CRMMacMessagesSource
import CRMMacSystem

struct RescanEnvironment: Sendable {
    var home: URL
    var makePiClient: @Sendable (URL) -> PiClient
    var stdout: @Sendable (String) -> Void
    var stderr: @Sendable (String) -> Void
}

/// Why a listed row has no recoverable body at its source.
enum ByNature: String, CaseIterable {
    case attachmentOnly = "attachment_only"
    case reaction
    case noneStored = "none_stored"
}

/// What the messages database says about one listed guid.
enum SourceDecision: Equatable {
    case body(String)
    case byNature(ByNature)
    /// No source row has the guid.
    case unavailable
    /// The extractor could not decode a present rich-text body.
    case captureFailure
}

struct RescanReport {
    var listed = 0
    var outcomes: [FillBodyOutcome: Int] = [:]
    var byNature: [ByNature: Int] = [:]
    var unavailable = 0
    var captureFailureGuids: [String] = []

    var lines: [String] {
        var out = ["listed: \(listed)"]
        for outcome in [FillBodyOutcome.filled, .unchanged, .unknown] {
            out.append("\(outcome.rawValue): \(outcomes[outcome, default: 0])")
        }
        for nature in ByNature.allCases {
            out.append("by_nature.\(nature.rawValue): \(byNature[nature, default: 0])")
        }
        out.append("unavailable: \(unavailable)")
        out.append("capture_failure: \(captureFailureGuids.count)")
        out += captureFailureGuids.map { "capture_failure_guid: \($0)" }
        return out
    }
}

/// The Pi caps a listing page and an enrichment request at 500 items, and a
/// request at 8 MiB. A listing page holds at most 500 guids and is submitted as
/// a unit, so a request never exceeds 500 items. Requests are split by the
/// size of each item as JSON-encoded, which counts escaping.
private let pageLimit = 500
private let maxRequestBytes = 8 << 20
/// `{"items":[` and `]}`.
private let requestEnvelopeBytes = 12

/// A single body whose encoded item cannot fit in one request.
private struct OversizeBody: Error {}

func rescanMessages(_ env: RescanEnvironment) async -> Int32 {
    let paths = DaemonPaths(home: env.home)
    let config: DaemonConfig
    let apiKey: String
    do {
        config = try ConfigStore(fileURL: paths.configFile).load()
        apiKey = try FileAPIKeyStore(path: paths.apiKeyFile.path).readAPIKey()
    } catch {
        env.stderr("rescan-messages: cannot read the daemon's config.json or api-key file")
        return 1
    }
    let pool: DatabasePool
    do {
        var dbConfig = Configuration()
        dbConfig.readonly = true
        let chatDB = env.home.appendingPathComponent("Library/Messages/chat.db")
        pool = try DatabasePool(
            path: SQLiteSnapshotReader.readOnlyURI(for: chatDB), configuration: dbConfig)
    } catch {
        env.stderr("rescan-messages: cannot open the messages database read-only")
        return 1
    }

    let pi = env.makePiClient(config.piURL)
    let auth = PiAuth(hostID: config.hostID, apiKey: apiKey)
    do {
        let report = try await walk(pi: pi, auth: auth, pool: pool)
        report.lines.forEach(env.stdout)
        return 0
    } catch let error as PiClientError {
        env.stderr("rescan-messages: Pi request failed (\(error.kind))")
        return 1
    } catch is OversizeBody {
        env.stderr("rescan-messages: a body is too large for one Pi request")
        return 1
    } catch {
        env.stderr("rescan-messages: messages database read failed")
        return 1
    }
}

private func walk(pi: PiClient, auth: PiAuth, pool: DatabasePool) async throws -> RescanReport {
    var report = RescanReport()
    var after: String?
    repeat {
        let page = try await pi.missingBodies(auth: auth, after: after, limit: pageLimit)
        let guids = page.items.map(\.guid)
        let decisions = try await pool.read { db in try guids.map { try decide(db: db, guid: $0) } }
        report.listed += guids.count

        var recoverable: [FillBodyItem] = []
        for (guid, decision) in zip(guids, decisions) {
            switch decision {
            case .body(let body): recoverable.append(FillBodyItem(guid: guid, body: body))
            case .byNature(let nature): report.byNature[nature, default: 0] += 1
            case .unavailable: report.unavailable += 1
            case .captureFailure: report.captureFailureGuids.append(guid)
            }
        }
        for batch in try batches(of: recoverable) {
            for result in try await pi.fillBodies(auth: auth, items: batch).results {
                report.outcomes[result.outcome, default: 0] += 1
            }
        }
        after = page.nextAfter
    } while after != nil
    return report
}

private func batches(of items: [FillBodyItem]) throws -> [[FillBodyItem]] {
    let encoder = JSONEncoder()
    var batches: [[FillBodyItem]] = []
    var current: [FillBodyItem] = []
    var currentBytes = requestEnvelopeBytes
    for item in items {
        // One byte for the comma that joins the item to its neighbour.
        let bytes = try encoder.encode(item).count + 1
        if requestEnvelopeBytes + bytes >= maxRequestBytes { throw OversizeBody() }
        if !current.isEmpty && currentBytes + bytes >= maxRequestBytes {
            batches.append(current)
            current = []
            currentBytes = requestEnvelopeBytes
        }
        current.append(item)
        currentBytes += bytes
    }
    if !current.isEmpty { batches.append(current) }
    return batches
}

/// Reads the source record for `guid` and decides what to do with its row.
private func decide(db: Database, guid: String) throws -> SourceDecision {
    guard let row = try Row.fetchOne(db, sql: """
        SELECT text, attributedBody, associated_message_type,
               cache_has_attachments != 0
                   OR EXISTS (SELECT 1 FROM message_attachment_join
                              WHERE message_attachment_join.message_id = message.ROWID)
                   AS has_attachment
        FROM message WHERE guid = ?
        """, arguments: [guid]) else {
        return .unavailable
    }
    switch MessageBodyExtractor.extract(text: row["text"], attributedBody: row["attributedBody"]) {
    case .body(let body): return .body(body)
    case .undecodable: return .captureFailure
    case .absent:
        let type: Int = row["associated_message_type"] ?? 0
        // Tapbacks add (2000...2005) and remove (3000...3005) a reaction.
        if (2000...2005).contains(type) || (3000...3005).contains(type) { return .byNature(.reaction) }
        let hasAttachment: Bool = row["has_attachment"]
        return .byNature(hasAttachment ? .attachmentOnly : .noneStored)
    }
}

private extension PiClientError {
    /// A short label for stderr; the Pi's own message text stays out of it.
    var kind: String {
        switch self {
        case .clientError(let status, _, _): return "rejected, HTTP \(status)"
        case .serverError(let status, _): return "server error, HTTP \(status)"
        case .transport: return "unreachable"
        case .authenticationRevoked: return "host key rejected"
        default: return "unusable answer"
        }
    }
}
