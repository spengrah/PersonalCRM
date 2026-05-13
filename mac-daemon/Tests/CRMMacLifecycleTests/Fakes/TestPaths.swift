import Foundation
@testable import CRMMacLifecycle

/// Convenience builder for a deterministic LifecyclePaths layout
/// rooted under /tmp/<run>. All paths are POSIX strings — the
/// InMemoryFilesystem only cares about the keys, not whether the
/// segments actually exist on disk.
struct TestPaths {
    static func make(root: String = "/tmp/crm-mac-test") -> LifecyclePaths {
        let configDir = "\(root)/Library/Application Support/crm-mac"
        let binDir = "\(configDir)/bin"
        let logsDir = "\(root)/Library/Logs/crm-mac"
        let agentsDir = "\(root)/Library/LaunchAgents"
        return LifecyclePaths(
            configDirPath: configDir,
            binDirPath: binDir,
            binaryPath: "\(binDir)/crm-mac",
            configFilePath: "\(configDir)/config.json",
            stateFilePath: "\(configDir)/state.json",
            launchAgentsDirPath: agentsDir,
            plistPath: "\(agentsDir)/\(Daemon.label).plist",
            logsDirPath: logsDir,
            stdoutLogPath: "\(logsDir)/stdout.log",
            stderrLogPath: "\(logsDir)/stderr.log")
    }
}
