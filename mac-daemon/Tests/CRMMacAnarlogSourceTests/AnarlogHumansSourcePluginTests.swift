// Tests for the AnarlogHumansSourcePlugin per-tick orchestrator.
//
// Focus: the carry-forward invariants:
//   - file becomes malformed (frontmatter corrupted) but remains
//     physically present → 0 delete events, cursor entry PRESERVED
//     (the prior cursor entry survives to the new cursor)
//   - file becomes oversized → 0 delete events, cursor entry
//     PRESERVED
//   - hash-mismatch → recovery flag set; subsequent tick enters
//     recovery path
//   - bootstrap route + Pi-known UUID present but malformed
//     synthesizes cursor entry from /known-ids; no event emitted
//
// Mocks: an in-memory AnarlogFilesystem stub that returns canned
// directory entries + file bytes; URL-pattern-routing transport that
// scripts cursor + known-ids + ingest responses without network I/O;
// a spy publisher that records published items.
import XCTest
import CRMMacCore
import CRMMacPiClient
@testable import CRMMacAnarlogSource

final class AnarlogHumansSourcePluginTests: XCTestCase {

    private let testAuth = PiAuth(
        hostID: UUID(uuidString: "11111111-2222-3333-4444-555555555555")!,
        apiKey: "k")

    // MARK: - Filesystem stub

    private final class StubFilesystem: AnarlogFilesystem, @unchecked Sendable {
        struct Entry {
            let bytes: Data
            let mtime: Date?
        }
        var files: [String: Entry] = [:]
        var directories: Set<String> = []
        var listError: Error?
        var permissionDeniedReads: Set<String> = []

        init(rootHumansPath: String) {
            directories.insert(rootHumansPath)
            // Also mark the parent root as existing for the
            // path_missing / humans_subdir_missing checks.
            let root = (rootHumansPath as NSString).deletingLastPathComponent
            directories.insert(root)
        }

        func exists(_ path: String) -> Bool {
            files[path] != nil || directories.contains(path)
        }

        func isDirectory(_ path: String) -> Bool {
            directories.contains(path)
        }

        func isReadableDirectory(_ path: String) -> Bool {
            directories.contains(path)
        }

        func listDirectory(_ dir: String) throws -> [String] {
            if let err = listError { throw err }
            let prefix = dir.hasSuffix("/") ? dir : dir + "/"
            var children: [String] = []
            for path in files.keys where path.hasPrefix(prefix) {
                let tail = String(path.dropFirst(prefix.count))
                if !tail.contains("/") {
                    children.append(tail)
                }
            }
            return children
        }

        func readFile(_ path: String) throws -> Data {
            if permissionDeniedReads.contains(path) {
                throw AnarlogFilesystemError.permissionDenied(path)
            }
            guard let entry = files[path] else {
                throw AnarlogFilesystemError.ioError("file not found: \(path)")
            }
            return entry.bytes
        }

        func mtime(_ path: String) -> Date? {
            files[path]?.mtime
        }

        func put(path: String, bytes: Data, mtime: Date? = Date()) {
            files[path] = Entry(bytes: bytes, mtime: mtime)
        }
    }

    // MARK: - PiClient scripting

    fileprivate struct PiScript {
        var cursorGet: SourceCursorState = SourceCursorState(
            cursor: "", cursorEpoch: 0, backfillComplete: false)
        var knownIDs: KnownIDsData = KnownIDsData(ids: [])
        var ingestResult: IngestEventsData = IngestEventsData(
            accepted: 0, duplicate: 0, rejected: 0, errors: [])
        var ingestThrows: Error?
        var cursorCommitThrows: Error?
    }

    private final class MockTransport: @unchecked Sendable {
        let script: PiScript
        var committedCursor: String?
        var ingestBodies: [Data] = []
        var commitWasAttempted = false

        init(_ script: PiScript) { self.script = script }

        func asFunc() -> TransportFunc {
            return { [self] request in
                let path = request.url?.path ?? ""
                let method = request.httpMethod ?? "GET"
                if path.hasSuffix("/cursor") && method == "GET" {
                    return (encodeCursor(script.cursorGet), Self.ok(request))
                }
                if path.hasSuffix("/known-ids") && method == "GET" {
                    return (encodeKnownIDs(script.knownIDs), Self.ok(request))
                }
                if path.hasSuffix("/ingest/events") && method == "POST" {
                    if let body = request.httpBody { ingestBodies.append(body) }
                    if let e = script.ingestThrows { throw e }
                    return (encodeIngest(script.ingestResult), Self.ok(request))
                }
                if path.hasSuffix("/cursor") && method == "POST" {
                    commitWasAttempted = true
                    if let body = request.httpBody,
                       let parsed = try? JSONSerialization.jsonObject(with: body) as? [String: Any],
                       let cur = parsed["cursor"] as? String {
                        committedCursor = cur
                    }
                    if let e = script.cursorCommitThrows { throw e }
                    let ok = Data(#"{"success":true,"data":{"ok":true}}"#.utf8)
                    return (ok, Self.ok(request))
                }
                throw URLError(.unsupportedURL)
            }
        }

        private func encodeCursor(_ s: SourceCursorState) -> Data {
            let dict: [String: Any] = [
                "success": true,
                "data": [
                    "cursor": s.cursor,
                    "cursor_epoch": s.cursorEpoch,
                    "backfill_complete": s.backfillComplete,
                ],
            ]
            return try! JSONSerialization.data(withJSONObject: dict)
        }

        private func encodeKnownIDs(_ k: KnownIDsData) -> Data {
            let ids: [[String: Any]] = k.ids.map { e in
                var d: [String: Any] = ["source_id": e.sourceID]
                if let h = e.lastContentHash { d["last_content_hash"] = h }
                else { d["last_content_hash"] = NSNull() }
                return d
            }
            return try! JSONSerialization.data(withJSONObject: [
                "success": true,
                "data": ["ids": ids],
            ])
        }

        private func encodeIngest(_ i: IngestEventsData) -> Data {
            let errs: [[String: Any]] = i.errors.map { e in
                ["index": e.index, "code": e.code, "message": e.message]
            }
            return try! JSONSerialization.data(withJSONObject: [
                "accepted": i.accepted,
                "duplicate": i.duplicate,
                "rejected": i.rejected,
                "errors": errs,
            ])
        }

        private static func ok(_ req: URLRequest) -> HTTPURLResponse {
            HTTPURLResponse(url: req.url!, statusCode: 200,
                            httpVersion: "HTTP/1.1",
                            headerFields: ["Content-Type": "application/json"])!
        }
    }

    // MARK: - config stub

    private final class StubConfigSource: AnarlogConfigSource, @unchecked Sendable {
        let result: Result<AnarlogConfig?, Error>
        init(_ cfg: AnarlogConfig?) { self.result = .success(cfg) }
        func load() throws -> AnarlogConfig? {
            switch result {
            case .success(let v): return v
            case .failure(let e): throw e
            }
        }
    }

    // MARK: - rig

    private struct Rig {
        let plugin: AnarlogHumansSourcePlugin
        let filesystem: StubFilesystem
        let mutator: StateMutator
        let transport: MockTransport
        let humansPath: String
    }

    private func makeRig(
        files: [(String, String)] = [],  // (uuid, body)
        config: AnarlogConfig? = AnarlogConfig(
            rootPath: "/tmp/anarlog-test",
            humansEnabled: true,
            sessionsEnabled: false),
        script: PiScript = PiScript()
    ) -> Rig {
        // When config is nil the plugin short-circuits before touching
        // the filesystem, so the path here is just a placeholder.
        let humansPath = (config?.rootPath ?? "/tmp/anarlog-test") + "/humans"
        let fs = StubFilesystem(rootHumansPath: humansPath)
        for (uuid, body) in files {
            fs.put(path: "\(humansPath)/\(uuid).md", bytes: Data(body.utf8))
        }
        let transport = MockTransport(script)
        let piClient = PiClient(
            baseURL: URL(string: "https://test.invalid")!,
            transport: transport.asFunc(),
            logger: NoopLogger())

        let stateURL = URL(fileURLWithPath: NSTemporaryDirectory())
            .appendingPathComponent("anarlog-humans-state-\(UUID().uuidString).json")
        let stateStore = StateStore(fileURL: stateURL)
        try? stateStore.initializeIfMissing()
        let mutator = StateMutator(store: stateStore)

        let publisher = AnarlogHumansPublisher(
            sender: { auth, body in
                try await piClient.ingestEvents(auth: auth, body: body)
            },
            auth: testAuth, logger: NoopLogger())
        let plugin = AnarlogHumansSourcePlugin(
            tickInterval: 300,
            piClient: piClient,
            auth: testAuth,
            mutator: mutator,
            publisher: publisher,
            filesystem: fs,
            configSource: StubConfigSource(config),
            healthRegistry: SourceHealthRegistry(),
            logger: NoopLogger())
        return Rig(plugin: plugin, filesystem: fs, mutator: mutator,
                   transport: transport, humansPath: humansPath)
    }

    private func validHumanBody(name: String = "Contact A") -> String {
        """
        ---
        name: \(name)
        emails: []
        job_title: ''
        pinned: false
        pin_order: 0
        ---
        """
    }

    private func uuid(_ tag: String) -> String {
        // Generate deterministic-shaped UUIDs from a tag for readability.
        let padded = String(repeating: "0", count: max(0, 12 - tag.count)) + tag
        return "0a18829e-12b6-40f6-93f8-6307973\(String(padded.suffix(5)))"
    }

    // MARK: - happy paths

    func testEmptyHumansDirYieldsEmptyCursor() async throws {
        let rig = makeRig(files: [])
        try await rig.plugin.tick()
        XCTAssertEqual(rig.transport.ingestBodies.count, 0)
        XCTAssertTrue(rig.transport.commitWasAttempted)
        XCTAssertEqual(rig.transport.committedCursor, "{}")
    }

    func testFirstRunWithFilesEmitsUpserts() async throws {
        let u1 = uuid("00001")
        let u2 = uuid("00002")
        let rig = makeRig(files: [
            (u1, validHumanBody(name: "A")),
            (u2, validHumanBody(name: "B")),
        ])
        try await rig.plugin.tick()
        // 1 ingest call (1 batch).
        XCTAssertEqual(rig.transport.ingestBodies.count, 1)
        // 2 events in that batch.
        let parsed = try JSONSerialization.jsonObject(
            with: rig.transport.ingestBodies[0]) as! [String: Any]
        let events = parsed["events"] as! [[String: Any]]
        XCTAssertEqual(events.count, 2)
        XCTAssertEqual(rig.transport.committedCursor != "{}", true)
    }

    func testNoOpDeltaTickEmitsNothing() async throws {
        let u1 = uuid("00003")
        let rig = makeRig(files: [(u1, validHumanBody())])
        // First tick — establishes cursor.
        try await rig.plugin.tick()
        XCTAssertEqual(rig.transport.ingestBodies.count, 1)
        let firstCommitted = rig.transport.committedCursor

        // Second tick: cursor is now non-empty; route is .delta;
        // contentChanged is false → no events.
        // We need to set up the cursor GET to return what we just
        // committed so the second tick sees the committed state.
        let script = PiScript(
            cursorGet: SourceCursorState(
                cursor: firstCommitted!, cursorEpoch: 0, backfillComplete: true),
            knownIDs: KnownIDsData(ids: []),
            ingestResult: IngestEventsData(
                accepted: 0, duplicate: 0, rejected: 0, errors: []))
        let rig2 = makeRig(files: [(u1, validHumanBody())], script: script)
        try await rig2.plugin.tick()
        // Cursor is populated; route is delta; no contentChange so
        // no ingest call.
        XCTAssertEqual(rig2.transport.ingestBodies.count, 0)
        // Cursor commit still happens (with same content).
        XCTAssertTrue(rig2.transport.commitWasAttempted)
    }

    func testFileRemovedEmitsTombstone() async throws {
        let u1 = uuid("00004")
        // Prior cursor has u1 with a known payload hash.
        let priorCursor = try AnarlogHumansCursorCodec.encode([
            u1: AnarlogHumansCursorEntry(
                contentHash: "prior", payloadHash: "priorpay", mtimeEpochMs: nil),
        ])
        let script = PiScript(
            cursorGet: SourceCursorState(
                cursor: priorCursor, cursorEpoch: 0, backfillComplete: true))
        // Now file is gone from disk.
        let rig = makeRig(files: [], script: script)
        try await rig.plugin.tick()
        XCTAssertEqual(rig.transport.ingestBodies.count, 1)
        let parsed = try JSONSerialization.jsonObject(
            with: rig.transport.ingestBodies[0]) as! [String: Any]
        let events = parsed["events"] as! [[String: Any]]
        XCTAssertEqual(events.count, 1)
        let event = events[0]
        XCTAssertEqual(event["kind"] as? String, "external_contact.deleted")
        // Deterministic source_id uses prior payload hash.
        XCTAssertEqual(event["source_id"] as? String, "\(u1)@deleted@priorpay")
        // Cursor commit reflects the file removal.
        XCTAssertEqual(rig.transport.committedCursor, "{}")
    }

    // MARK: - carry-forward invariant: malformed file does NOT tombstone

    func testTC_H6_MalformedFilePreservesCursorAndEmitsNoDelete() async throws {
        let u1 = uuid("00006")
        // Prior cursor entry is present (delta route).
        let priorCursor = try AnarlogHumansCursorCodec.encode([
            u1: AnarlogHumansCursorEntry(
                contentHash: "prev", payloadHash: "prevpay", mtimeEpochMs: nil),
        ])
        let script = PiScript(
            cursorGet: SourceCursorState(
                cursor: priorCursor, cursorEpoch: 0, backfillComplete: true))
        // File is physically present but body is garbage (no `---` opener).
        let rig = makeRig(
            files: [(u1, "garbage that won't parse as frontmatter")],
            script: script)

        try await rig.plugin.tick()

        // Critical P0 assertion: ZERO events in any batch.
        if rig.transport.ingestBodies.isEmpty {
            // Even better — no batch at all means definitely no delete.
        } else {
            let parsed = try JSONSerialization.jsonObject(
                with: rig.transport.ingestBodies[0]) as! [String: Any]
            let events = parsed["events"] as! [[String: Any]]
            for ev in events {
                XCTAssertNotEqual(ev["kind"] as? String,
                                  "external_contact.deleted",
                                  "P0 violation: malformed file produced a delete event")
            }
        }

        // Cursor was committed (publish was clean — 0 events).
        XCTAssertTrue(rig.transport.commitWasAttempted)
        // The committed cursor PRESERVES the prior entry for u1.
        let committed = try XCTUnwrap(rig.transport.committedCursor)
        let decoded = try XCTUnwrap(AnarlogHumansCursorCodec.decodeOrNil(committed))
        let preserved = try XCTUnwrap(decoded[u1])
        XCTAssertEqual(preserved.payloadHash, "prevpay")
        XCTAssertEqual(preserved.contentHash, "prev")
    }

    // MARK: - carry-forward invariant: oversized payload does NOT tombstone

    func testTC_H7_OversizedPayloadPreservesCursorAndEmitsNoDelete() async throws {
        let u1 = uuid("00007")
        let priorCursor = try AnarlogHumansCursorCodec.encode([
            u1: AnarlogHumansCursorEntry(
                contentHash: "prev", payloadHash: "prevpay", mtimeEpochMs: nil),
        ])
        let script = PiScript(
            cursorGet: SourceCursorState(
                cursor: priorCursor, cursorEpoch: 0, backfillComplete: true))
        // Build a body whose memo body alone exceeds maxPayloadBytes.
        let bigMemo = String(repeating: "X", count: CRMMacAnarlogSource.maxPayloadBytes + 1024)
        let body = """
        ---
        name: Big Contact
        ---
        \(bigMemo)
        """
        let rig = makeRig(files: [(u1, body)], script: script)

        try await rig.plugin.tick()

        if !rig.transport.ingestBodies.isEmpty {
            let parsed = try JSONSerialization.jsonObject(
                with: rig.transport.ingestBodies[0]) as! [String: Any]
            let events = parsed["events"] as! [[String: Any]]
            for ev in events {
                XCTAssertNotEqual(ev["kind"] as? String,
                                  "external_contact.deleted",
                                  "P0 violation: oversized file produced a delete event")
            }
        }
        XCTAssertTrue(rig.transport.commitWasAttempted)
        let committed = try XCTUnwrap(rig.transport.committedCursor)
        let decoded = try XCTUnwrap(AnarlogHumansCursorCodec.decodeOrNil(committed))
        let preserved = try XCTUnwrap(decoded[u1])
        XCTAssertEqual(preserved.payloadHash, "prevpay")
    }

    // MARK: - self-human filtered

    func testSelfHumanFileSkipped() async throws {
        let rig = makeRig(files: [
            ("00000000-0000-0000-0000-000000000000", validHumanBody()),
        ])
        try await rig.plugin.tick()
        XCTAssertEqual(rig.transport.ingestBodies.count, 0)
        XCTAssertEqual(rig.transport.committedCursor, "{}")
    }

    // MARK: - hash-mismatch sets recovery flag

    func testHashMismatchSetsRecoveryFlag() async throws {
        let u1 = uuid("00011")
        let script = PiScript(
            ingestResult: IngestEventsData(
                accepted: 0, duplicate: 0, rejected: 1,
                errors: [IngestEventError(
                    index: 0,
                    code: "EXTERNAL_CONTACT_HASH_MISMATCH",
                    message: "mismatch")]))
        let rig = makeRig(files: [(u1, validHumanBody())], script: script)
        try await rig.plugin.tick()
        let state = try await rig.mutator.read()
        let src = try XCTUnwrap(state.sources["anarlog_humans"])
        XCTAssertTrue((src.lastError ?? "").contains("recovery_requested"))
        // Cursor must NOT have committed.
        XCTAssertFalse(rig.transport.commitWasAttempted)
    }

    // MARK: - unhealthy reasons

    func testNotConfiguredWhenConfigNil() async throws {
        let rig = makeRig(files: [], config: nil)
        try await rig.plugin.tick()
        let state = try await rig.mutator.read()
        let src = try XCTUnwrap(state.sources["anarlog_humans"])
        XCTAssertEqual(src.lastError, "not_configured")
    }

    func testNotConfiguredWhenDisabled() async throws {
        let rig = makeRig(files: [], config: AnarlogConfig(
            rootPath: "/tmp/anarlog-test",
            humansEnabled: false,
            sessionsEnabled: false))
        try await rig.plugin.tick()
        let state = try await rig.mutator.read()
        let src = try XCTUnwrap(state.sources["anarlog_humans"])
        XCTAssertEqual(src.lastError, "not_configured")
    }

    // MARK: - lastError carries anomaly counts on clean commit

    func testCleanCommitWithMalformedFileRecordsAnomaly() async throws {
        let u1 = uuid("00099")
        let priorCursor = try AnarlogHumansCursorCodec.encode([
            u1: AnarlogHumansCursorEntry(
                contentHash: "p", payloadHash: "p", mtimeEpochMs: nil),
        ])
        let script = PiScript(
            cursorGet: SourceCursorState(
                cursor: priorCursor, cursorEpoch: 0, backfillComplete: true))
        let rig = makeRig(files: [(u1, "garbage")], script: script)
        try await rig.plugin.tick()
        XCTAssertTrue(rig.transport.commitWasAttempted)
        let state = try await rig.mutator.read()
        let src = try XCTUnwrap(state.sources["anarlog_humans"])
        XCTAssertTrue((src.lastError ?? "").contains("parse_failed=1"),
                      "expected anomaly summary; got: \(src.lastError ?? "nil")")
    }

    // MARK: - bootstrap via known-ids

    func testBootstrapViaKnownIDsTombstonesMissing() async throws {
        let u1 = uuid("00008")
        // Empty cursor; known-ids returns 1 prior UUID not in scan.
        let script = PiScript(
            cursorGet: SourceCursorState(
                cursor: "", cursorEpoch: 0, backfillComplete: false),
            knownIDs: KnownIDsData(ids: [
                KnownContactID(sourceID: "\(u1)@deadbeef",
                               lastContentHash: "deadbeef"),
            ]))
        let rig = makeRig(files: [], script: script)
        try await rig.plugin.tick()
        XCTAssertEqual(rig.transport.ingestBodies.count, 1)
        let parsed = try JSONSerialization.jsonObject(
            with: rig.transport.ingestBodies[0]) as! [String: Any]
        let events = parsed["events"] as! [[String: Any]]
        XCTAssertEqual(events.count, 1)
        XCTAssertEqual(events[0]["kind"] as? String, "external_contact.deleted")
        XCTAssertEqual(events[0]["source_id"] as? String, "\(u1)@deleted@deadbeef")
    }

    func testTC_H19_BootstrapPiKnownButMalformedPreservesAndSynthesizes() async throws {
        let u1 = uuid("00019")
        let script = PiScript(
            cursorGet: SourceCursorState(
                cursor: "", cursorEpoch: 0, backfillComplete: false),
            knownIDs: KnownIDsData(ids: [
                KnownContactID(sourceID: "\(u1)@deadbeef",
                               lastContentHash: "deadbeef"),
            ]))
        let rig = makeRig(files: [(u1, "garbage")], script: script)
        try await rig.plugin.tick()
        // No upsert (file failed to parse) and no delete (file is
        // physically present).
        if !rig.transport.ingestBodies.isEmpty {
            let parsed = try JSONSerialization.jsonObject(
                with: rig.transport.ingestBodies[0]) as! [String: Any]
            let events = parsed["events"] as! [[String: Any]]
            XCTAssertEqual(events.count, 0, "no events should be emitted for a present-but-malformed Pi-known file")
        }
        // The cursor commit synthesizes an entry using
        // knownIDs.lastContentHash so the next scan can build a
        // deterministic delete source_id.
        XCTAssertTrue(rig.transport.commitWasAttempted)
        let committed = try XCTUnwrap(rig.transport.committedCursor)
        let decoded = try XCTUnwrap(AnarlogHumansCursorCodec.decodeOrNil(committed))
        let synthesized = try XCTUnwrap(decoded[u1])
        XCTAssertEqual(synthesized.payloadHash, "deadbeef")
    }
}
