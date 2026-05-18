// Installer implements `crm-mac install`, `--upgrade`, and
// `--register-only` against the SMAppService + crm-mac.app bundle
// architecture.
//
// Fresh-install sequence:
//   1. Validate inputs.
//   2. Preflight: refuse if existing install detected.
//   3. mkdir -p config/logs directories (NO bin/ — the bare-binary
//      install location is migration-only).
//   4. Assemble the new bundle at a tmp path
//      (`<configDir>/crm-mac.app.tmp.<pid>`) via BundleAssembler.
//   5. POST /api/v1/host on the Pi.
//   6. Persist config.json, api-key file, state.json.
//   7. Atomic-rename tmp bundle to the install path
//      (`<configDir>/crm-mac.app`). Destination is absent — preflight
//      refused otherwise — so it's a same-parent, empty-destination
//      rename (atomic on APFS).
//   8. Substitute `__INSTALL_PREFIX__` placeholder in the bundle's
//      embedded LaunchAgents plist with the real install-time bundle
//      path.
//   9. Register the agent with SMAppService.
//
// Upgrade sequence (backup-rename-swap):
//   - Validate; read existing config; check legacy migration.
//   - Stop the running daemon (SMAppService.unregister + SIGTERM
//     via ProcessSignaller; pidfile-poll up to 10s).
//   - Rename existing crm-mac.app -> crm-mac.app.backup.<pid>
//     (same-parent, empty-destination rename — atomic).
//   - Assemble new bundle at crm-mac.app.tmp.<pid>.
//   - Atomic-rename tmp bundle -> crm-mac.app (destination absent
//     again because the prior rename moved the old one).
//   - Substitute placeholder + register.
//   - rm -rf crm-mac.app.backup.<pid> (best-effort).
//   - On any failure between assemble and register: rm -rf tmp
//     bundle AND rename backup back to crm-mac.app (rollback). If
//     the restore itself fails the composed `upgradeRollbackFailed`
//     error carries both the original failure and the restore failure.
//
// Legacy migration (`runLegacyMigrationIfNeeded`):
//   Detects a pre-bundle bare-binary install
//   (~/.../bin/crm-mac + ~/Library/LaunchAgents/<label>.plist) and
//   stops the legacy daemon + bootouts the legacy launchd
//   registration BEFORE bundle assembly (labels collide between
//   legacy and SMAppService registrations). After register, deletes
//   the legacy plist file + bare binary best-effort via
//   `cleanupLegacyArtifactsIfAny`. The cleanup runs only on a
//   successful register so a partial-migration retry can still see
//   the legacy files.
import Foundation
import CRMMacCore
import CRMMacPiClient

public struct InstallRequest: Equatable {
    public let piURL: URL
    public let pairingToken: String
    public let hostname: String
    public let upgrade: Bool
    public let registerOnly: Bool

    public init(
        piURL: URL,
        pairingToken: String,
        hostname: String,
        upgrade: Bool = false,
        registerOnly: Bool = false
    ) {
        self.piURL = piURL
        self.pairingToken = pairingToken
        self.hostname = hostname
        self.upgrade = upgrade
        self.registerOnly = registerOnly
    }
}

public struct InstallSummary: Equatable {
    public let hostID: UUID
    public let cursorEpoch: Int64
    /// Path to the installed daemon binary inside the bundle:
    /// `<bundleAppPath>/Contents/MacOS/crm-mac`.
    public let bundleBinaryPath: String
    /// Path to the assembled bundle: `<bundleAppPath>`. Includes the
    /// `.app` extension.
    public let bundleAppPath: String

    public init(
        hostID: UUID,
        cursorEpoch: Int64,
        bundleBinaryPath: String,
        bundleAppPath: String
    ) {
        self.hostID = hostID
        self.cursorEpoch = cursorEpoch
        self.bundleBinaryPath = bundleBinaryPath
        self.bundleAppPath = bundleAppPath
    }

    /// Pre-rewrite alias preserved for caller print statements that
    /// historically rendered the binary path.
    public var binaryPath: String { bundleBinaryPath }
}

public enum InstallError: Error, CustomStringConvertible {
    case missingHostnameFlag
    case invalidPairingToken
    case alreadyInstalled
    case noExistingInstall
    case pairFailed(PiClientError)
    case ambiguousPair(underlying: String)
    case persistFailed(hostID: UUID, stage: String, underlying: String)
    /// SMAppService.register() failed. `requiresApproval=true` when
    /// the underlying NSError code matches
    /// `kSMErrorLaunchDeniedByUser` (operator denied in
    /// System Settings → Login Items). All other failures surface
    /// with `requiresApproval=false`.
    case agentRegistrationFailed(message: String, requiresApproval: Bool)
    /// Stop-the-running-daemon timed out during upgrade or migration.
    /// `pid` is read from the pidfile so the operator can `kill -TERM
    /// <pid>` manually.
    case daemonStillRunning(pid: pid_t)
    /// Legacy launchctl bootout verification reported the legacy
    /// registration is STILL loaded after the grace period. Operator
    /// must manually run `launchctl bootout gui/$(id -u)/<label>`
    /// and re-run.
    case legacyBootoutFailed(stderr: String)
    /// Upgrade hit two failures in a row: the original failure that
    /// triggered rollback, plus a restore-backup failure. The
    /// operator now has no bundle at the final install path — the
    /// backup is still at `crm-mac.app.backup.<pid>`. The
    /// `originalError` describes what tripped the rollback;
    /// `backupPath` carries the location of the surviving backup so
    /// the operator can manually `mv` it back.
    case upgradeRollbackFailed(originalError: String, restoreError: String, backupPath: String)
    case filesystemFailed(String)
    case codesignFailed(String)
    case keychainFailed(String)
    case unexpected(String)

    public var description: String {
        switch self {
        case .missingHostnameFlag:
            return "--hostname <label> is required. Pick a non-PII label like 'mac-1', 'work-mac', 'home-laptop'."
        case .invalidPairingToken:
            return "pairing token is empty or malformed"
        case .alreadyInstalled:
            return "crm-mac is already installed. Run `crm-mac uninstall --purge` first, or use --upgrade / --register-only."
        case .noExistingInstall:
            return "no existing install found; run `crm-mac install` without --upgrade / --register-only"
        case .pairFailed(let e):
            return "pair failed: \(e)"
        case .ambiguousPair(let underlying):
            return "pair status is ambiguous (the Pi may or may not have committed before the response was lost): \(underlying). " +
                "Recovery: run `crm-admin --list-hosts` on the Pi to see if a host row was created, then " +
                "`crm-admin --revoke-host <id>` if so. Then re-mint a token and re-run `crm-mac install`."
        case .persistFailed(let id, let stage, let underlying):
            return "post-pair persistence failed at \(stage): \(underlying). The Pi paired host_id=\(id.uuidString.lowercased()). " +
                "To recover: (1) run `crm-mac uninstall --purge` to clear partial local state, " +
                "(2) on the Pi run `crm-admin --revoke-host \(id.uuidString.lowercased())`, " +
                "(3) re-mint a token and re-run `crm-mac install`."
        case .agentRegistrationFailed(let m, let approval):
            let suffix = approval
                ? " Approve crm-mac in System Settings → General → Login Items → Allow in Background, then re-run `crm-mac install --register-only`."
                : " Address the underlying issue, then re-run `crm-mac install --register-only`."
            return "agent registration failed: \(m).\(suffix)"
        case .daemonStillRunning(let pid):
            if pid <= 0 {
                // pid 0 / negative pid: the pidfile was malformed or
                // unreadable. Telling the operator to `kill -TERM 0`
                // would target the current process group on POSIX —
                // catastrophic; could kill unrelated shell/session
                // processes. Surface the safer recovery path.
                return "the crm-mac daemon's pidfile was present but unreadable, and the lock did not release within the SIGTERM grace period. " +
                    "Recovery: identify the daemon process with `pgrep -f crm-mac` then `kill -TERM <pid>` (or `kill -9 <pid>` if stuck), then re-run."
            }
            return "the running crm-mac daemon (pid=\(pid)) did not exit within the SIGTERM grace period. " +
                "Recovery: `kill -TERM \(pid)` (or `kill -9 \(pid)` if it's stuck), then re-run."
        case .legacyBootoutFailed(let stderr):
            return "legacy launchd registration could not be bootouted: \(stderr). " +
                "Recovery: `launchctl bootout gui/$(id -u)/\(Daemon.label)`, then re-run `crm-mac install --upgrade`."
        case .upgradeRollbackFailed(let original, let restore, let backup):
            return "upgrade rollback failed. Original failure: \(original). Restore backup also failed: \(restore). " +
                "The previous bundle is still at \(backup); to recover, manually `mv \(backup) \(backup.replacingOccurrences(of: ".backup.\(ProcessInfo.processInfo.processIdentifier)", with: ""))` (or the equivalent for your shell), then re-run `crm-mac install --register-only`."
        case .filesystemFailed(let m): return "filesystem: \(m)"
        case .codesignFailed(let m): return "codesign: \(m)"
        case .keychainFailed(let m): return "keychain: \(m)"
        case .unexpected(let m): return "unexpected: \(m)"
        }
    }
}

/// All collaborators an Installer needs.
public struct InstallerDependencies {
    public let paths: LifecyclePaths
    public let filesystem: FilesystemAdapter
    public let executable: ExecutableAdapter
    public let keychain: KeychainStore
    public let agentService: AgentService
    public let processSignaller: ProcessSignaller
    public let bundleAssembler: BundleAssembler
    public let piClientFactory: (URL) -> PiClient
    public let clock: ClockAdapter
    public let logger: LoggerProtocol
    /// One-shot migration source for installs that predate the
    /// file-backed FileAPIKeyStore. When non-nil and `keychain` is
    /// empty, runUpgrade / runRegisterOnly copy from this into
    /// `keychain` and then delete the legacy entry.
    public let legacyKeychain: KeychainStore?
    /// Legacy launchctl runner — used only by the migration path to
    /// bootout the pre-bundle launchd registration. Nil in tests
    /// that don't exercise migration.
    public let legacyLaunchctl: LaunchctlRunner?

    /// Timeout (seconds) for the SIGTERM + pidfile-poll on upgrade
    /// + migration. Default 10s. Override in tests for faster runs.
    public let stopDaemonTimeoutSeconds: TimeInterval
    /// Grace period (seconds) after legacy bootout before the
    /// printService verification probe in the migration's legacy
    /// bootout check.
    public let legacyBootoutGraceSeconds: TimeInterval

    public init(
        paths: LifecyclePaths,
        filesystem: FilesystemAdapter,
        executable: ExecutableAdapter,
        keychain: KeychainStore,
        agentService: AgentService,
        processSignaller: ProcessSignaller,
        bundleAssembler: BundleAssembler,
        piClientFactory: @escaping (URL) -> PiClient,
        clock: ClockAdapter,
        logger: LoggerProtocol,
        legacyKeychain: KeychainStore? = nil,
        legacyLaunchctl: LaunchctlRunner? = nil,
        stopDaemonTimeoutSeconds: TimeInterval = 10,
        legacyBootoutGraceSeconds: TimeInterval = 2
    ) {
        self.paths = paths
        self.filesystem = filesystem
        self.executable = executable
        self.keychain = keychain
        self.agentService = agentService
        self.processSignaller = processSignaller
        self.bundleAssembler = bundleAssembler
        self.piClientFactory = piClientFactory
        self.clock = clock
        self.logger = logger
        self.legacyKeychain = legacyKeychain
        self.legacyLaunchctl = legacyLaunchctl
        self.stopDaemonTimeoutSeconds = stopDaemonTimeoutSeconds
        self.legacyBootoutGraceSeconds = legacyBootoutGraceSeconds
    }
}

public struct Installer {
    public let deps: InstallerDependencies

    public init(_ deps: InstallerDependencies) {
        self.deps = deps
    }

    /// Placeholder string the build-time shell script writes into the
    /// embedded LaunchAgents plist for the binary path. The installer
    /// substitutes this with the real install-time bundle path before
    /// SMAppService.register reads it.
    public static let installPrefixPlaceholder = "__INSTALL_PREFIX__"

    public func run(_ request: InstallRequest) async throws -> InstallSummary {
        if request.upgrade && request.registerOnly {
            throw InstallError.unexpected("--upgrade and --register-only are mutually exclusive")
        }
        if request.upgrade {
            return try await runUpgrade()
        }
        if request.registerOnly {
            return try await runRegisterOnly()
        }
        return try await runFreshInstall(request)
    }

    // MARK: - fresh install

    private func runFreshInstall(_ request: InstallRequest) async throws -> InstallSummary {
        if request.hostname.isEmpty {
            throw InstallError.missingHostnameFlag
        }
        if request.pairingToken.isEmpty {
            throw InstallError.invalidPairingToken
        }
        if existingInstallDetected() {
            throw InstallError.alreadyInstalled
        }

        // Create supporting directories. NO bin/ — bare binary
        // location is migration-only.
        for dir in [deps.paths.configDirPath, deps.paths.logsDirPath] {
            do {
                try deps.filesystem.createDirectory(at: dir)
            } catch {
                throw InstallError.filesystemFailed("mkdir \(dir): \(error)")
            }
        }

        // Assemble the new bundle at a tmp path.
        let sourcePath: String
        do {
            sourcePath = try deps.executable.currentExecutablePath()
        } catch {
            throw InstallError.unexpected("currentExecutablePath: \(error)")
        }
        let tmpBundle = tmpBundlePath()
        let launchAgentContent = renderPlaceholderLaunchAgentContent()
        let infoPlistContent = try loadInfoPlistContent()
        do {
            try deps.bundleAssembler.assemble(BundleAssemblerInput(
                machoSourcePath: sourcePath,
                bundlePath: tmpBundle,
                launchAgentPlistContent: launchAgentContent,
                infoPlistContent: infoPlistContent,
                codesignIdentifier: Daemon.label))
        } catch let err as ExecutableAdapterError {
            try? deps.filesystem.remove(at: tmpBundle)
            throw InstallError.codesignFailed("\(err)")
        } catch {
            try? deps.filesystem.remove(at: tmpBundle)
            throw InstallError.filesystemFailed("assemble bundle: \(error)")
        }

        // Pair with the Pi.
        let client = deps.piClientFactory(request.piURL)
        let pairResult: PairData
        do {
            pairResult = try await client.pair(
                token: request.pairingToken,
                hostname: request.hostname,
                daemonVersion: Daemon.version,
                protocolVersion: Daemon.protocolVersion)
        } catch let piErr as PiClientError {
            try? deps.filesystem.remove(at: tmpBundle)
            deps.logger.error("pair failed", metadata: ["error": .private(String(describing: piErr))])
            switch piErr {
            case .transport, .serverError:
                throw InstallError.ambiguousPair(underlying: String(describing: piErr))
            default:
                throw InstallError.pairFailed(piErr)
            }
        } catch {
            try? deps.filesystem.remove(at: tmpBundle)
            throw InstallError.ambiguousPair(underlying: String(describing: error))
        }

        // Persist config + api-key + state.
        do {
            let cfg = DaemonConfig(
                piURL: request.piURL,
                hostID: pairResult.hostID,
                hostname: request.hostname,
                installedAt: deps.clock.now())
            try writeConfig(cfg, hostID: pairResult.hostID)
            do {
                try deps.keychain.writeAPIKey(pairResult.apiKey)
            } catch {
                throw InstallError.persistFailed(
                    hostID: pairResult.hostID,
                    stage: "keychain write",
                    underlying: String(describing: error))
            }
            try writeInitialState(hostID: pairResult.hostID)
        } catch let pErr as InstallError {
            try? deps.filesystem.remove(at: tmpBundle)
            throw pErr
        } catch {
            try? deps.filesystem.remove(at: tmpBundle)
            throw InstallError.persistFailed(
                hostID: pairResult.hostID,
                stage: "post-pair persistence",
                underlying: String(describing: error))
        }

        // Atomic-rename tmp bundle to the install path. Destination
        // is absent (preflight refused otherwise) — same-parent,
        // empty-destination rename.
        do {
            try deps.filesystem.rename(
                from: tmpBundle,
                to: deps.paths.bundleAppPath)
        } catch {
            try? deps.filesystem.remove(at: tmpBundle)
            throw InstallError.persistFailed(
                hostID: pairResult.hostID,
                stage: "rename tmp bundle -> install",
                underlying: String(describing: error))
        }

        // Substitute placeholder in the bundle's embedded plist.
        do {
            try substituteInstallPrefixPlaceholder(
                bundlePath: deps.paths.bundleAppPath)
        } catch {
            throw InstallError.filesystemFailed(
                "substitute install prefix: \(error)")
        }

        // Register the agent.
        try registerAgent()

        deps.logger.info("install: complete", metadata: [
            "host_id": .private(pairResult.hostID.uuidString),
            "bundle": .private(deps.paths.bundleAppPath),
        ])
        return InstallSummary(
            hostID: pairResult.hostID,
            cursorEpoch: pairResult.cursorEpoch,
            bundleBinaryPath: deps.paths.bundleBinaryPath,
            bundleAppPath: deps.paths.bundleAppPath)
    }

    // MARK: - upgrade

    private func runUpgrade() async throws -> InstallSummary {
        guard deps.filesystem.fileExists(at: deps.paths.configFilePath) else {
            throw InstallError.noExistingInstall
        }
        try migrateLegacyKeychainIfNeeded()
        guard (try? deps.keychain.readAPIKey()) != nil else {
            throw InstallError.noExistingInstall
        }
        let cfg = try readConfig()

        // If a legacy bare-binary install is present, run the one-shot
        // migration. This stops the legacy daemon + bootouts the
        // legacy launchd registration BEFORE bundle assembly because
        // the labels collide. On success the migration already
        // assembled + registered the new bundle; clean up legacy
        // artifacts and return.
        let migrated = try await runLegacyMigrationIfNeeded()
        if migrated {
            cleanupLegacyArtifactsIfAny()
            deps.logger.info("install: legacy migration complete", metadata: [
                "host_id": .private(cfg.hostID.uuidString),
            ])
            return InstallSummary(
                hostID: cfg.hostID,
                cursorEpoch: 0,
                bundleBinaryPath: deps.paths.bundleBinaryPath,
                bundleAppPath: deps.paths.bundleAppPath)
        }

        // Stop the running daemon (if any). On a fresh
        // SMAppService-managed install the agentService.unregister is
        // the canonical way to ask launchd to stop relaunching the
        // daemon; SIGTERM + pidfile-poll handles the actual process
        // exit because SMAppService.unregister does NOT terminate
        // running processes.
        try await stopRunningDaemon()

        // Backup the existing bundle (if present — fresh upgrade
        // after a `--register-only` failure may have no bundle).
        let backupPath = backupBundlePath()
        let hadExistingBundle = deps.filesystem.fileExists(
            at: deps.paths.bundleAppPath)
        if hadExistingBundle {
            do {
                try deps.filesystem.rename(
                    from: deps.paths.bundleAppPath,
                    to: backupPath)
            } catch {
                throw InstallError.filesystemFailed(
                    "rename existing bundle to backup: \(error)")
            }
        }

        // Assemble new bundle at tmp path.
        let sourcePath: String
        do {
            sourcePath = try deps.executable.currentExecutablePath()
        } catch {
            try rollbackUpgrade(
                originalError: InstallError.unexpected("currentExecutablePath: \(error)"),
                tmpBundle: nil,
                backupPath: backupPath,
                hadExisting: hadExistingBundle,
                newBundleAtFinalPath: false)
        }
        let tmpBundle = tmpBundlePath()
        let launchAgentContent = renderPlaceholderLaunchAgentContent()
        let infoPlistContent: Data
        do {
            infoPlistContent = try loadInfoPlistContent()
        } catch {
            try rollbackUpgrade(
                originalError: InstallError.filesystemFailed("load info.plist content: \(error)"),
                tmpBundle: nil,
                backupPath: backupPath,
                hadExisting: hadExistingBundle,
                newBundleAtFinalPath: false)
        }
        do {
            try deps.bundleAssembler.assemble(BundleAssemblerInput(
                machoSourcePath: sourcePath,
                bundlePath: tmpBundle,
                launchAgentPlistContent: launchAgentContent,
                infoPlistContent: infoPlistContent,
                codesignIdentifier: Daemon.label))
        } catch let err as ExecutableAdapterError {
            try rollbackUpgrade(
                originalError: InstallError.codesignFailed("\(err)"),
                tmpBundle: tmpBundle,
                backupPath: backupPath,
                hadExisting: hadExistingBundle,
                newBundleAtFinalPath: false)
        } catch {
            try rollbackUpgrade(
                originalError: InstallError.filesystemFailed("assemble bundle: \(error)"),
                tmpBundle: tmpBundle,
                backupPath: backupPath,
                hadExisting: hadExistingBundle,
                newBundleAtFinalPath: false)
        }

        // Atomic-rename tmp bundle -> final path.
        do {
            try deps.filesystem.rename(
                from: tmpBundle,
                to: deps.paths.bundleAppPath)
        } catch {
            try rollbackUpgrade(
                originalError: InstallError.filesystemFailed("rename tmp bundle -> install: \(error)"),
                tmpBundle: tmpBundle,
                backupPath: backupPath,
                hadExisting: hadExistingBundle,
                newBundleAtFinalPath: false)
        }

        // Substitute placeholder + register. Rollback on any
        // failure between here and the end of register: remove the
        // new bundle, restore the backup.
        do {
            try substituteInstallPrefixPlaceholder(
                bundlePath: deps.paths.bundleAppPath)
        } catch {
            try rollbackUpgrade(
                originalError: InstallError.filesystemFailed("substitute install prefix: \(error)"),
                tmpBundle: nil,
                backupPath: backupPath,
                hadExisting: hadExistingBundle,
                newBundleAtFinalPath: true)
        }
        do {
            try registerAgent()
        } catch {
            // Register failed — restore the previous install so the
            // operator isn't left with a stopped daemon + new bundle
            // they can't run. Surface the original register error.
            try rollbackUpgrade(
                originalError: error,
                tmpBundle: nil,
                backupPath: backupPath,
                hadExisting: hadExistingBundle,
                newBundleAtFinalPath: true)
        }

        // Cleanup backup (best-effort).
        if hadExistingBundle {
            try? deps.filesystem.remove(at: backupPath)
        }

        // Partial-migration retry: if the operator reruns
        // `--upgrade` (rather than `--register-only`) after a prior
        // legacy-migration register failure, the bundle already
        // existed at this entry so the migration short-circuit
        // didn't fire; the normal upgrade path just succeeded.
        // Legacy artifacts may still be on disk from the earlier
        // partial migration — sweep them now that the new bundle is
        // registered. No-op on a fresh upgrade where no legacy
        // artifacts exist.
        cleanupLegacyArtifactsIfAny()

        deps.logger.info("install: upgrade complete", metadata: [
            "host_id": .private(cfg.hostID.uuidString),
        ])
        return InstallSummary(
            hostID: cfg.hostID,
            cursorEpoch: 0,
            bundleBinaryPath: deps.paths.bundleBinaryPath,
            bundleAppPath: deps.paths.bundleAppPath)
    }

    // MARK: - register-only

    private func runRegisterOnly() async throws -> InstallSummary {
        guard deps.filesystem.fileExists(at: deps.paths.configFilePath) else {
            throw InstallError.noExistingInstall
        }
        try migrateLegacyKeychainIfNeeded()
        guard (try? deps.keychain.readAPIKey()) != nil else {
            throw InstallError.noExistingInstall
        }
        let cfg = try readConfig()

        // If a legacy install is present and no bundle is in place,
        // run the full migration. It assembles + registers; afterwards
        // we sweep the legacy files.
        let migrated = try await runLegacyMigrationIfNeeded()
        if migrated {
            cleanupLegacyArtifactsIfAny()
            return InstallSummary(
                hostID: cfg.hostID,
                cursorEpoch: 0,
                bundleBinaryPath: deps.paths.bundleBinaryPath,
                bundleAppPath: deps.paths.bundleAppPath)
        }

        // Refuse if the bundle is missing — register-only registers
        // an EXISTING bundle; if it's not there, the operator wants
        // --upgrade (which reassembles).
        guard deps.filesystem.fileExists(at: deps.paths.bundleAppPath) else {
            throw InstallError.noExistingInstall
        }

        // Substitute placeholder (idempotent — already-substituted
        // plists are a no-op) + register. ANY thrown error from
        // substitution is a real filesystem failure and propagates.
        try substituteInstallPrefixPlaceholder(
            bundlePath: deps.paths.bundleAppPath)
        try registerAgent()
        // Partial-migration retry case: if a prior --upgrade got
        // through assemble + rename but the register failed, the
        // legacy plist + bare binary are still on disk and step 7
        // never completed. Sweep them now that register succeeded.
        // No-op on a fresh install where no legacy artifacts exist.
        cleanupLegacyArtifactsIfAny()
        return InstallSummary(
            hostID: cfg.hostID,
            cursorEpoch: 0,
            bundleBinaryPath: deps.paths.bundleBinaryPath,
            bundleAppPath: deps.paths.bundleAppPath)
    }

    // MARK: - legacy migration

    /// Detect + migrate a pre-rewrite bare-binary install.
    /// Full migration runs when all three signals hold:
    ///   (1) legacy binary at paths.legacyBinaryPath
    ///   (2) NO bundle at paths.bundleAppPath
    ///   (3) at least one user-data file present (config/state/api-key)
    ///
    /// Steps 1-6 only — stop the legacy daemon, bootout the legacy
    /// launchd registration, assemble the new bundle, register via
    /// SMAppService. Returns true iff a full migration ran (so the
    /// caller knows the rest of upgrade/register-only is unneeded).
    ///
    /// Step 7 (delete legacy plist + bare binary) is run by the
    /// CALLER after a successful register, via
    /// `cleanupLegacyArtifactsIfAny()`. The split matters because a
    /// partial-migration retry needs to attempt register FIRST and
    /// only sweep the legacy files if it succeeds — otherwise the
    /// legacy plist could be deleted while the new bundle is still
    /// unregistered, leaving the operator with no working install
    /// even though the legacy install was viable.
    private func runLegacyMigrationIfNeeded() async throws -> Bool {
        let hasLegacyBinary = deps.filesystem.fileExists(
            at: deps.paths.legacyBinaryPath)
        let hasNewBundle = deps.filesystem.fileExists(
            at: deps.paths.bundleAppPath)
        let hasUserData =
            deps.filesystem.fileExists(at: deps.paths.configFilePath) ||
            deps.filesystem.fileExists(at: deps.paths.stateFilePath) ||
            ((try? deps.keychain.readAPIKey()) != nil)

        guard hasLegacyBinary, !hasNewBundle, hasUserData else {
            return false
        }
        // Stop the legacy daemon.
        try await stopLegacyDaemon()
        // Bootout legacy registration + verify gone.
        try bootoutLegacyAndVerify()
        // Assemble the bundle + register. Cleanup is deferred to the
        // caller's post-register hook so a register failure leaves
        // the legacy files in place for the retry.
        try await assembleAndRegisterNewBundle()
        return true
    }

    /// Best-effort delete of legacy plist + bare binary.
    /// Idempotent and tolerant of "already gone". Called by the
    /// caller AFTER a successful register so a register-failure
    /// retry path can still find the legacy artifacts and have them
    /// participate in the next migration attempt.
    ///
    /// Leaving the legacy plist on disk is not just cosmetic — macOS
    /// can re-load it from `~/Library/LaunchAgents/` on next login
    /// and resurrect the bare-binary service the migration was meant
    /// to retire. The post-register cleanup is what guarantees that
    /// doesn't happen on a successful migration.
    private func cleanupLegacyArtifactsIfAny() {
        var didCleanup = false
        if deps.filesystem.fileExists(at: deps.paths.legacyPlistPath) {
            try? deps.filesystem.remove(at: deps.paths.legacyPlistPath)
            didCleanup = true
        }
        if deps.filesystem.fileExists(at: deps.paths.legacyBinaryPath) {
            try? deps.filesystem.remove(at: deps.paths.legacyBinaryPath)
            didCleanup = true
        }
        if didCleanup {
            deps.logger.info("legacy migration: cleanup complete", metadata: [:])
        }
    }

    /// Stop the legacy daemon: read the legacy pidfile (same path
    /// as the new install — the daemon writes to
    /// <configDir>/daemon.pid) and send SIGTERM; poll for release.
    ///
    /// A present-but-malformed pidfile is NOT treated as "not
    /// running" — the daemon may be alive and we just can't parse its
    /// pid. The flock probe is the authoritative running-or-not
    /// check; we skip SIGTERM but still poll.
    private func stopLegacyDaemon() async throws {
        let pidfilePath = deps.paths.pidfilePath
        guard deps.filesystem.fileExists(at: pidfilePath) else {
            // No pidfile — legacy daemon not running. Benign.
            return
        }
        var pid: pid_t = 0
        if let pidData = try? deps.filesystem.read(from: pidfilePath),
           let raw = String(data: pidData, encoding: .utf8),
           let parsed = pid_t(raw.trimmingCharacters(in: .whitespacesAndNewlines)),
           parsed > 0 {
            pid = parsed
            do {
                try deps.processSignaller.sendSIGTERM(pid: pid)
            } catch {
                deps.logger.warning("legacy migration: SIGTERM failed (continuing)", metadata: [
                    "pid": .public("\(pid)"),
                    "error": .private("\(error)"),
                ])
            }
        } else {
            deps.logger.warning("legacy migration: pidfile present but unreadable/malformed; relying on flock probe", metadata: [
                "path": .private(pidfilePath),
            ])
        }
        let released = await deps.processSignaller.waitForPidfileRelease(
            path: pidfilePath,
            timeoutSeconds: deps.stopDaemonTimeoutSeconds)
        if !released {
            throw InstallError.daemonStillRunning(pid: pid)
        }
    }

    /// Bootout the legacy launchd registration; verify gone via a
    /// `printService` probe after the configured grace period. If
    /// the legacy registration is still loaded the migration fails
    /// with `InstallError.legacyBootoutFailed`.
    private func bootoutLegacyAndVerify() throws {
        guard let legacy = deps.legacyLaunchctl else {
            // No legacy launchctl wired — happens in tests that
            // don't exercise migration. Treat as already-unloaded.
            return
        }
        let bootoutResult = (try? legacy.bootout(label: Daemon.label))
            ?? LaunchctlInvocation(arguments: [], exitCode: -1)
        // Wait then probe. The grace period is held
        // open synchronously here because the rest of the migration
        // (bundle assembly + register) IS the next step; there's no
        // gain to async-sleeping just this section.
        Thread.sleep(forTimeInterval: deps.legacyBootoutGraceSeconds)
        let probe: LaunchctlInvocation
        do {
            probe = try legacy.printService(label: Daemon.label)
        } catch {
            // Probe failed — best-effort treat as not registered.
            return
        }
        if probe.exitCode == 0 {
            throw InstallError.legacyBootoutFailed(stderr: bootoutResult.stderr)
        }
    }

    /// Bundle assembly + atomic-rename + substitute placeholder +
    /// register. Used by the migration path; the fresh install flow
    /// inlines its own version (which also does pair + persist).
    private func assembleAndRegisterNewBundle() async throws {
        let sourcePath: String
        do {
            sourcePath = try deps.executable.currentExecutablePath()
        } catch {
            throw InstallError.unexpected("currentExecutablePath: \(error)")
        }
        // Create supporting dirs (idempotent).
        for dir in [deps.paths.configDirPath, deps.paths.logsDirPath] {
            try? deps.filesystem.createDirectory(at: dir)
        }
        let tmpBundle = tmpBundlePath()
        let launchAgentContent = renderPlaceholderLaunchAgentContent()
        let infoPlistContent = try loadInfoPlistContent()
        do {
            try deps.bundleAssembler.assemble(BundleAssemblerInput(
                machoSourcePath: sourcePath,
                bundlePath: tmpBundle,
                launchAgentPlistContent: launchAgentContent,
                infoPlistContent: infoPlistContent,
                codesignIdentifier: Daemon.label))
        } catch let err as ExecutableAdapterError {
            try? deps.filesystem.remove(at: tmpBundle)
            throw InstallError.codesignFailed("\(err)")
        } catch {
            try? deps.filesystem.remove(at: tmpBundle)
            throw InstallError.filesystemFailed("assemble bundle: \(error)")
        }
        do {
            try deps.filesystem.rename(
                from: tmpBundle,
                to: deps.paths.bundleAppPath)
        } catch {
            try? deps.filesystem.remove(at: tmpBundle)
            throw InstallError.filesystemFailed(
                "rename tmp bundle -> install: \(error)")
        }
        do {
            try substituteInstallPrefixPlaceholder(
                bundlePath: deps.paths.bundleAppPath)
        } catch {
            throw InstallError.filesystemFailed(
                "substitute install prefix: \(error)")
        }
        try registerAgent()
    }

    // MARK: - shared helpers

    /// Wrap `agentService.register()`. The wrapper returns an outcome
    /// enum (registered / alreadyRegistered) — both are success cases
    /// from the workflow's perspective.
    private func registerAgent() throws {
        let outcome: AgentRegisterOutcome
        do {
            outcome = try deps.agentService.register()
        } catch let err as AgentServiceError {
            switch err {
            case .registrationFailed(let m, let approval):
                throw InstallError.agentRegistrationFailed(
                    message: m, requiresApproval: approval)
            case .unregistrationFailed, .bundleNotFound:
                throw InstallError.agentRegistrationFailed(
                    message: "\(err)", requiresApproval: false)
            }
        } catch {
            throw InstallError.agentRegistrationFailed(
                message: "\(error)", requiresApproval: false)
        }
        switch outcome {
        case .registered:
            deps.logger.info("agent registered", metadata: [:])
        case .alreadyRegistered:
            deps.logger.info("agent already registered (no-op)", metadata: [:])
        }
    }

    /// Stop the running daemon: unregister via SMAppService, SIGTERM
    /// the process via ProcessSignaller, poll the pidfile.
    ///
    /// If the pidfile exists but its contents are unparseable, the
    /// flock probe is the authoritative running-or-not check (the
    /// daemon's PidfileLock holds the same lock the probe acquires).
    /// We skip SIGTERM but still poll — and if the poll times out,
    /// surface `daemonStillRunning(pid: 0)` so the upgrade fails
    /// loudly instead of stomping on a still-running daemon.
    private func stopRunningDaemon() async throws {
        do {
            try await deps.agentService.unregister()
        } catch {
            deps.logger.warning(
                "stop daemon: agentService.unregister failed (continuing)",
                metadata: ["error": .private("\(error)")])
        }
        let pidfilePath = deps.paths.pidfilePath
        guard deps.filesystem.fileExists(at: pidfilePath) else {
            return
        }
        var pid: pid_t = 0
        if let pidData = try? deps.filesystem.read(from: pidfilePath),
           let raw = String(data: pidData, encoding: .utf8),
           let parsed = pid_t(raw.trimmingCharacters(in: .whitespacesAndNewlines)),
           parsed > 0 {
            pid = parsed
            do {
                try deps.processSignaller.sendSIGTERM(pid: pid)
            } catch {
                deps.logger.warning(
                    "stop daemon: SIGTERM failed (continuing)",
                    metadata: ["pid": .public("\(pid)"), "error": .private("\(error)")])
            }
        } else {
            deps.logger.warning("stop daemon: pidfile present but unreadable/malformed; relying on flock probe", metadata: [
                "path": .private(pidfilePath),
            ])
        }
        let released = await deps.processSignaller.waitForPidfileRelease(
            path: pidfilePath,
            timeoutSeconds: deps.stopDaemonTimeoutSeconds)
        if !released {
            throw InstallError.daemonStillRunning(pid: pid)
        }
    }

    /// Replace `__INSTALL_PREFIX__` in the bundle's embedded
    /// LaunchAgents plist with the real install-time bundle path.
    /// The bundle path is XML-escaped (via `xmlEscapePlistString`)
    /// before substitution — home directories containing `&`, `<`,
    /// `>`, `"`, or `'` would otherwise produce an invalid plist
    /// even though the renderer claims to handle those characters.
    /// Idempotent: re-running on an already-substituted plist (no
    /// placeholder present) is a no-op success.
    private func substituteInstallPrefixPlaceholder(bundlePath: String) throws {
        let plistFile = "\(bundlePath)/\(BundleAssembler.launchAgentPlistRelativePath)"
        let data: Data
        do {
            data = try deps.filesystem.read(from: plistFile)
        } catch {
            throw InstallError.filesystemFailed(
                "read embedded plist: \(error)")
        }
        let original = String(data: data, encoding: .utf8) ?? ""
        let escapedReplacement = xmlEscapePlistString(bundlePath)
        let substituted = original.replacingOccurrences(
            of: Self.installPrefixPlaceholder,
            with: escapedReplacement)
        if substituted == original {
            // Already substituted — idempotent no-op.
            return
        }
        do {
            try deps.filesystem.write(
                Data(substituted.utf8), to: plistFile)
        } catch {
            throw InstallError.filesystemFailed(
                "write substituted plist: \(error)")
        }
    }

    /// Render the templated LaunchAgent plist content. Uses
    /// `__INSTALL_PREFIX__` for the binary-path component so
    /// `substituteInstallPrefixPlaceholder` can rewrite it post-
    /// rename. Logs + config dir use the real install-time paths
    /// (computed from configDirPath / logsDirPath) — those don't
    /// need substitution.
    private func renderPlaceholderLaunchAgentContent() -> String {
        let plist = LaunchAgentPlist(
            label: Daemon.label,
            binaryPath: "\(Self.installPrefixPlaceholder)/\(BundleAssembler.machoRelativePath)",
            configDirPath: deps.paths.configDirPath,
            stdoutPath: deps.paths.stdoutLogPath,
            stderrPath: deps.paths.stderrLogPath)
        return plist.render()
    }

    /// Load the canonical Info.plist bytes from `Bundle.main`. The
    /// running binary's Mach-O has the file embedded via
    /// `__TEXT,__info_plist` (linker flag in Package.swift). On the
    /// upgrade path the running binary IS already in a bundle, so
    /// `Bundle.main.infoDictionary` resolves from
    /// `Contents/Info.plist`. Either path surfaces the same dict.
    /// Re-serialize to XML for the assembled bundle.
    ///
    /// CAUTION: this method exists so production install + upgrade
    /// can read the canonical Info.plist. Tests use the
    /// `infoPlistContent` injection on `BundleAssemblerInput`
    /// directly via fixture bytes — `Bundle.main.infoDictionary`
    /// from a unit-test process resolves to the test runner's
    /// bundle, NOT crm-mac's.
    private func loadInfoPlistContent() throws -> Data {
        guard let dict = Bundle.main.infoDictionary, !dict.isEmpty else {
            throw InstallError.unexpected(
                "Bundle.main.infoDictionary is empty — the running binary is missing the embedded Info.plist section. " +
                "Check that `make mac-daemon` ran the linker -sectcreate flag.")
        }
        do {
            return try PropertyListSerialization.data(
                fromPropertyList: dict,
                format: .xml,
                options: 0)
        } catch {
            throw InstallError.unexpected("serialize Info.plist: \(error)")
        }
    }

    private func tmpBundlePath() -> String {
        return "\(deps.paths.configDirPath)/crm-mac.app.tmp.\(ProcessInfo.processInfo.processIdentifier)"
    }

    private func backupBundlePath() -> String {
        return "\(deps.paths.configDirPath)/crm-mac.app.backup.\(ProcessInfo.processInfo.processIdentifier)"
    }

    /// Restore the previous bundle from `<crm-mac.app>.backup.<pid>`
    /// during an upgrade rollback. Surfaces the rename failure so the
    /// caller knows the operator now has neither bundle in place
    /// (left only with the backup) — distinguish that from the
    /// happy-rollback case.
    private func restoreBundleBackup(from backupPath: String, hadExisting: Bool) throws {
        guard hadExisting else { return }
        guard deps.filesystem.fileExists(at: backupPath) else { return }
        do {
            try deps.filesystem.rename(
                from: backupPath, to: deps.paths.bundleAppPath)
        } catch {
            deps.logger.error("upgrade rollback: restore backup failed", metadata: [
                "backup": .private(backupPath),
                "error": .private("\(error)"),
            ])
            throw error
        }
    }

    /// Best-effort cleanup-then-restore for the upgrade-path
    /// rollback. Removes the tmp/new bundle (if present), restores
    /// the backup, throws back the original failure on success — or
    /// a composed `upgradeRollbackFailed` if the restore itself fails.
    /// Callers `throw try rollbackUpgrade(...)` — the helper always
    /// terminates the call with a throw.
    private func rollbackUpgrade(
        originalError: Error,
        tmpBundle: String?,
        backupPath: String,
        hadExisting: Bool,
        newBundleAtFinalPath: Bool
    ) throws -> Never {
        if let tmp = tmpBundle, deps.filesystem.fileExists(at: tmp) {
            try? deps.filesystem.remove(at: tmp)
        }
        if newBundleAtFinalPath,
           deps.filesystem.fileExists(at: deps.paths.bundleAppPath) {
            try? deps.filesystem.remove(at: deps.paths.bundleAppPath)
        }
        do {
            try restoreBundleBackup(from: backupPath, hadExisting: hadExisting)
        } catch let restoreError {
            throw InstallError.upgradeRollbackFailed(
                originalError: String(describing: originalError),
                restoreError: String(describing: restoreError),
                backupPath: backupPath)
        }
        // The upgrade path called agentService.unregister() before
        // touching the bundle (to stop the running daemon). After a
        // successful backup restore the old bundle is back on disk
        // but is unregistered, so launchd won't relaunch it. Re-register
        // best-effort so the operator isn't stuck with a stopped
        // service after a rollback. Re-register failures here are
        // logged but not folded into the original error — the caller
        // already has a useful actionable message, and the operator's
        // worst case after a re-register failure is the same as a
        // partial-install register failure (run `crm-mac install
        // --register-only` to retry).
        if hadExisting {
            do {
                _ = try deps.agentService.register()
            } catch {
                deps.logger.warning(
                    "upgrade rollback: re-register of restored backup failed (operator must run `crm-mac install --register-only`)",
                    metadata: ["error": .private("\(error)")])
            }
        }
        throw originalError
    }

    /// One-shot copy of the API key from the legacy macOS Keychain
    /// into the file-backed FileAPIKeyStore. Same logic as the
    /// pre-rewrite Installer.
    private func migrateLegacyKeychainIfNeeded() throws {
        if (try? deps.keychain.readAPIKey()) != nil {
            return
        }
        guard let legacy = deps.legacyKeychain else {
            return
        }
        let value: String
        do {
            value = try legacy.readAPIKey()
        } catch KeychainStoreError.notFound {
            return
        } catch {
            throw InstallError.keychainFailed(
                "read legacy keychain: \(error)")
        }
        do {
            try deps.keychain.writeAPIKey(value)
        } catch {
            throw InstallError.keychainFailed(
                "write api-key file during migration: \(error)")
        }
        do {
            try legacy.deleteAPIKey()
        } catch {
            deps.logger.warning("migration: legacy keychain delete failed (non-fatal)", metadata: [
                "error": .private(String(describing: error)),
            ])
        }
        deps.logger.info("migration: api-key copied from legacy Keychain to file", metadata: [
            "path": .public(deps.paths.apiKeyFilePath),
        ])
    }

    /// Preflight detection — refuses fresh-install when artifacts
    /// from an existing install are detected. Probes:
    ///   - bundle present at bundleAppPath
    ///   - legacy bare binary at legacyBinaryPath
    ///   - config.json present
    ///   - api-key file present
    ///   - agent service reports `.enabled`
    private func existingInstallDetected() -> Bool {
        if deps.filesystem.fileExists(at: deps.paths.bundleAppPath) { return true }
        if deps.filesystem.fileExists(at: deps.paths.legacyBinaryPath) { return true }
        if deps.filesystem.fileExists(at: deps.paths.configFilePath) { return true }
        if (try? deps.keychain.readAPIKey()) != nil { return true }
        if deps.agentService.currentStatus() == .enabled {
            return true
        }
        return false
    }

    private func writeConfig(_ cfg: DaemonConfig, hostID: UUID) throws {
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
        encoder.dateEncodingStrategy = .iso8601
        let data: Data
        do {
            data = try encoder.encode(cfg)
        } catch {
            throw InstallError.persistFailed(
                hostID: hostID,
                stage: "encode config",
                underlying: String(describing: error))
        }
        do {
            try deps.filesystem.write(data, to: deps.paths.configFilePath)
        } catch {
            throw InstallError.persistFailed(
                hostID: hostID,
                stage: "write config",
                underlying: String(describing: error))
        }
    }

    private func readConfig() throws -> DaemonConfig {
        let data: Data
        do {
            data = try deps.filesystem.read(from: deps.paths.configFilePath)
        } catch {
            throw InstallError.filesystemFailed("read config: \(error)")
        }
        let decoder = JSONDecoder()
        decoder.dateDecodingStrategy = .iso8601
        do {
            return try decoder.decode(DaemonConfig.self, from: data)
        } catch {
            throw InstallError.filesystemFailed("decode config: \(error)")
        }
    }

    private func writeInitialState(hostID: UUID) throws {
        let state = DaemonState(
            schemaVersion: DaemonState.currentSchemaVersion,
            hostID: hostID)
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
        encoder.dateEncodingStrategy = .iso8601
        let data: Data
        do {
            data = try encoder.encode(state)
        } catch {
            throw InstallError.persistFailed(
                hostID: hostID,
                stage: "encode state",
                underlying: String(describing: error))
        }
        do {
            try deps.filesystem.write(data, to: deps.paths.stateFilePath)
        } catch {
            throw InstallError.persistFailed(
                hostID: hostID,
                stage: "write state",
                underlying: String(describing: error))
        }
    }
}
