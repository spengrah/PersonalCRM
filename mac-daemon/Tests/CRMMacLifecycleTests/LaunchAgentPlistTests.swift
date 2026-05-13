import XCTest
@testable import CRMMacLifecycle

final class LaunchAgentPlistTests: XCTestCase {
    func testRenderMatchesGoldenFixture() {
        let plist = LaunchAgentPlist(
            label: Daemon.label,
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
        let plist = LaunchAgentPlist(
            label: Daemon.label,
            binaryPath: "/Users/example/bin/crm-mac",
            configDirPath: "/Users/example/cfg",
            stdoutPath: "/Users/example/logs/stdout.log",
            stderrPath: "/Users/example/logs/stderr.log")
        let data = Data(plist.render().utf8)
        let any = try PropertyListSerialization.propertyList(from: data, options: [], format: nil)
        let dict = any as! [String: Any]
        XCTAssertEqual(dict["Label"] as? String, Daemon.label)
        XCTAssertEqual((dict["KeepAlive"] as? [String: Any])?["Crashed"] as? Bool, true)
        XCTAssertEqual((dict["ProgramArguments"] as? [String])?.last, "daemon")
    }

    func testXMLEscapesSpecialCharacters() throws {
        let plist = LaunchAgentPlist(
            label: Daemon.label,
            binaryPath: "/Users/o'malley/Library/bin/crm-mac",
            configDirPath: "/Users/<weird>",
            stdoutPath: "/Users/a&b/stdout.log",
            stderrPath: "/Users/a&b/stderr.log").render()
        // Critical: parseable as a plist even with special chars.
        let data = Data(plist.utf8)
        let any = try PropertyListSerialization.propertyList(from: data, options: [], format: nil)
        let dict = any as! [String: Any]
        let programArgs = dict["ProgramArguments"] as! [String]
        XCTAssertEqual(programArgs.first, "/Users/o'malley/Library/bin/crm-mac")
        // CRM_MAC_CONFIG_DIR survives < > round-trip.
        let env = dict["EnvironmentVariables"] as! [String: String]
        XCTAssertEqual(env["CRM_MAC_CONFIG_DIR"], "/Users/<weird>")
        // stderr path round-trips with & intact.
        XCTAssertEqual(dict["StandardErrorPath"] as? String, "/Users/a&b/stderr.log")
    }
}
