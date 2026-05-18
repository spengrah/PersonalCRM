import XCTest
@testable import CRMMacSystem
import CRMMacLifecycle

final class DaemonPathsTests: XCTestCase {
    func testExpandsHome() {
        let home = URL(fileURLWithPath: "/Users/alice")
        let paths = DaemonPaths(home: home)
        XCTAssertEqual(
            paths.configDir.path,
            "/Users/alice/Library/Application Support/crm-mac")
        // Bundle layout (new install location — SMAppService).
        XCTAssertEqual(
            paths.bundleApp.path,
            "/Users/alice/Library/Application Support/crm-mac/crm-mac.app")
        XCTAssertEqual(
            paths.bundleBinary.path,
            "/Users/alice/Library/Application Support/crm-mac/crm-mac.app/Contents/MacOS/crm-mac")
        XCTAssertEqual(
            paths.bundleInfoPlist.path,
            "/Users/alice/Library/Application Support/crm-mac/crm-mac.app/Contents/Info.plist")
        XCTAssertEqual(
            paths.bundlePlist.path,
            "/Users/alice/Library/Application Support/crm-mac/crm-mac.app/Contents/Library/LaunchAgents/\(Daemon.label).plist")
        // Legacy paths (kept for migration detection).
        XCTAssertEqual(
            paths.legacyBinary.path,
            "/Users/alice/Library/Application Support/crm-mac/bin/crm-mac")
        XCTAssertEqual(
            paths.legacyPlist.path,
            "/Users/alice/Library/LaunchAgents/\(Daemon.label).plist")
        // Other paths unchanged.
        XCTAssertEqual(paths.stdoutLog.path, "/Users/alice/Library/Logs/crm-mac/stdout.log")
        XCTAssertEqual(paths.stderrLog.path, "/Users/alice/Library/Logs/crm-mac/stderr.log")
        XCTAssertEqual(paths.apiKeyFile.path, "/Users/alice/Library/Application Support/crm-mac/api-key")
    }

    func testLegacyAliasesMirrorLegacyPaths() {
        // Pre-rewrite deprecated aliases. Kept while the Installer
        // rewrite is rolled out commit-by-commit. Both must point at
        // the LEGACY locations so existing call sites preserve their
        // semantics.
        let home = URL(fileURLWithPath: "/Users/alice")
        let paths = DaemonPaths(home: home)
        XCTAssertEqual(paths.binaryPath, paths.legacyBinary)
        XCTAssertEqual(paths.plistPath, paths.legacyPlist)
    }
}
