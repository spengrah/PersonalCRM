// End-to-end coverage for the plugin → notification-center
// handoff. AnarlogSessionsSourcePlugin.runTick must forward
// outcome.needsAttention to OrphanNotificationCenter.consume(...)
// after a successful publish.
//
// Pins the forwarding wire-up so a future refactor that drops
// the call (e.g. moving it under an `if` that fires only on
// non-empty rejection sets) is caught.
import XCTest
import UserNotifications
import CRMMacCore
import CRMMacOrphanNotifications
import CRMMacPiClient
@testable import CRMMacAnarlogSource

final class AnarlogSessionsPluginOrphanForwardingTests: XCTestCase {

    private let testAuth = PiAuth(
        hostID: UUID(uuidString: "11111111-2222-3333-4444-555555555555")!,
        apiKey: "k")
    private static let sessionUUIDA = "0a631ec3-fa11-47d2-aa0f-17b320860001"

    func testPluginForwardsNeedsAttentionToCenter() async throws {
        let rootPath = "/tmp/anarlog-fwd-\(UUID().uuidString)"
        let sessionsPath = rootPath + "/sessions"
        let fs = StubFS()
        fs.putDir(rootPath)
        fs.putDir(sessionsPath)
        let sessionDir = "\(sessionsPath)/\(Self.sessionUUIDA)"
        fs.putDir(sessionDir)
        let metaJSON = """
        {
          "id": "\(Self.sessionUUIDA)",
          "title": "Forwarded Session",
          "created_at": "2026-03-16T20:34:49Z",
          "user_id": "\(CRMMacAnarlogSource.selfHumanUUID)",
          "participants": [{"human_id": "22222222-2222-2222-2222-222222222222"}]
        }
        """
        fs.putFile("\(sessionDir)/_meta.json", bytes: Data(metaJSON.utf8))

        // Sender returns a needs_attention payload — the plugin
        // must forward it through.
        let publisher = AnarlogSessionsPublisher(
            sender: { _, body in
                IngestEventsData(
                    accepted: body.events.count,
                    duplicate: 0, rejected: 0, errors: [],
                    needsAttention: [
                        NeedsAttentionItem(sessionID: Self.sessionUUIDA,
                                           reason: "orphan"),
                    ])
            },
            auth: testAuth, logger: NoopLogger())

        // Real PiClient + a per-test mock transport that scripts
        // the cursor + known-ids + commit endpoints (the plugin
        // contacts those on every tick).
        let transport = SimpleScriptTransport()
        let piClient = PiClient(
            baseURL: URL(string: "https://test.invalid")!,
            transport: transport.asFunc(),
            logger: NoopLogger())

        // In-memory state.
        let stateURL = URL(fileURLWithPath: NSTemporaryDirectory())
            .appendingPathComponent("anarlog-fwd-\(UUID().uuidString).json")
        let stateStore = StateStore(fileURL: stateURL)
        try stateStore.initializeIfMissing()
        let mutator = StateMutator(store: stateStore)

        // Notification center + fakes.
        let fakePresenter = FakeFwdPresenter()
        let config = AnarlogConfig(
            rootPath: rootPath, humansEnabled: false, sessionsEnabled: true)
        let configSource = StubFwdConfigSource(config)
        let metaLookup = AnarlogSessionMetadataLookup(
            configSource: configSource, filesystem: fs)
        let center = OrphanNotificationCenter(
            presenter: fakePresenter,
            opener: FakeFwdOpener(),
            mutator: mutator,
            metadataLookup: metaLookup,
            piURL: URL(string: "https://pi.example")!,
            needsAttentionFetcher: { [] },
            logger: NoopLogger())

        let plugin = AnarlogSessionsSourcePlugin(
            tickInterval: 3600,
            piClient: piClient,
            auth: testAuth,
            mutator: mutator,
            publisher: publisher,
            filesystem: fs,
            configSource: configSource,
            healthRegistry: SourceHealthRegistry(),
            orphanNotificationCenter: center,
            logger: NoopLogger())

        // Trigger one tick.
        try await plugin.tick()

        // Assert the fake presenter received exactly one add call
        // — proving the plugin forwarded the publisher's
        // needs_attention payload through the center.
        let calls = await fakePresenter.recordedCalls()
        XCTAssertEqual(calls.count, 1,
                       "plugin should forward outcome.needsAttention to center")
        XCTAssertEqual(calls[0].identifier, "orphan:\(Self.sessionUUIDA)")
        XCTAssertTrue(calls[0].body.contains("Forwarded Session"))

        try? FileManager.default.removeItem(atPath: stateURL.path)
    }
}

// MARK: - Local fakes (kept local to this file so they don't
// collide with the larger plugin-tests' rig.)

private final class StubFS: AnarlogFilesystem, @unchecked Sendable {
    var files: [String: Data] = [:]
    var directories: Set<String> = []
    func exists(_ path: String) -> Bool {
        files[path] != nil || directories.contains(path)
    }
    func isDirectory(_ path: String) -> Bool { directories.contains(path) }
    func isReadableDirectory(_ path: String) -> Bool { directories.contains(path) }
    func listDirectory(_ dir: String) throws -> [String] {
        let prefix = dir.hasSuffix("/") ? dir : dir + "/"
        var out: Set<String> = []
        for p in files.keys where p.hasPrefix(prefix) {
            let tail = String(p.dropFirst(prefix.count))
            if let s = tail.firstIndex(of: "/") {
                out.insert(String(tail[..<s]))
            } else {
                out.insert(tail)
            }
        }
        for d in directories where d.hasPrefix(prefix) && d != dir {
            let tail = String(d.dropFirst(prefix.count))
            if let s = tail.firstIndex(of: "/") {
                out.insert(String(tail[..<s]))
            } else {
                out.insert(tail)
            }
        }
        return Array(out)
    }
    func readFile(_ path: String) throws -> Data {
        guard let b = files[path] else {
            throw AnarlogFilesystemError.ioError("not found: \(path)")
        }
        return b
    }
    func mtime(_ path: String) -> Date? { nil }
    func putDir(_ p: String) { directories.insert(p) }
    func putFile(_ path: String, bytes: Data) {
        files[path] = bytes
        let parent = (path as NSString).deletingLastPathComponent
        directories.insert(parent)
    }
}

private final class StubFwdConfigSource: AnarlogConfigSource, @unchecked Sendable {
    let cfg: AnarlogConfig?
    init(_ cfg: AnarlogConfig?) { self.cfg = cfg }
    func load() throws -> AnarlogConfig? { cfg }
}

/// Minimal recording presenter. We deliberately re-define a tiny
/// fake here (rather than depending on the
/// CRMMacOrphanNotificationsTests Support/ fakes) to avoid an
/// inter-test-target dep.
private actor FakeFwdPresenter: UserNotificationPresenter {
    private(set) var calls: [NotificationRequestSpec] = []
    func recordedCalls() -> [NotificationRequestSpec] { calls }
    func requestAuthorization() async -> Bool { true }
    func add(_ spec: NotificationRequestSpec) async throws { calls.append(spec) }
    func removeDelivered(withIdentifiers ids: [String]) async {}
    func removePending(withIdentifiers ids: [String]) async {}
    func setDelegate(_ ref: UserNotificationDelegateRef?) async {}
}

private struct FakeFwdOpener: WorkspaceOpener {
    func open(_ url: URL) async -> Bool { true }
}

/// Minimal Pi-side transport: returns empty success for every
/// endpoint the plugin contacts during a tick. Sufficient for
/// this test — we only care that the publish returns and the
/// outcome.needsAttention is forwarded.
private final class SimpleScriptTransport: @unchecked Sendable {
    func asFunc() -> TransportFunc {
        return { request in
            let path = request.url?.path ?? ""
            let method = request.httpMethod ?? "GET"
            let ok = HTTPURLResponse(url: request.url!, statusCode: 200,
                                     httpVersion: "HTTP/1.1",
                                     headerFields: ["Content-Type": "application/json"])!
            if path.hasSuffix("/cursor") && method == "GET" {
                let body = Data(#"""
                    {"success":true,"data":{"cursor":"","cursor_epoch":0,"backfill_complete":false}}
                    """#.utf8)
                return (body, ok)
            }
            if path.hasSuffix("/known-ids") && method == "GET" {
                let body = Data(#"{"success":true,"data":{"ids":[]}}"#.utf8)
                return (body, ok)
            }
            if path.hasSuffix("/ingest/events") && method == "POST" {
                // Empty response — the publisher fills in via its
                // sender closure, NOT via the transport.
                let body = Data(#"{"accepted":0,"duplicate":0,"rejected":0,"errors":[]}"#.utf8)
                return (body, ok)
            }
            if path.hasSuffix("/cursor") && method == "POST" {
                let body = Data(#"{"success":true,"data":{"ok":true}}"#.utf8)
                return (body, ok)
            }
            throw URLError(.unsupportedURL)
        }
    }
}
