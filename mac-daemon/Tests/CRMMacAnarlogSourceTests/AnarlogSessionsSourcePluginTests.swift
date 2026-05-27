// Tests for the AnarlogSessionsSourcePlugin per-tick orchestrator.
//
// Focus: same carry-forward invariants the humans plugin has, plus
// session-specific behaviors:
//   - pre-backfill-floor sessions get a floor_skip cursor entry and
//     never emit
//   - missing _meta.json or invalid JSON → carry-forward; no delete
//   - skip-list dirs ignored
//   - non-UUID dirs ignored
//   - bare files at sessions/<uuid> ignored (not a directory)
//   - self-UUID filtered from participants
//   - in-flight coalescing
//   - recovery branch reaches /known-ids (currently returns empty;
//     code path symmetric with humans)
import XCTest
import CRMMacCore
import CRMMacPiClient
@testable import CRMMacAnarlogSource

final class AnarlogSessionsSourcePluginTests: XCTestCase {

    private let testAuth = PiAuth(
        hostID: UUID(uuidString: "11111111-2222-3333-4444-555555555555")!,
        apiKey: "k")

    // MARK: - Stub filesystem (mirrors humans-plugin stub)

    private final class StubFilesystem: AnarlogFilesystem, @unchecked Sendable {
        var files: [String: Data] = [:]
        var directories: Set<String> = []
        var permissionDeniedDirs: Set<String> = []

        init(rootPath: String, sessionsPath: String) {
            directories.insert(rootPath)
            directories.insert(sessionsPath)
        }

        func exists(_ path: String) -> Bool {
            files[path] != nil || directories.contains(path)
        }

        func isDirectory(_ path: String) -> Bool {
            directories.contains(path)
        }

        func isReadableDirectory(_ path: String) -> Bool {
            if permissionDeniedDirs.contains(path) { return false }
            return directories.contains(path)
        }

        func listDirectory(_ dir: String) throws -> [String] {
            if permissionDeniedDirs.contains(dir) {
                throw AnarlogFilesystemError.permissionDenied(dir)
            }
            let prefix = dir.hasSuffix("/") ? dir : dir + "/"
            var children: Set<String> = []
            // Include both files and subdirectories.
            for path in files.keys where path.hasPrefix(prefix) {
                let tail = String(path.dropFirst(prefix.count))
                if let slash = tail.firstIndex(of: "/") {
                    children.insert(String(tail[..<slash]))
                } else {
                    children.insert(tail)
                }
            }
            for d in directories where d.hasPrefix(prefix) && d != dir {
                let tail = String(d.dropFirst(prefix.count))
                if let slash = tail.firstIndex(of: "/") {
                    children.insert(String(tail[..<slash]))
                } else {
                    children.insert(tail)
                }
            }
            return Array(children)
        }

        func readFile(_ path: String) throws -> Data {
            guard let bytes = files[path] else {
                throw AnarlogFilesystemError.ioError("file not found: \(path)")
            }
            return bytes
        }

        func mtime(_ path: String) -> Date? { nil }

        func putSessionDir(_ dir: String) {
            directories.insert(dir)
        }
        func putFile(_ path: String, bytes: Data) {
            files[path] = bytes
            // Ensure parent dir is registered as a directory.
            let parent = (path as NSString).deletingLastPathComponent
            directories.insert(parent)
        }
    }

    // MARK: - PiClient scripting (reused shape from humans plugin tests)

    fileprivate struct PiScript {
        var cursorGet: SourceCursorState = SourceCursorState(
            cursor: "", cursorEpoch: 0, backfillComplete: false)
        var knownIDs: KnownIDsData = KnownIDsData(ids: [])
        var ingestResult: IngestEventsData = IngestEventsData(
            accepted: 0, duplicate: 0, rejected: 0, errors: [])
    }

    private final class MockTransport: @unchecked Sendable {
        let script: PiScript
        var committedCursor: String?
        var ingestBodies: [Data] = []
        var commitWasAttempted = false
        var knownIDsCalled = false
        var ingestCallCount = 0

        init(_ script: PiScript) { self.script = script }

        func asFunc() -> TransportFunc {
            return { [self] request in
                let path = request.url?.path ?? ""
                let method = request.httpMethod ?? "GET"
                if path.hasSuffix("/cursor") && method == "GET" {
                    return (encodeCursor(script.cursorGet), Self.ok(request))
                }
                if path.hasSuffix("/known-ids") && method == "GET" {
                    knownIDsCalled = true
                    return (encodeKnownIDs(script.knownIDs), Self.ok(request))
                }
                if path.hasSuffix("/ingest/events") && method == "POST" {
                    if let body = request.httpBody { ingestBodies.append(body) }
                    ingestCallCount += 1
                    return (encodeIngest(script.ingestResult), Self.ok(request))
                }
                if path.hasSuffix("/cursor") && method == "POST" {
                    commitWasAttempted = true
                    if let body = request.httpBody,
                       let parsed = try? JSONSerialization.jsonObject(with: body) as? [String: Any],
                       let cur = parsed["cursor"] as? String {
                        committedCursor = cur
                    }
                    let ok = Data(#"{"success":true,"data":{"ok":true}}"#.utf8)
                    return (ok, Self.ok(request))
                }
                throw URLError(.unsupportedURL)
            }
        }

        private func encodeCursor(_ s: SourceCursorState) -> Data {
            try! JSONSerialization.data(withJSONObject: [
                "success": true,
                "data": [
                    "cursor": s.cursor,
                    "cursor_epoch": s.cursorEpoch,
                    "backfill_complete": s.backfillComplete,
                ],
            ])
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
            try! JSONSerialization.data(withJSONObject: [
                "accepted": i.accepted,
                "duplicate": i.duplicate,
                "rejected": i.rejected,
                "errors": i.errors.map {
                    ["index": $0.index, "code": $0.code, "message": $0.message]
                },
            ])
        }
        private static func ok(_ req: URLRequest) -> HTTPURLResponse {
            HTTPURLResponse(url: req.url!, statusCode: 200,
                            httpVersion: "HTTP/1.1",
                            headerFields: ["Content-Type": "application/json"])!
        }
    }

    private final class StubConfigSource: AnarlogConfigSource, @unchecked Sendable {
        let cfg: AnarlogConfig?
        init(_ cfg: AnarlogConfig?) { self.cfg = cfg }
        func load() throws -> AnarlogConfig? { cfg }
    }

    // MARK: - rig

    private struct Rig {
        let plugin: AnarlogSessionsSourcePlugin
        let filesystem: StubFilesystem
        let mutator: StateMutator
        let transport: MockTransport
        let sessionsPath: String
    }

    private func makeRig(
        sessions: [(uuid: String, metaJSON: String, summary: String?, memo: String?)] = [],
        topLevelExtras: [String] = [],
        config: AnarlogConfig? = AnarlogConfig(
            rootPath: "/tmp/anarlog-test",
            humansEnabled: false,
            sessionsEnabled: true),
        script: PiScript = PiScript()
    ) -> Rig {
        let rootPath = config!.rootPath
        let sessionsPath = rootPath + "/sessions"
        let fs = StubFilesystem(rootPath: rootPath, sessionsPath: sessionsPath)
        for (uuid, metaJSON, summary, memo) in sessions {
            let dir = "\(sessionsPath)/\(uuid)"
            fs.putSessionDir(dir)
            fs.putFile("\(dir)/_meta.json", bytes: Data(metaJSON.utf8))
            if let s = summary {
                fs.putFile("\(dir)/_summary.md", bytes: Data(s.utf8))
            }
            if let m = memo {
                fs.putFile("\(dir)/_memo.md", bytes: Data(m.utf8))
            }
        }
        for extra in topLevelExtras {
            // Add a top-level file (like settings.json) to verify the
            // skip list ignores it.
            fs.putFile("\(sessionsPath)/\(extra)", bytes: Data("{}".utf8))
        }
        let transport = MockTransport(script)
        let piClient = PiClient(
            baseURL: URL(string: "https://test.invalid")!,
            transport: transport.asFunc(),
            logger: NoopLogger())
        let stateURL = URL(fileURLWithPath: NSTemporaryDirectory())
            .appendingPathComponent("anarlog-sessions-state-\(UUID().uuidString).json")
        let stateStore = StateStore(fileURL: stateURL)
        try? stateStore.initializeIfMissing()
        let mutator = StateMutator(store: stateStore)
        let publisher = AnarlogSessionsPublisher(
            sender: { auth, body in
                try await piClient.ingestEvents(auth: auth, body: body)
            },
            auth: testAuth, logger: NoopLogger())
        let plugin = AnarlogSessionsSourcePlugin(
            tickInterval: 3600,
            piClient: piClient,
            auth: testAuth,
            mutator: mutator,
            publisher: publisher,
            filesystem: fs,
            configSource: StubConfigSource(config),
            healthRegistry: SourceHealthRegistry(),
            logger: NoopLogger())
        return Rig(plugin: plugin, filesystem: fs, mutator: mutator,
                   transport: transport, sessionsPath: sessionsPath)
    }

    private func metaJSON(
        uuid: String,
        title: String = "session-title",
        createdAt: String = "2026-03-16T20:34:49Z",
        participants: [String] = ["11111111-1111-1111-1111-111111111111"]
    ) -> String {
        let participantsJSON = participants
            .map { "{\"human_id\":\"\($0)\"}" }
            .joined(separator: ",")
        return """
        {
          "id": "\(uuid)",
          "title": "\(title)",
          "created_at": "\(createdAt)",
          "user_id": "\(CRMMacAnarlogSource.selfHumanUUID)",
          "participants": [\(participantsJSON)]
        }
        """
    }

    private func sessionUUID(_ tag: String) -> String {
        let padded = String(repeating: "0", count: max(0, 12 - tag.count)) + tag
        return "0a631ec3-fa11-47d2-aa0f-17b32086\(String(padded.suffix(4)))"
    }

    // MARK: - happy path

    func testSessionWithMetaAndSummaryEmits() async throws {
        let u1 = sessionUUID("00001")
        let rig = makeRig(sessions: [
            (uuid: u1,
             metaJSON: metaJSON(uuid: u1),
             summary: "summary body",
             memo: nil),
        ])
        try await rig.plugin.tick()
        XCTAssertEqual(rig.transport.ingestBodies.count, 1)
        let parsed = try JSONSerialization.jsonObject(
            with: rig.transport.ingestBodies[0]) as! [String: Any]
        let events = parsed["events"] as! [[String: Any]]
        XCTAssertEqual(events.count, 1)
        XCTAssertEqual(events[0]["kind"] as? String, "meeting_note.recorded")
    }

    func testSessionWithAllThreeFilesEmits() async throws {
        let u1 = sessionUUID("00002")
        let rig = makeRig(sessions: [
            (uuid: u1, metaJSON: metaJSON(uuid: u1),
             summary: "s", memo: "m"),
        ])
        try await rig.plugin.tick()
        let parsed = try JSONSerialization.jsonObject(
            with: rig.transport.ingestBodies[0]) as! [String: Any]
        let events = parsed["events"] as! [[String: Any]]
        let payloadAny = events[0]["payload"] as! [String: Any]
        XCTAssertEqual(payloadAny["summary"] as? String, "s")
        XCTAssertEqual(payloadAny["memo"] as? String, "m")
    }

    // MARK: - pre-floor sessions

    func testPreFloorSessionGetsSentinelEntryAndNoEvent() async throws {
        let u1 = sessionUUID("00003")
        let rig = makeRig(sessions: [
            (uuid: u1,
             metaJSON: metaJSON(uuid: u1, createdAt: "2025-12-15T10:00:00Z"),
             summary: nil, memo: nil),
        ])
        try await rig.plugin.tick()
        XCTAssertEqual(rig.transport.ingestBodies.count, 0,
                       "pre-floor session must not emit any event")
        XCTAssertTrue(rig.transport.commitWasAttempted)
        let committed = try XCTUnwrap(rig.transport.committedCursor)
        let decoded = try XCTUnwrap(AnarlogSessionsCursorCodec.decodeOrNil(committed))
        let sentinel = try XCTUnwrap(decoded[u1])
        XCTAssertTrue(sentinel.isFloorSkipped)
    }

    // MARK: - oversized payload carry-forward

    func testOversizedSessionPayloadPreservesCursorAndEmitsNoDelete() async throws {
        let u1 = sessionUUID("00004")
        let priorCursor = try AnarlogSessionsCursorCodec.encode([
            u1: AnarlogSessionsCursorEntry(
                metaHash: "prevmeta",
                summaryHash: "prevsum",
                memoHash: "prevmemo",
                payloadHash: "prevpay"),
        ])
        let script = PiScript(
            cursorGet: SourceCursorState(
                cursor: priorCursor, cursorEpoch: 0, backfillComplete: true))
        let bigSummary = String(repeating: "X", count: CRMMacAnarlogSource.maxPayloadBytes + 1024)
        let rig = makeRig(
            sessions: [(uuid: u1,
                        metaJSON: metaJSON(uuid: u1),
                        summary: bigSummary, memo: nil)],
            script: script)
        try await rig.plugin.tick()
        if !rig.transport.ingestBodies.isEmpty {
            let parsed = try JSONSerialization.jsonObject(
                with: rig.transport.ingestBodies[0]) as! [String: Any]
            let events = parsed["events"] as! [[String: Any]]
            for ev in events {
                XCTAssertNotEqual(ev["kind"] as? String, "meeting_note.deleted",
                                  "P0 violation: oversized session produced a delete")
            }
        }
        XCTAssertTrue(rig.transport.commitWasAttempted)
        let committed = try XCTUnwrap(rig.transport.committedCursor)
        let decoded = try XCTUnwrap(AnarlogSessionsCursorCodec.decodeOrNil(committed))
        let preserved = try XCTUnwrap(decoded[u1])
        XCTAssertEqual(preserved.payloadHash, "prevpay")
    }

    // MARK: - missing or invalid _meta.json

    func testMissingMetaPreservesCursor() async throws {
        let u1 = sessionUUID("00005")
        let priorCursor = try AnarlogSessionsCursorCodec.encode([
            u1: AnarlogSessionsCursorEntry(
                metaHash: "prev", summaryHash: nil, memoHash: nil,
                payloadHash: "prev"),
        ])
        let script = PiScript(
            cursorGet: SourceCursorState(
                cursor: priorCursor, cursorEpoch: 0, backfillComplete: true))
        let rig = makeRig(sessions: [], script: script)
        // Manually register only the session dir, no _meta.json.
        rig.filesystem.putSessionDir("\(rig.sessionsPath)/\(u1)")
        try await rig.plugin.tick()
        // No delete event for u1 — physically present, cursor entry
        // carried forward.
        if !rig.transport.ingestBodies.isEmpty {
            let parsed = try JSONSerialization.jsonObject(
                with: rig.transport.ingestBodies[0]) as! [String: Any]
            let events = parsed["events"] as! [[String: Any]]
            for ev in events {
                XCTAssertNotEqual(ev["kind"] as? String, "meeting_note.deleted")
            }
        }
        let committed = try XCTUnwrap(rig.transport.committedCursor)
        let decoded = try XCTUnwrap(AnarlogSessionsCursorCodec.decodeOrNil(committed))
        XCTAssertNotNil(decoded[u1])
    }

    func testInvalidMetaJSONPreservesCursor() async throws {
        let u1 = sessionUUID("00006")
        let priorCursor = try AnarlogSessionsCursorCodec.encode([
            u1: AnarlogSessionsCursorEntry(
                metaHash: "prev", summaryHash: nil, memoHash: nil,
                payloadHash: "prev"),
        ])
        let script = PiScript(
            cursorGet: SourceCursorState(
                cursor: priorCursor, cursorEpoch: 0, backfillComplete: true))
        let rig = makeRig(sessions: [
            (uuid: u1, metaJSON: "not json", summary: nil, memo: nil),
        ], script: script)
        try await rig.plugin.tick()
        if !rig.transport.ingestBodies.isEmpty {
            let parsed = try JSONSerialization.jsonObject(
                with: rig.transport.ingestBodies[0]) as! [String: Any]
            let events = parsed["events"] as! [[String: Any]]
            for ev in events {
                XCTAssertNotEqual(ev["kind"] as? String, "meeting_note.deleted")
            }
        }
        let committed = try XCTUnwrap(rig.transport.committedCursor)
        let decoded = try XCTUnwrap(AnarlogSessionsCursorCodec.decodeOrNil(committed))
        XCTAssertNotNil(decoded[u1])
    }

    // MARK: - skip-list + non-UUID

    func testSkipListEntriesIgnored() async throws {
        let rig = makeRig(
            sessions: [],
            topLevelExtras: ["settings.json", "store.json", "events.json"])
        try await rig.plugin.tick()
        // No events should fire.
        XCTAssertEqual(rig.transport.ingestBodies.count, 0)
    }

    func testNonUUIDDirIgnored() async throws {
        let rig = makeRig(sessions: [])
        rig.filesystem.putSessionDir("\(rig.sessionsPath)/not-a-uuid-dir")
        try await rig.plugin.tick()
        XCTAssertEqual(rig.transport.ingestBodies.count, 0)
    }

    // MARK: - self-user filtered from participants

    func testSelfUserFilteredFromParticipants() async throws {
        let u1 = sessionUUID("00007")
        let realParticipant = "22222222-2222-2222-2222-222222222222"
        let meta = """
        {
          "id": "\(u1)",
          "title": "t",
          "created_at": "2026-03-16T20:34:49Z",
          "user_id": "\(CRMMacAnarlogSource.selfHumanUUID)",
          "participants": [
            {"human_id": "\(CRMMacAnarlogSource.selfHumanUUID)"},
            {"human_id": "\(realParticipant)"}
          ]
        }
        """
        let rig = makeRig(sessions: [
            (uuid: u1, metaJSON: meta, summary: nil, memo: nil),
        ])
        try await rig.plugin.tick()
        let parsed = try JSONSerialization.jsonObject(
            with: rig.transport.ingestBodies[0]) as! [String: Any]
        let events = parsed["events"] as! [[String: Any]]
        let payload = events[0]["payload"] as! [String: Any]
        let parts = payload["participant_ids"] as! [String]
        XCTAssertEqual(parts, [realParticipant])
    }

    // MARK: - in-flight coalescing

    func testInFlightCoalescing() async throws {
        // Two concurrent tick() calls coalesce — the second only fires
        // one re-tick after the first returns, not multiple.
        let u1 = sessionUUID("00008")
        let rig = makeRig(sessions: [
            (uuid: u1, metaJSON: metaJSON(uuid: u1),
             summary: "s", memo: nil),
        ])
        async let t1: () = rig.plugin.tick()
        async let t2: () = rig.plugin.tick()
        async let t3: () = rig.plugin.tick()
        _ = try await (t1, t2, t3)
        // Actor serializes; we expect <= 2 ingest calls (first tick +
        // at most one coalesced re-tick).
        XCTAssertLessThanOrEqual(rig.transport.ingestCallCount, 2)
        XCTAssertGreaterThanOrEqual(rig.transport.ingestCallCount, 1)
    }

    // MARK: - recovery branch consults /known-ids

    func testRecoveryBranchCallsKnownIDsEvenForSessions() async throws {
        let u1 = sessionUUID("00009")
        let rig = makeRig(sessions: [
            (uuid: u1, metaJSON: metaJSON(uuid: u1),
             summary: "s", memo: nil),
        ])
        // Pre-set recovery flag.
        try await rig.mutator.mutate { state in
            var src = state.sources["anarlog_sessions"] ?? SourceState()
            src.lastError = "recovery_requested:test"
            state.sources["anarlog_sessions"] = src
        }
        try await rig.plugin.tick()
        XCTAssertTrue(rig.transport.knownIDsCalled,
                      "recovery branch must consult /known-ids")
    }

    // MARK: - configuration unhealthy

    func testSessionsDisabledMarksNotConfigured() async throws {
        let rig = makeRig(sessions: [], config: AnarlogConfig(
            rootPath: "/tmp/anarlog-test",
            humansEnabled: true,
            sessionsEnabled: false))
        try await rig.plugin.tick()
        let state = try await rig.mutator.read()
        let src = try XCTUnwrap(state.sources["anarlog_sessions"])
        XCTAssertEqual(src.lastError, "not_configured")
    }
}
