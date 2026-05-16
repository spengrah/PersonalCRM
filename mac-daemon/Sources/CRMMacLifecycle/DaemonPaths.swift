// LifecyclePaths is the path bundle the workflows take. It mirrors
// CRMMacSystem.DaemonPaths but lives in CRMMacLifecycle so the
// workflows don't take a dependency on the system target. The
// production composition root (ProductionContext) builds an instance
// from CRMMacSystem.DaemonPaths.
import Foundation

public struct LifecyclePaths: Equatable {
    public let configDirPath: String
    public let binDirPath: String
    public let binaryPath: String
    public let configFilePath: String
    public let stateFilePath: String
    public let launchAgentsDirPath: String
    public let plistPath: String
    public let logsDirPath: String
    public let stdoutLogPath: String
    public let stderrLogPath: String

    public init(
        configDirPath: String,
        binDirPath: String,
        binaryPath: String,
        configFilePath: String,
        stateFilePath: String,
        launchAgentsDirPath: String,
        plistPath: String,
        logsDirPath: String,
        stdoutLogPath: String,
        stderrLogPath: String
    ) {
        self.configDirPath = configDirPath
        self.binDirPath = binDirPath
        self.binaryPath = binaryPath
        self.configFilePath = configFilePath
        self.stateFilePath = stateFilePath
        self.launchAgentsDirPath = launchAgentsDirPath
        self.plistPath = plistPath
        self.logsDirPath = logsDirPath
        self.stdoutLogPath = stdoutLogPath
        self.stderrLogPath = stderrLogPath
    }

    /// Daemon-runtime directory.  The daemon's PidfileLock writes
    /// `daemon.pid` here so the CLI ops subcommands can detect the
    /// daemon-running state.  Defaults to the config dir so the same
    /// folder hosts config / state / pid.
    public var runtimeDirPath: String { configDirPath }
}
