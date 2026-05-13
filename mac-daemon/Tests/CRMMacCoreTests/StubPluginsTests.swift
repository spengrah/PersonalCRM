import XCTest
@testable import CRMMacCore

final class StubPluginsTests: XCTestCase {
    func testStubMessagesPluginHasMessagesID() {
        let p = StubMessagesPlugin(context: SourceContext(logger: NoopLogger()))
        XCTAssertEqual(p.id, .messages)
        XCTAssertEqual(p.tickInterval, 60)
    }

    func testStubICloudContactsPluginHasICloudContactsID() {
        let p = StubICloudContactsPlugin(context: SourceContext(logger: NoopLogger()))
        XCTAssertEqual(p.id, .icloudContacts)
    }

    func testStubMessagesPluginTickDoesNotThrow() async throws {
        let plugin = StubMessagesPlugin(context: SourceContext(logger: NoopLogger()))
        try await plugin.tick()
    }

    func testStubICloudContactsPluginTickDoesNotThrow() async throws {
        let plugin = StubICloudContactsPlugin(context: SourceContext(logger: NoopLogger()))
        try await plugin.tick()
    }

    func testSourceIDRawValues() {
        XCTAssertEqual(SourceID.messages.rawValue, "messages")
        XCTAssertEqual(SourceID.icloudContacts.rawValue, "icloud_contacts")
    }
}
