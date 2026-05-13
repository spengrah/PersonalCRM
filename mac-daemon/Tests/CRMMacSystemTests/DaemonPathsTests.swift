import XCTest
@testable import CRMMacSystem
import CRMMacLifecycle

final class DaemonPathsTests: XCTestCase {
    func testExpandsHome() {
        let home = URL(fileURLWithPath: "/Users/alice")
        let paths = DaemonPaths(home: home)
        XCTAssertEqual(paths.configDir.path, "/Users/alice/Library/Application Support/crm-mac")
        XCTAssertEqual(paths.binaryPath.path, "/Users/alice/Library/Application Support/crm-mac/bin/crm-mac")
        XCTAssertEqual(paths.plistPath.path, "/Users/alice/Library/LaunchAgents/\(Daemon.label).plist")
        XCTAssertEqual(paths.stdoutLog.path, "/Users/alice/Library/Logs/crm-mac/stdout.log")
        XCTAssertEqual(paths.stderrLog.path, "/Users/alice/Library/Logs/crm-mac/stderr.log")
    }
}
