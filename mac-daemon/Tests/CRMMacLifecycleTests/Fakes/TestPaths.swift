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
        let bundleApp = "\(configDir)/crm-mac.app"
        return LifecyclePaths(
            configDirPath: configDir,
            binDirPath: binDir,
            configFilePath: "\(configDir)/config.json",
            stateFilePath: "\(configDir)/state.json",
            launchAgentsDirPath: agentsDir,
            logsDirPath: logsDir,
            stdoutLogPath: "\(logsDir)/stdout.log",
            stderrLogPath: "\(logsDir)/stderr.log",
            bundleAppPath: bundleApp,
            bundleBinaryPath: "\(bundleApp)/Contents/MacOS/crm-mac",
            bundlePlistPath: "\(bundleApp)/Contents/Library/LaunchAgents/\(Daemon.label).plist",
            bundleInfoPlistPath: "\(bundleApp)/Contents/Info.plist",
            legacyBinaryPath: "\(binDir)/crm-mac",
            legacyPlistPath: "\(agentsDir)/\(Daemon.label).plist")
    }
}
