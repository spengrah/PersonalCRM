import XCTest
@testable import CRMMacCore

final class HeartbeatStateProviderTests: XCTestCase {
    func testInMemoryProviderStartsAtInitialValue() async {
        let p = InMemoryHeartbeatStateProvider(initial: 2)
        let v = await p.lastKnownPiProtocolVersion
        XCTAssertEqual(v, 2)
    }

    func testInMemoryProviderDefaultsToNil() async {
        let p = InMemoryHeartbeatStateProvider()
        let v = await p.lastKnownPiProtocolVersion
        XCTAssertNil(v)
    }

    func testInMemoryProviderSetUpdates() async {
        let p = InMemoryHeartbeatStateProvider()
        await p.set(2)
        let v = await p.lastKnownPiProtocolVersion
        XCTAssertEqual(v, 2)
    }

    func testInMemoryProviderSetNilClears() async {
        let p = InMemoryHeartbeatStateProvider(initial: 2)
        await p.set(nil)
        let v = await p.lastKnownPiProtocolVersion
        XCTAssertNil(v)
    }
}
