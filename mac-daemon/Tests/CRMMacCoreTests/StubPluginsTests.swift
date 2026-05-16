import XCTest
@testable import CRMMacCore

final class StubPluginsTests: XCTestCase {
    func testStubICloudContactsPluginHasICloudContactsID() {
        let p = StubICloudContactsPlugin(context: SourceContext(logger: NoopLogger()))
        XCTAssertEqual(p.id, .icloudContacts)
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
