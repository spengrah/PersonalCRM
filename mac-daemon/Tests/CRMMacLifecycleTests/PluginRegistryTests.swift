import XCTest
import CRMMacCore
@testable import CRMMacLifecycle

/// Local test doubles — the prior no-op stubs were removed when the
/// real source plugins (MessagesSourcePlugin, ICloudContactsSourcePlugin)
/// landed; these fakes preserve PluginRegistry's wiring coverage
/// without re-introducing scheduler-level stubs.
private final class FakeMessagesPluginForRegistryTests: SourcePlugin {
    let id: SourceID = .messages
    let tickInterval: TimeInterval = 60
    func tick() async throws {}
}

private final class FakeICloudContactsPluginForRegistryTests: SourcePlugin {
    let id: SourceID = .icloudContacts
    let tickInterval: TimeInterval = 60
    func tick() async throws {}
}

final class PluginRegistryTests: XCTestCase {
    func testRegisterAllRegistersEachPlugin() {
        let runner = FakeScheduleRunner()
        let registry = PluginRegistry(runner: runner, logger: NoopLogger())
        let p1 = FakeMessagesPluginForRegistryTests()
        let p2 = FakeICloudContactsPluginForRegistryTests()
        registry.registerAll([p1, p2])
        XCTAssertEqual(runner.registrations.count, 2)
        XCTAssertEqual(registry.registrationCount, 2)
    }

    func testCancelAllPropagates() {
        let runner = FakeScheduleRunner()
        let registry = PluginRegistry(runner: runner, logger: NoopLogger())
        let p1 = FakeMessagesPluginForRegistryTests()
        registry.registerAll([p1])
        registry.cancelAll()
        XCTAssertEqual(runner.cancelledCount(), 1)
        XCTAssertEqual(registry.registrationCount, 0)
    }

    func testFakeRunnerFiresRegisteredPlugin() async throws {
        let runner = FakeScheduleRunner()
        let registry = PluginRegistry(runner: runner, logger: NoopLogger())
        let plugin = FakeMessagesPluginForRegistryTests()
        registry.registerAll([plugin])
        let fired = try await runner.fire(id: .messages)
        XCTAssertTrue(fired)
    }
}
