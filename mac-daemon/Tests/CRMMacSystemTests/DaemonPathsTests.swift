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

    func testDeprecatedAliasesPointAtBundleLocations() {
        // The deprecated `binaryPath` / `plistPath` aliases point at
        // the BUNDLE locations (the post-rewrite canonical paths),
        // not at the legacy bare-binary / plist locations. The
        // legacy locations remain accessible via the dedicated
        // `legacyBinary` / `legacyPlist` accessors.
        let home = URL(fileURLWithPath: "/Users/alice")
        let paths = DaemonPaths(home: home)
        // Use the deprecated property to assert the alias still maps
        // to the bundle path.
        XCTAssertEqual(paths.binaryPath, paths.bundleBinary)
        XCTAssertEqual(paths.plistPath, paths.bundlePlist)
        // Legacy paths are separate.
        XCTAssertNotEqual(paths.binaryPath, paths.legacyBinary)
        XCTAssertNotEqual(paths.plistPath, paths.legacyPlist)
    }
}
