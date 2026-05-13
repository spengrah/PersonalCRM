// User-domain path bundle for the production install. The plist
// generator itself lives in CRMMacLifecycle as a pure-string render
// shared by Installer + tests.
import Foundation
import CRMMacLifecycle

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
        launchAgentsDir.appendingPathComponent("\(Daemon.label).plist")
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
