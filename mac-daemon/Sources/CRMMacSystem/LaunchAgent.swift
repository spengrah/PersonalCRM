// LaunchAgent generates the `<label>.plist` content for the user-
// domain launchd agent. Pure string generation — no system framework
// imports here. Grouped with CRMMacSystem because its primary
// consumer is ProductionLaunchctlRunner, which DOES shell out.
//
// Plan D14 specifies the exact plist shape: Label, ProgramArguments,
// RunAtLoad, KeepAlive={Crashed:true}, ProcessType=Background,
// StandardOut/ErrPath, EnvironmentVariables{CRM_MAC_CONFIG_DIR}.
import Foundation

public struct LaunchAgentPlist {
    public static let label = "xyz.spengrah.crm-mac"
    public static let configDirName = "crm-mac"

    public let label: String
    public let binaryPath: String
    public let configDirPath: String
    public let stdoutPath: String
    public let stderrPath: String

    public init(
        label: String = LaunchAgentPlist.label,
        binaryPath: String,
        configDirPath: String,
        stdoutPath: String,
        stderrPath: String
    ) {
        self.label = label
        self.binaryPath = binaryPath
        self.configDirPath = configDirPath
        self.stdoutPath = stdoutPath
        self.stderrPath = stderrPath
    }

    /// Generated plist content. Deterministic byte-for-byte string so
    /// the golden-fixture test works.
    public func render() -> String {
        return """
        <?xml version="1.0" encoding="UTF-8"?>
        <!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
        <plist version="1.0">
        <dict>
            <key>Label</key>
            <string>\(label)</string>
            <key>ProgramArguments</key>
            <array>
                <string>\(binaryPath)</string>
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
            <string>\(stdoutPath)</string>
            <key>StandardErrorPath</key>
            <string>\(stderrPath)</string>
            <key>EnvironmentVariables</key>
            <dict>
                <key>CRM_MAC_CONFIG_DIR</key>
                <string>\(configDirPath)</string>
            </dict>
        </dict>
        </plist>

        """
    }
}

/// Default user-domain paths for the install. Constructed from
/// NSHomeDirectory() at install time so they expand to the real
/// operator's home rather than a build-time constant.
public struct DaemonPaths {
    public let home: URL
    public init(home: URL = URL(fileURLWithPath: NSHomeDirectory())) {
        self.home = home
    }

    public var configDir: URL {
        home.appendingPathComponent("Library/Application Support/crm-mac", isDirectory: true)
    }
    public var binDir: URL {
        configDir.appendingPathComponent("bin", isDirectory: true)
    }
    public var binaryPath: URL {
        binDir.appendingPathComponent("crm-mac")
    }
    public var configFile: URL {
        configDir.appendingPathComponent("config.json")
    }
    public var stateFile: URL {
        configDir.appendingPathComponent("state.json")
    }
    public var launchAgentsDir: URL {
        home.appendingPathComponent("Library/LaunchAgents", isDirectory: true)
    }
    public var plistPath: URL {
        launchAgentsDir.appendingPathComponent("\(LaunchAgentPlist.label).plist")
    }
    public var logsDir: URL {
        home.appendingPathComponent("Library/Logs/crm-mac", isDirectory: true)
    }
    public var stdoutLog: URL {
        logsDir.appendingPathComponent("stdout.log")
    }
    public var stderrLog: URL {
        logsDir.appendingPathComponent("stderr.log")
    }
}
