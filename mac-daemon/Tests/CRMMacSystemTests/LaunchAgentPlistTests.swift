import XCTest
@testable import CRMMacSystem

final class LaunchAgentPlistTests: XCTestCase {
    func testRenderMatchesGoldenFixture() {
        let plist = LaunchAgentPlist(
            label: "xyz.spengrah.crm-mac",
            binaryPath: "/Users/example/Library/Application Support/crm-mac/bin/crm-mac",
            configDirPath: "/Users/example/Library/Application Support/crm-mac",
            stdoutPath: "/Users/example/Library/Logs/crm-mac/stdout.log",
            stderrPath: "/Users/example/Library/Logs/crm-mac/stderr.log")
        let expected = """
        <?xml version="1.0" encoding="UTF-8"?>
        <!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
        <plist version="1.0">
        <dict>
            <key>Label</key>
            <string>xyz.spengrah.crm-mac</string>
            <key>ProgramArguments</key>
            <array>
                <string>/Users/example/Library/Application Support/crm-mac/bin/crm-mac</string>
                <string>daemon</string>
            </array>
            <key>RunAtLoad</key>
            <true/>
            <key>KeepAlive</key>
            <dict>
                <key>Crashed</key>
                <true/>
            </dict>
            <key>ProcessType</key>
            <string>Background</string>
            <key>StandardOutPath</key>
            <string>/Users/example/Library/Logs/crm-mac/stdout.log</string>
            <key>StandardErrorPath</key>
            <string>/Users/example/Library/Logs/crm-mac/stderr.log</string>
            <key>EnvironmentVariables</key>
            <dict>
                <key>CRM_MAC_CONFIG_DIR</key>
                <string>/Users/example/Library/Application Support/crm-mac</string>
            </dict>
        </dict>
        </plist>

        """
        XCTAssertEqual(plist.render(), expected)
    }

    func testPlistParsesAsPropertyList() throws {
        // The render output must parse as a PropertyList so launchd
        // will load it. We use PropertyListSerialization to confirm
        // the bytes are well-formed.
        let plist = LaunchAgentPlist(
            binaryPath: "/Users/example/bin/crm-mac",
            configDirPath: "/Users/example/cfg",
            stdoutPath: "/Users/example/logs/stdout.log",
            stderrPath: "/Users/example/logs/stderr.log")
        let data = Data(plist.render().utf8)
        let any = try PropertyListSerialization.propertyList(from: data, options: [], format: nil)
        let dict = any as! [String: Any]
        XCTAssertEqual(dict["Label"] as? String, LaunchAgentPlist.label)
        XCTAssertEqual((dict["KeepAlive"] as? [String: Any])?["Crashed"] as? Bool, true)
        XCTAssertEqual((dict["ProgramArguments"] as? [String])?.last, "daemon")
    }

    func testDaemonPathsExpandsHome() {
        let home = URL(fileURLWithPath: "/Users/alice")
        let paths = DaemonPaths(home: home)
        XCTAssertEqual(paths.configDir.path, "/Users/alice/Library/Application Support/crm-mac")
        XCTAssertEqual(paths.binaryPath.path, "/Users/alice/Library/Application Support/crm-mac/bin/crm-mac")
        XCTAssertEqual(paths.plistPath.path, "/Users/alice/Library/LaunchAgents/xyz.spengrah.crm-mac.plist")
        XCTAssertEqual(paths.stdoutLog.path, "/Users/alice/Library/Logs/crm-mac/stdout.log")
        XCTAssertEqual(paths.stderrLog.path, "/Users/alice/Library/Logs/crm-mac/stderr.log")
    }
}
