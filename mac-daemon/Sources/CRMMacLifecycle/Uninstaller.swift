// Uninstaller implements `crm-mac uninstall`.
//
// Default uninstall:
//   1. launchctl bootout (tolerates non-zero — service may not be loaded)
//   2. delete plist file
//   3. delete Keychain entry
// Does NOT remove config.json / state.json / installed binary.
//
// With --purge:
//   4. additionally remove config.json, state.json, installed binary,
//      logs (best-effort)
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
    public let bootoutInvoked: Bool
    public let bootoutExitCode: Int32
    public let plistDeleted: Bool
    public let keychainDeleted: Bool
    public let purged: Bool

    public init(
        bootoutInvoked: Bool,
        bootoutExitCode: Int32,
        plistDeleted: Bool,
        keychainDeleted: Bool,
        purged: Bool
    ) {
        self.bootoutInvoked = bootoutInvoked
        self.bootoutExitCode = bootoutExitCode
        self.plistDeleted = plistDeleted
        self.keychainDeleted = keychainDeleted
        self.purged = purged
    }
}

public struct UninstallerDependencies {
    public let paths: LifecyclePaths
    public let filesystem: FilesystemAdapter
    public let keychain: KeychainStore
    public let launchctl: LaunchctlRunner
    public let logger: LoggerProtocol

    public init(
        paths: LifecyclePaths,
        filesystem: FilesystemAdapter,
        keychain: KeychainStore,
        launchctl: LaunchctlRunner,
        logger: LoggerProtocol
    ) {
        self.paths = paths
        self.filesystem = filesystem
        self.keychain = keychain
        self.launchctl = launchctl
        self.logger = logger
    }
}

public struct Uninstaller {
    public let deps: UninstallerDependencies

    public init(_ deps: UninstallerDependencies) {
        self.deps = deps
    }

    public func run(_ request: UninstallRequest) throws -> UninstallSummary {
        // bootout — tolerate non-zero (service may not be loaded).
        var bootoutExit: Int32 = 0
        var bootoutInvoked = false
        do {
            let inv = try deps.launchctl.bootout(label: Daemon.label)
            bootoutInvoked = true
            bootoutExit = inv.exitCode
        } catch {
            deps.logger.warning("uninstall: bootout invocation failed", metadata: [
                "error": .private(String(describing: error)),
            ])
        }

        // Delete plist.
        var plistDeleted = false
        if deps.filesystem.fileExists(at: deps.paths.plistPath) {
            try deps.filesystem.remove(at: deps.paths.plistPath)
            plistDeleted = true
        }

        // Delete Keychain entry.
        var keychainDeleted = false
        do {
            try deps.keychain.deleteAPIKey()
            keychainDeleted = true
        } catch {
            deps.logger.warning("uninstall: keychain delete failed", metadata: [
                "error": .private(String(describing: error)),
            ])
        }

        var purged = false
        if request.purge {
            // The icloud_contacts hash cache lives alongside config.json
            // + state.json; include it in the purge so a re-pair on
            // the same Mac starts with no stale prior-hash bindings.
            let icloudHashCachePath = URL(fileURLWithPath: deps.paths.configDirPath)
                .appendingPathComponent("icloud_contacts_hashes.json").path
            for path in [
                deps.paths.configFilePath,
                deps.paths.stateFilePath,
                deps.paths.binaryPath,
                deps.paths.stdoutLogPath,
                deps.paths.stderrLogPath,
                icloudHashCachePath,
            ] {
                if deps.filesystem.fileExists(at: path) {
                    try? deps.filesystem.remove(at: path)
                }
            }
            purged = true
        }

        return UninstallSummary(
            bootoutInvoked: bootoutInvoked,
            bootoutExitCode: bootoutExit,
            plistDeleted: plistDeleted,
            keychainDeleted: keychainDeleted,
            purged: purged)
    }
}
