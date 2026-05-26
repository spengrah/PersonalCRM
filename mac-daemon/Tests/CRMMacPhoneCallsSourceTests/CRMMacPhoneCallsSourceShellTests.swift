import XCTest
@testable import CRMMacPhoneCallsSource

final class CRMMacPhoneCallsSourceShellTests: XCTestCase {
    func testPayloadVersionIsOne() {
        XCTAssertEqual(CRMMacPhoneCallsSource.payloadVersion, 1)
    }

    func testMinPiProtocolVersionIsTwo() {
        // The Pi must accept call.* events to receive any payload from
        // this source; protocol_version 2 is the marker.
        XCTAssertEqual(CRMMacPhoneCallsSource.minPiProtocolVersion, 2)
    }
}
