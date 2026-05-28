// Coverage for NotificationReconcileLoopPlugin — verifies the
// tick() body calls reconcile() and that reconcile() errors
// don't escalate.
import XCTest
import CRMMacCore
import CRMMacPiClient
@testable import CRMMacOrphanNotifications

final class NotificationReconcileLoopPluginTests: XCTestCase {
    private let piURL = URL(string: "https://pi.example")!

    private var tempStateURL: URL!
    private var stateStore: StateStore!
    private var mutator: StateMutator!

    override func setUpWithError() throws {
        try super.setUpWithError()
        let tmpDir = FileManager.default.temporaryDirectory
            .appendingPathComponent("crm-mac-reconcile-loop-tests-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: tmpDir, withIntermediateDirectories: true)
        tempStateURL = tmpDir.appendingPathComponent("state.json")
        stateStore = StateStore(fileURL: tempStateURL)
        try stateStore.save(DaemonState())
        mutator = StateMutator(store: stateStore)
    }

    override func tearDownWithError() throws {
        try? FileManager.default.removeItem(at: tempStateURL.deletingLastPathComponent())
        try super.tearDownWithError()
    }

    actor FetcherCallCounter {
        private(set) var count: Int = 0
        func bump() { count += 1 }
    }

    func testTickCallsReconcileOnce() async throws {
        let counter = FetcherCallCounter()
        let center = OrphanNotificationCenter(
            presenter: FakeUserNotificationPresenter(authorizationResult: true),
            opener: FakeWorkspaceOpener(),
            mutator: mutator,
            metadataLookup: FakeSessionMetadataLookup(),
            piURL: piURL,
            needsAttentionFetcher: {
                await counter.bump()
                return []
            },
            logger: NoopLogger())
        let plugin = NotificationReconcileLoopPlugin(
            center: center, logger: NoopLogger())
        try await plugin.tick()
        let n = await counter.count
        XCTAssertEqual(n, 1)
    }

    private struct TestError: Error {}

    func testTickReconcileErrorDoesNotThrow() async throws {
        // reconcile() catches fetcher errors internally + logs;
        // the plugin's tick must not propagate.
        let center = OrphanNotificationCenter(
            presenter: FakeUserNotificationPresenter(authorizationResult: true),
            opener: FakeWorkspaceOpener(),
            mutator: mutator,
            metadataLookup: FakeSessionMetadataLookup(),
            piURL: piURL,
            needsAttentionFetcher: { throw TestError() },
            logger: NoopLogger())
        let plugin = NotificationReconcileLoopPlugin(
            center: center, logger: NoopLogger())
        // Must not throw.
        try await plugin.tick()
    }

    func testPluginIDIsNotificationReconcile() {
        let center = OrphanNotificationCenter(
            presenter: FakeUserNotificationPresenter(),
            opener: FakeWorkspaceOpener(),
            mutator: mutator,
            metadataLookup: FakeSessionMetadataLookup(),
            piURL: piURL,
            needsAttentionFetcher: { [] },
            logger: NoopLogger())
        let plugin = NotificationReconcileLoopPlugin(
            center: center, logger: NoopLogger())
        XCTAssertEqual(plugin.id.rawValue, "notification_reconcile")
        XCTAssertEqual(plugin.tickInterval, 300)
    }
}
