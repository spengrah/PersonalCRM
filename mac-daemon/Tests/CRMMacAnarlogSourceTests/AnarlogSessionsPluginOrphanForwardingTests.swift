// End-to-end coverage for the plugin → notification-center handoff.
import Foundation
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
        let fake = try FakeAnarlogCLI()
        let client = fake.makeClient()
        let list = FakeAnarlogResponse(
            argv: FakeAnarlogCLI.listArgv(offset: 0),
            stdout: FakeAnarlogCLI.listStdout(
                entries: [FakeAnarlogListEntry(
                    id: Self.sessionUUIDA, createdAt: "2026-03-16T20:34:49.000Z")],
                offset: 0, nextOffset: nil))
        let get = FakeAnarlogResponse(
            argv: FakeAnarlogCLI.getArgv(id: Self.sessionUUIDA),
            stdout: FakeAnarlogCLI.getStdout(FakeAnarlogRecord(
                id: Self.sessionUUIDA,
                title: "Forwarded Session",
                createdAt: "2026-03-16T20:34:49.000Z",
                noteMarkdown: nil,
                summaries: [],
                participants: [FakeAnarlogParticipant(
                    humanID: "22222222-2222-2222-2222-222222222222",
                    displayName: nil, email: nil, jobTitle: nil)])))
        try fake.setScenario(FakeAnarlogScenario(responses: [list, get, get]))

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

        let transport = SimpleScriptTransport()
        let piClient = PiClient(
            baseURL: URL(string: "https://test.invalid")!,
            transport: transport.asFunc(), logger: NoopLogger())

        let stateURL = URL(fileURLWithPath: NSTemporaryDirectory())
            .appendingPathComponent("anarlog-fwd-\(UUID().uuidString).json")
        let stateStore = StateStore(fileURL: stateURL)
        try stateStore.initializeIfMissing()
        let mutator = StateMutator(store: stateStore)

        let fakePresenter = FakeFwdPresenter()
        var config = AnarlogConfig(
            rootPath: "/tmp/anarlog-fwd", humansEnabled: false, sessionsEnabled: true)
        try config.setOperatorPersonID("99999999-9999-9999-9999-999999999999")
        let configSource = StubFwdConfigSource(config)
        let metaLookup = AnarlogSessionMetadataLookup(
            configSource: configSource,
            makeCLIClient: { _ in client })
        let center = OrphanNotificationCenter(
            presenter: fakePresenter,
            opener: FakeFwdOpener(),
            mutator: mutator,
            metadataLookup: metaLookup,
            piURL: URL(string: "https://pi.example")!,
            needsAttentionFetcher: { [] },
            logger: NoopLogger())

        let plugin = AnarlogSessionsSourcePlugin(
            piClient: piClient,
            auth: testAuth,
            mutator: mutator,
            publisher: publisher,
            configSource: configSource,
            makeCLIClient: { _ in client },
            healthSink: NoopAnarlogHealthSink(),
            healthRegistry: SourceHealthRegistry(),
            orphanNotificationCenter: center,
            logger: NoopLogger(),
            clock: { ISO8601DateFormatter().date(from: "2026-03-20T12:00:00Z")! })

        try await plugin.tick()

        let calls = await fakePresenter.recordedCalls()
        XCTAssertEqual(calls.count, 1,
                       "plugin should forward outcome.needsAttention to center")
        XCTAssertEqual(calls[0].identifier, "orphan:\(Self.sessionUUIDA):1")
        XCTAssertTrue(calls[0].body.contains("Forwarded Session"))
        XCTAssertEqual(try fake.invocations(), [FakeAnarlogCLI.listArgv(offset: 0),
                                                 FakeAnarlogCLI.getArgv(id: Self.sessionUUIDA),
                                                 FakeAnarlogCLI.getArgv(id: Self.sessionUUIDA)])

        try? FileManager.default.removeItem(atPath: stateURL.path)
    }
}

private final class StubFwdConfigSource: AnarlogConfigSource, @unchecked Sendable {
    let cfg: AnarlogConfig?
    init(_ cfg: AnarlogConfig?) { self.cfg = cfg }
    func load() throws -> AnarlogConfig? { cfg }
}

private actor FakeFwdPresenter: UserNotificationPresenter {
    private(set) var calls: [NotificationRequestSpec] = []
    func recordedCalls() -> [NotificationRequestSpec] { calls }
    func requestAuthorization() async -> Bool { true }
    func add(_ spec: NotificationRequestSpec) async throws { calls.append(spec) }
    func removeDelivered(withIdentifiers ids: [String]) async {}
    func removePending(withIdentifiers ids: [String]) async {}
    func setDelegate(_ ref: UserNotificationDelegateRef?) async {}
    func getDeliveredIdentifiers() async -> [String] { [] }
    func getPendingIdentifiers() async -> [String] { [] }
}

private struct FakeFwdOpener: WorkspaceOpener {
    func open(_ url: URL) async -> Bool { true }
}

private final class SimpleScriptTransport: @unchecked Sendable {
    func asFunc() -> TransportFunc {
        { request in
            let path = request.url?.path ?? ""
            let method = request.httpMethod ?? "GET"
            let ok = HTTPURLResponse(url: request.url!, statusCode: 200,
                                     httpVersion: "HTTP/1.1",
                                     headerFields: ["Content-Type": "application/json"])!
            if path.hasSuffix("/cursor") && method == "GET" {
                let body = Data(#"{"success":true,"data":{"cursor":"","cursor_epoch":0,"backfill_complete":false}}"#.utf8)
                return (body, ok)
            }
            if path.hasSuffix("/known-ids") && method == "GET" {
                return (Data(#"{"success":true,"data":{"ids":[]}}"#.utf8), ok)
            }
            if path.hasSuffix("/ingest/events") && method == "POST" {
                return (Data(#"{"accepted":0,"duplicate":0,"rejected":0,"errors":[]}"#.utf8), ok)
            }
            if path.hasSuffix("/cursor") && method == "POST" {
                return (Data(#"{"success":true,"data":{"ok":true}}"#.utf8), ok)
            }
            throw URLError(.unsupportedURL)
        }
    }
}
