// Uninstaller implements `crm-mac uninstall`.
//
// Default uninstall (plan D9):
//   1. Stop the running daemon (SIGTERM via ProcessSignaller +
//      pidfile-poll up to 10s). Tolerant — uninstall continues even
//      if the daemon doesn't exit cleanly.
//   2. SMAppService.unregister (tolerant of "not registered" errors).
//   3. Delete the bundle directory at paths.bundleAppPath
//      (recursive).
//   4. Delete the api-key entry.
//   5. Legacy cleanup (unconditional, best-effort): bootout the
//      legacy launchctl registration if the legacy plist exists;
//      delete the legacy plist file + legacy bare binary if present.
//
// With --purge:
//   6. additionally remove config.json, state.json, logs, the
//      icloud_contacts hash cache, and (on purge) the legacy bin/
//      directory if empty.
//
// Pi-side mac_host row is NOT touched — `crm-admin --revoke-host <id>`
// is the Pi-side knob.
import Foundation
import CRMMacCore

public struct UninstallRequest: Equatable {
    public let purge: Bool
    public init(purge: Bool = false) {
        self.purge = purge
    }
}

public struct UninstallSummary: Equatable {
    /// True iff the SIGTERM + pidfile poll succeeded (or no pidfile
    /// was present). False if the daemon refused to stop within the
    /// timeout — uninstall continues regardless; operator can
    /// `kill -9` separately.
    public let daemonStopped: Bool
    /// True iff `agentService.unregister()` was called. The call may
    /// have thrown an "already not registered" error and still
    /// returns true here — this field tracks invocation, not
    /// success.
    public let unregisterInvoked: Bool
    /// True iff the bundle directory existed and was removed.
    public let bundleDeleted: Bool
    /// True iff the api-key entry existed and was deleted.
    public let keychainDeleted: Bool
    /// True iff the legacy launchd plist file at
    /// ~/Library/LaunchAgents/<label>.plist existed and was removed.
    /// False on a fresh install with no legacy artifacts (the common
    /// post-rewrite case).
    public let legacyPlistDeleted: Bool
    /// True iff the legacy bare binary at <configDir>/bin/crm-mac
    /// existed and was removed.
    public let legacyBinaryDeleted: Bool
    public let purged: Bool

    public init(
        daemonStopped: Bool,
        unregisterInvoked: Bool,
        bundleDeleted: Bool,
        keychainDeleted: Bool,
        legacyPlistDeleted: Bool,
        legacyBinaryDeleted: Bool,
        purged: Bool
    ) {
        self.daemonStopped = daemonStopped
        self.unregisterInvoked = unregisterInvoked
        self.bundleDeleted = bundleDeleted
        self.keychainDeleted = keychainDeleted
        self.legacyPlistDeleted = legacyPlistDeleted
        self.legacyBinaryDeleted = legacyBinaryDeleted
        self.purged = purged
    }
}

public struct UninstallerDependencies {
    public let paths: LifecyclePaths
    public let filesystem: FilesystemAdapter
    public let keychain: KeychainStore
    public let agentService: AgentService
    public let processSignaller: ProcessSignaller
    public let logger: LoggerProtocol
    /// Legacy launchctl runner for cleaning up pre-rewrite installs.
    /// Nil in tests that don't seed legacy artifacts.
    public let legacyLaunchctl: LaunchctlRunner?
    /// Timeout (seconds) for the stop-daemon SIGTERM + pidfile poll.
    /// Default 10s.
    public let stopDaemonTimeoutSeconds: TimeInterval

    public init(
        paths: LifecyclePaths,
        filesystem: FilesystemAdapter,
        keychain: KeychainStore,
        agentService: AgentService,
        processSignaller: ProcessSignaller,
        logger: LoggerProtocol,
        legacyLaunchctl: LaunchctlRunner? = nil,
        stopDaemonTimeoutSeconds: TimeInterval = 10
    ) {
        self.paths = paths
        self.filesystem = filesystem
        self.keychain = keychain
        self.agentService = agentService
        self.processSignaller = processSignaller
        self.logger = logger
        self.legacyLaunchctl = legacyLaunchctl
        self.stopDaemonTimeoutSeconds = stopDaemonTimeoutSeconds
    }
}

public struct Uninstaller {
    public let deps: UninstallerDependencies

    public init(_ deps: UninstallerDependencies) {
        self.deps = deps
    }

    public func run(_ request: UninstallRequest) async throws -> UninstallSummary {
        // 1. Stop the running daemon. Tolerant of failure — the
        // uninstall continues so config + plist cleanup still runs.
        let daemonStopped = await stopRunningDaemonTolerant()

        // 2. Unregister via SMAppService. Tolerant of
        // "already-not-registered" errors.
        var unregisterInvoked = false
        do {
            try await deps.agentService.unregister()
            unregisterInvoked = true
        } catch {
            unregisterInvoked = true  // we DID call it; it just threw
            deps.logger.warning("uninstall: agentService.unregister failed (continuing)", metadata: [
                "error": .private(String(describing: error)),
            ])
        }

        // 3. Delete the bundle directory.
        var bundleDeleted = false
        if deps.filesystem.fileExists(at: deps.paths.bundleAppPath) {
            do {
                try deps.filesystem.remove(at: deps.paths.bundleAppPath)
                bundleDeleted = true
            } catch {
                deps.logger.warning("uninstall: bundle delete failed", metadata: [
                    "error": .private(String(describing: error)),
                ])
            }
        }

        // 4. Delete api-key.
        var keychainDeleted = false
        do {
            try deps.keychain.deleteAPIKey()
            keychainDeleted = true
        } catch {
            deps.logger.warning("uninstall: keychain delete failed", metadata: [
                "error": .private(String(describing: error)),
            ])
        }

        // 5. Legacy cleanup (best-effort).
        let (legacyPlistDeleted, legacyBinaryDeleted) = cleanupLegacyArtifacts()

        // 6. Purge — remove user-data + logs + icloud hash cache.
        var purged = false
        if request.purge {
            let icloudHashCachePath = URL(fileURLWithPath: deps.paths.configDirPath)
                .appendingPathComponent("icloud_contacts_hashes.json").path
            for path in [
                deps.paths.configFilePath,
                deps.paths.stateFilePath,
                deps.paths.stdoutLogPath,
                deps.paths.stderrLogPath,
                icloudHashCachePath,
            ] {
                if deps.filesystem.fileExists(at: path) {
                    try? deps.filesystem.remove(at: path)
                }
            }
            // Drop the legacy bin/ dir (empty by this point).
            if deps.filesystem.fileExists(at: deps.paths.binDirPath) {
                try? deps.filesystem.remove(at: deps.paths.binDirPath)
            }
            purged = true
        }

        return UninstallSummary(
            daemonStopped: daemonStopped,
            unregisterInvoked: unregisterInvoked,
            bundleDeleted: bundleDeleted,
            keychainDeleted: keychainDeleted,
            legacyPlistDeleted: legacyPlistDeleted,
            legacyBinaryDeleted: legacyBinaryDeleted,
            purged: purged)
    }

    /// SIGTERM the running daemon if a pidfile is present; poll for
    /// release. Returns true on clean stop or no-pidfile; false on
    /// timeout. Tolerant — never throws.
    private func stopRunningDaemonTolerant() async -> Bool {
        let pidfilePath = deps.paths.pidfilePath
        guard deps.filesystem.fileExists(at: pidfilePath) else {
            return true
        }
        let pidData: Data
        do {
            pidData = try deps.filesystem.read(from: pidfilePath)
        } catch {
            return true
        }
        let raw = String(data: pidData, encoding: .utf8) ?? ""
        guard let pid = pid_t(raw.trimmingCharacters(in: .whitespacesAndNewlines)) else {
            return true
        }
        do {
            try deps.processSignaller.sendSIGTERM(pid: pid)
        } catch {
            deps.logger.warning("uninstall: SIGTERM failed (continuing)", metadata: [
                "pid": .public("\(pid)"),
                "error": .private("\(error)"),
            ])
        }
        return await deps.processSignaller.waitForPidfileRelease(
            path: pidfilePath,
            timeoutSeconds: deps.stopDaemonTimeoutSeconds)
    }

    /// Best-effort delete of legacy launchd plist + legacy bare
    /// binary. Returns (plistDeleted, binaryDeleted).
    private func cleanupLegacyArtifacts() -> (Bool, Bool) {
        var plistDeleted = false
        var binaryDeleted = false
        if deps.filesystem.fileExists(at: deps.paths.legacyPlistPath) {
            // Bootout the legacy registration if launchctl is wired
            // (tolerant of non-zero — service may not be loaded).
            if let legacy = deps.legacyLaunchctl {
                _ = try? legacy.bootout(label: Daemon.label)
            }
            do {
                try deps.filesystem.remove(at: deps.paths.legacyPlistPath)
                plistDeleted = true
            } catch {
                deps.logger.warning("uninstall: legacy plist delete failed", metadata: [
                    "error": .private(String(describing: error)),
                ])
            }
        }
        if deps.filesystem.fileExists(at: deps.paths.legacyBinaryPath) {
            do {
                try deps.filesystem.remove(at: deps.paths.legacyBinaryPath)
                binaryDeleted = true
            } catch {
                deps.logger.warning("uninstall: legacy binary delete failed", metadata: [
                    "error": .private(String(describing: error)),
                ])
            }
        }
        return (plistDeleted, binaryDeleted)
    }
}
