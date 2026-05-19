// User-domain path bundle for the production install. The plist
// generator itself lives in CRMMacLifecycle as a pure-string render
// shared by Installer + tests.
//
// As of the SMAppService rewrite the install target is a
// `crm-mac.app` bundle under `~/Library/Application Support/crm-mac/`
// rather than a bare binary under `bin/`. The legacy paths
// (legacyBinaryPath / legacyPlistPath) are kept for one-shot
// migration detection + cleanup.
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
    public var configFile: URL {
        configDir.appendingPathComponent("config.json")
    }
    public var stateFile: URL {
        configDir.appendingPathComponent("state.json")
    }
    public var launchAgentsDir: URL {
        home.appendingPathComponent("Library/LaunchAgents", isDirectory: true)
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

    // Bundle (new install layout — SMAppService).
    public var bundleApp: URL {
        configDir.appendingPathComponent("crm-mac.app", isDirectory: true)
    }
    public var bundleBinary: URL {
        bundleApp.appendingPathComponent("Contents/MacOS/crm-mac")
    }
    public var bundlePlist: URL {
        bundleApp.appendingPathComponent("Contents/Library/LaunchAgents/\(Daemon.label).plist")
    }
    public var bundleInfoPlist: URL {
        bundleApp.appendingPathComponent("Contents/Info.plist")
    }

    // Legacy paths (kept for migration detection + cleanup).
    public var legacyBinary: URL {
        binDir.appendingPathComponent("crm-mac")
    }
    public var legacyPlist: URL {
        launchAgentsDir.appendingPathComponent("\(Daemon.label).plist")
    }

    /// Deprecated alias for `bundleBinary`. Out-of-tree callers
    /// reading `binaryPath` should see the in-bundle binary path
    /// (NOT the legacy bare binary, which has its own dedicated
    /// accessor `legacyBinary`).
    @available(*, deprecated, message: "Use bundleBinary (or legacyBinary for migration probes)")
    public var binaryPath: URL { bundleBinary }

    /// Deprecated alias for `bundlePlist`. Out-of-tree callers
    /// reading `plistPath` should see the in-bundle plist path.
    /// The legacy launchd plist remains accessible via `legacyPlist`.
    @available(*, deprecated, message: "Use bundlePlist (or legacyPlist for migration cleanup)")
    public var plistPath: URL { bundlePlist }

    /// Daemon-runtime directory.  The daemon's PidfileLock writes
    /// `daemon.pid` here so the CLI ops subcommands can detect the
    /// daemon-running state.  Defaults to the configDir so the same
    /// folder hosts config/state/pid.
    public var runtimeDir: URL {
        configDir
    }
    /// On-disk plaintext file holding the daemon's API key (0600).
    /// Replaces the prior macOS Keychain entry — see FileAPIKeyStore.
    public var apiKeyFile: URL {
        configDir.appendingPathComponent("api-key")
    }
}
