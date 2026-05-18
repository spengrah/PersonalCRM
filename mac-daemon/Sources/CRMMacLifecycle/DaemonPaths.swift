// LifecyclePaths is the path bundle the workflows take. It mirrors
// CRMMacSystem.DaemonPaths but lives in CRMMacLifecycle so the
// workflows don't take a dependency on the system target. The
// production composition root (ProductionContext) builds an instance
// from CRMMacSystem.DaemonPaths.
//
// The bundle paths (bundleAppPath, bundleBinaryPath, bundlePlistPath,
// bundleInfoPlistPath) point at the assembled `crm-mac.app` under
// `~/Library/Application Support/crm-mac/`. The legacy paths
// (legacyBinaryPath = `~/.../bin/crm-mac`, legacyPlistPath =
// `~/Library/LaunchAgents/<label>.plist`) are kept for one-shot
// migration detection + cleanup. The pre-rewrite fields `binaryPath`
// and `plistPath` are retained as aliases for the legacy locations
// (NOT the bundle locations) during the transition; downstream
// callers are being migrated to use the explicit `bundleBinaryPath`
// / `legacyBinaryPath` / `bundlePlistPath` / `legacyPlistPath` names
// where the distinction matters.
import Foundation

public struct LifecyclePaths: Equatable {
    public let configDirPath: String
    public let binDirPath: String
    public let configFilePath: String
    public let stateFilePath: String
    public let launchAgentsDirPath: String
    public let logsDirPath: String
    public let stdoutLogPath: String
    public let stderrLogPath: String

    /// Final installed bundle path
    /// (`~/Library/Application Support/crm-mac/crm-mac.app`).
    public let bundleAppPath: String
    /// Daemon binary inside the bundle
    /// (`<bundleAppPath>/Contents/MacOS/crm-mac`).
    public let bundleBinaryPath: String
    /// Embedded LaunchAgent plist inside the bundle
    /// (`<bundleAppPath>/Contents/Library/LaunchAgents/<label>.plist`).
    public let bundlePlistPath: String
    /// Embedded Info.plist inside the bundle
    /// (`<bundleAppPath>/Contents/Info.plist`).
    public let bundleInfoPlistPath: String

    /// Pre-bundle bare-binary location used by the legacy install.
    /// Migration detects + removes this file.
    public let legacyBinaryPath: String
    /// Pre-bundle LaunchAgent plist location used by the legacy
    /// install (`~/Library/LaunchAgents/<label>.plist`). Migration
    /// bootouts the legacy launchd registration + removes the file.
    public let legacyPlistPath: String

    public init(
        configDirPath: String,
        binDirPath: String,
        configFilePath: String,
        stateFilePath: String,
        launchAgentsDirPath: String,
        logsDirPath: String,
        stdoutLogPath: String,
        stderrLogPath: String,
        bundleAppPath: String,
        bundleBinaryPath: String,
        bundlePlistPath: String,
        bundleInfoPlistPath: String,
        legacyBinaryPath: String,
        legacyPlistPath: String
    ) {
        self.configDirPath = configDirPath
        self.binDirPath = binDirPath
        self.configFilePath = configFilePath
        self.stateFilePath = stateFilePath
        self.launchAgentsDirPath = launchAgentsDirPath
        self.logsDirPath = logsDirPath
        self.stdoutLogPath = stdoutLogPath
        self.stderrLogPath = stderrLogPath
        self.bundleAppPath = bundleAppPath
        self.bundleBinaryPath = bundleBinaryPath
        self.bundlePlistPath = bundlePlistPath
        self.bundleInfoPlistPath = bundleInfoPlistPath
        self.legacyBinaryPath = legacyBinaryPath
        self.legacyPlistPath = legacyPlistPath
    }

    /// Deprecated alias for `bundleBinaryPath`. The post-rewrite
    /// daemon binary lives inside the bundle at
    /// `<bundleAppPath>/Contents/MacOS/crm-mac`; out-of-tree callers
    /// reading `binaryPath` should see THAT location (not the legacy
    /// bare binary). The legacy bare binary remains accessible via
    /// `legacyBinaryPath` for migration code.
    @available(*, deprecated, message: "Use bundleBinaryPath (or legacyBinaryPath for migration probes)")
    public var binaryPath: String { bundleBinaryPath }

    /// Deprecated alias for `bundlePlistPath`. The post-rewrite
    /// LaunchAgent plist lives inside the bundle at
    /// `<bundleAppPath>/Contents/Library/LaunchAgents/<label>.plist`;
    /// out-of-tree callers reading `plistPath` should see THAT
    /// location. The legacy launchd plist remains accessible via
    /// `legacyPlistPath` for migration cleanup.
    @available(*, deprecated, message: "Use bundlePlistPath (or legacyPlistPath for migration cleanup)")
    public var plistPath: String { bundlePlistPath }

    /// Daemon-runtime directory.  The daemon's PidfileLock writes
    /// `daemon.pid` here so the CLI ops subcommands can detect the
    /// daemon-running state.  Defaults to the config dir so the same
    /// folder hosts config / state / pid.
    public var runtimeDirPath: String { configDirPath }

    /// On-disk plaintext file holding the daemon's API key (0600).
    /// Replaces the prior macOS Keychain entry — see FileAPIKeyStore.
    public var apiKeyFilePath: String { "\(configDirPath)/api-key" }

    /// Daemon pidfile path. The daemon's PidfileLock writes its pid
    /// here; the Installer/Uninstaller read it to send SIGTERM via
    /// ProcessSignaller.
    public var pidfilePath: String { "\(runtimeDirPath)/daemon.pid" }
}
