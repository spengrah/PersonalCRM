// DaemonVersionInSyncTests guards against drift between
// `Daemon.version` (Swift constant in CRMMacLifecycle/Daemon.swift) and
// `CFBundleShortVersionString` in Sources/crm-mac/Info.plist. The two
// must match because the daemon emits Daemon.version in pair/heartbeat
// payloads, while macOS reads CFBundleShortVersionString from the
// bundled Info.plist for system-level reporting. A future bump that
// updates one but not the other surfaces here as a test failure.
import XCTest
@testable import CRMMacLifecycle

final class DaemonVersionInSyncTests: XCTestCase {
    func testDaemonVersionMatchesCFBundleShortVersionString() throws {
        let url = URL(fileURLWithPath: #filePath)
            .deletingLastPathComponent()  // CRMMacLifecycleTests/
            .deletingLastPathComponent()  // Tests/
            .deletingLastPathComponent()  // mac-daemon/
            .appendingPathComponent("Sources/crm-mac/Info.plist")
        let data = try Data(contentsOf: url)
        let any = try PropertyListSerialization.propertyList(
            from: data, options: [], format: nil)
        let dict = any as! [String: Any]
        let plistVersion = dict["CFBundleShortVersionString"] as? String ?? ""
        XCTAssertEqual(
            plistVersion,
            Daemon.version,
            "Info.plist CFBundleShortVersionString (\(plistVersion)) must match Daemon.version (\(Daemon.version))")
    }
}
