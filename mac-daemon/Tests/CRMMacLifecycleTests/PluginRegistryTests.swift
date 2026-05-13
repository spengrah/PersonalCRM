import XCTest
import CRMMacCore
@testable import CRMMacLifecycle

final class PluginRegistryTests: XCTestCase {
    func testRegisterAllRegistersEachPlugin() {
        let runner = FakeScheduleRunner()
        let registry = PluginRegistry(runner: runner, logger: NoopLogger())
        let p1 = StubMessagesPlugin(context: SourceContext(logger: NoopLogger()))
        let p2 = StubICloudContactsPlugin(context: SourceContext(logger: NoopLogger()))
        registry.registerAll([p1, p2])
        XCTAssertEqual(runner.registrations.count, 2)
        XCTAssertEqual(registry.registrationCount, 2)
    }

    func testCancelAllPropagates() {
        let runner = FakeScheduleRunner()
        let registry = PluginRegistry(runner: runner, logger: NoopLogger())
        let p1 = StubMessagesPlugin(context: SourceContext(logger: NoopLogger()))
        registry.registerAll([p1])
        registry.cancelAll()
        XCTAssertEqual(runner.cancelledCount(), 1)
        XCTAssertEqual(registry.registrationCount, 0)
    }

    func testFakeRunnerFiresRegisteredPlugin() async throws {
        let runner = FakeScheduleRunner()
        let registry = PluginRegistry(runner: runner, logger: NoopLogger())
        let plugin = StubMessagesPlugin(context: SourceContext(logger: NoopLogger()))
        registry.registerAll([plugin])
        let fired = try await runner.fire(id: .messages)
        XCTAssertTrue(fired)
    }
}
