// Installer implements `crm-mac install`, `--upgrade`, and
// `--register-only`.
//
// The fresh-install sequence:
//   1. Validate inputs.
//   2. Preflight: refuse if existing install detected unless
//      --upgrade or --register-only set.
//   3. mkdir -p config/bin/logs directories.
//   4. Stage running binary to a temp path; chmod +x; codesign.
//   5. POST /api/v1/host on the Pi.
//   6. Persist config.json, Keychain api-key, state.json.
//   7. Atomic-rename temp binary to install path.
//   8. Write plist + launchctl bootstrap.
//
// On any failure inside steps 5-6, the temp binary is unlinked. On
// any failure inside step 7 (rare — same-fs rename), the temp binary
// is unlinked and operator runs uninstall --purge. On step 8 failure,
// binary is in place; operator runs `crm-mac install --register-only`.
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
    public let binaryPath: String
    public let plistPath: String

    public init(hostID: UUID, cursorEpoch: Int64, binaryPath: String, plistPath: String) {
        self.hostID = hostID
        self.cursorEpoch = cursorEpoch
        self.binaryPath = binaryPath
        self.plistPath = plistPath
    }
}

public enum InstallError: Error, CustomStringConvertible {
    case missingHostnameFlag
    case invalidPairingToken
    case alreadyInstalled
    case noExistingInstall
    /// Pair surfaced a typed Pi error (410/409/4xx). Recovery is the
    /// typed-error specific path (mint a new token, revoke the
    /// existing host, etc.).
    case pairFailed(PiClientError)
    /// Pair failed at the transport layer or with a 5xx — the Pi may
    /// or may not have committed the host row. Operator must run
    /// `crm-admin --list-hosts` to disambiguate.
    case ambiguousPair(underlying: String)
    /// Post-pair local persistence (config, Keychain, state, or
    /// atomic rename) failed AFTER the Pi committed. The host ID is
    /// carried so the recovery message names it.
    case persistFailed(hostID: UUID, stage: String, underlying: String)
    case launchctlFailed(exitCode: Int32, stderr: String)
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
        case .launchctlFailed(let code, let stderr):
            return "launchctl bootstrap failed (exit \(code)): \(stderr). Binary is installed; run `crm-mac install --register-only` after fixing the underlying issue (System Settings -> Privacy & Security)."
        case .filesystemFailed(let m): return "filesystem: \(m)"
        case .codesignFailed(let m): return "codesign: \(m)"
        case .keychainFailed(let m): return "keychain: \(m)"
        case .unexpected(let m): return "unexpected: \(m)"
        }
    }
}

/// All collaborators an Installer needs. Test code instantiates with
/// fakes; production wires through CRMMacSystem implementations.
public struct InstallerDependencies {
    public let paths: LifecyclePaths
    public let filesystem: FilesystemAdapter
    public let executable: ExecutableAdapter
    public let keychain: KeychainStore
    public let launchctl: LaunchctlRunner
    public let piClientFactory: (URL) -> PiClient
    public let clock: ClockAdapter
    public let logger: LoggerProtocol

    public init(
        paths: LifecyclePaths,
        filesystem: FilesystemAdapter,
        executable: ExecutableAdapter,
        keychain: KeychainStore,
        launchctl: LaunchctlRunner,
        piClientFactory: @escaping (URL) -> PiClient,
        clock: ClockAdapter,
        logger: LoggerProtocol
    ) {
        self.paths = paths
        self.filesystem = filesystem
        self.executable = executable
        self.keychain = keychain
        self.launchctl = launchctl
        self.piClientFactory = piClientFactory
        self.clock = clock
        self.logger = logger
    }
}

public struct Installer {
    public let deps: InstallerDependencies

    public init(_ deps: InstallerDependencies) {
        self.deps = deps
    }

    public func run(_ request: InstallRequest) async throws -> InstallSummary {
        if request.upgrade && request.registerOnly {
            throw InstallError.unexpected("--upgrade and --register-only are mutually exclusive")
        }
        if request.upgrade {
            return try await runUpgrade()
        }
        if request.registerOnly {
            return try runRegisterOnly()
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

        // Create supporting directories.
        for dir in [deps.paths.configDirPath, deps.paths.binDirPath, deps.paths.logsDirPath, deps.paths.launchAgentsDirPath] {
            do {
                try deps.filesystem.createDirectory(at: dir)
            } catch {
                throw InstallError.filesystemFailed("mkdir \(dir): \(error)")
            }
        }

        // Stage the running executable to a temp path.
        let sourcePath: String
        do {
            sourcePath = try deps.executable.currentExecutablePath()
        } catch {
            throw InstallError.unexpected("currentExecutablePath: \(error)")
        }
        let tempPath = "\(deps.paths.binDirPath)/crm-mac.tmp.\(ProcessInfo.processInfo.processIdentifier)"
        do {
            try deps.filesystem.copy(from: sourcePath, to: tempPath)
            try deps.filesystem.makeExecutable(at: tempPath)
        } catch {
            try? deps.filesystem.remove(at: tempPath)
            throw InstallError.filesystemFailed("stage temp binary: \(error)")
        }
        do {
            try deps.executable.adhocCodesign(path: tempPath)
        } catch {
            try? deps.filesystem.remove(at: tempPath)
            throw InstallError.codesignFailed("\(error)")
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
            try? deps.filesystem.remove(at: tempPath)
            deps.logger.error("pair failed", metadata: ["error": .private(String(describing: piErr))])
            // Transport / 5xx failures are ambiguous — the Pi may
            // have committed before the response was lost. Surface a
            // distinct error so the operator gets the list-hosts
            // recovery path.
            switch piErr {
            case .transport, .serverError:
                throw InstallError.ambiguousPair(underlying: String(describing: piErr))
            default:
                throw InstallError.pairFailed(piErr)
            }
        } catch {
            try? deps.filesystem.remove(at: tempPath)
            throw InstallError.ambiguousPair(underlying: String(describing: error))
        }

        // Persist config + Keychain + state. On ANY failure unlink the
        // temp binary and surface persistFailed (with the paired host
        // ID) so the operator's recovery path is clear.
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
            try? deps.filesystem.remove(at: tempPath)
            throw pErr
        } catch {
            try? deps.filesystem.remove(at: tempPath)
            throw InstallError.persistFailed(
                hostID: pairResult.hostID,
                stage: "post-pair persistence",
                underlying: String(describing: error))
        }

        // Atomic-rename the temp binary into place. Final step
        // before launchd registration; any failure beyond this point
        // leaves the binary installed and recovery is
        // `crm-mac install --register-only`.
        do {
            try deps.filesystem.rename(from: tempPath, to: deps.paths.binaryPath)
        } catch {
            try? deps.filesystem.remove(at: tempPath)
            throw InstallError.persistFailed(
                hostID: pairResult.hostID,
                stage: "rename temp -> install",
                underlying: String(describing: error))
        }

        // Write plist + launchctl bootstrap. Both happen AFTER the
        // binary + config + Keychain + state are durably installed,
        // so any failure here leaves the install in a state where
        // `crm-mac install --register-only` is the recovery path.
        do {
            try writePlist()
        } catch let fsErr {
            throw InstallError.launchctlFailed(
                exitCode: -1,
                stderr: "write plist: \(fsErr)")
        }
        try bootstrapAgent()

        deps.logger.info("install: complete", metadata: [
            "host_id": .private(pairResult.hostID.uuidString),
            "binary": .private(deps.paths.binaryPath),
        ])
        return InstallSummary(
            hostID: pairResult.hostID,
            cursorEpoch: pairResult.cursorEpoch,
            binaryPath: deps.paths.binaryPath,
            plistPath: deps.paths.plistPath)
    }

    // MARK: - upgrade

    private func runUpgrade() async throws -> InstallSummary {
        guard deps.filesystem.fileExists(at: deps.paths.configFilePath) else {
            throw InstallError.noExistingInstall
        }
        guard (try? deps.keychain.readAPIKey()) != nil else {
            throw InstallError.noExistingInstall
        }
        let cfg = try readConfig()

        // Bootout the running daemon so we can replace the binary.
        // Failure here is tolerable — bootout returns non-zero when
        // service isn't loaded, which is fine for upgrade.
        _ = try? deps.launchctl.bootout(label: Daemon.label)

        // Stage + atomic-rename over the install path.
        let sourcePath: String
        do {
            sourcePath = try deps.executable.currentExecutablePath()
        } catch {
            throw InstallError.unexpected("currentExecutablePath: \(error)")
        }
        let tempPath = "\(deps.paths.binDirPath)/crm-mac.tmp.\(ProcessInfo.processInfo.processIdentifier)"
        do {
            try deps.filesystem.copy(from: sourcePath, to: tempPath)
            try deps.filesystem.makeExecutable(at: tempPath)
            try deps.executable.adhocCodesign(path: tempPath)
            try deps.filesystem.rename(from: tempPath, to: deps.paths.binaryPath)
        } catch {
            try? deps.filesystem.remove(at: tempPath)
            throw InstallError.filesystemFailed("upgrade rename: \(error)")
        }
        try writePlist()
        try bootstrapAgent()

        deps.logger.info("install: upgrade complete", metadata: [
            "host_id": .private(cfg.hostID.uuidString),
        ])
        return InstallSummary(
            hostID: cfg.hostID,
            cursorEpoch: 0,  // unknown without a heartbeat round-trip
            binaryPath: deps.paths.binaryPath,
            plistPath: deps.paths.plistPath)
    }

    // MARK: - register-only

    private func runRegisterOnly() throws -> InstallSummary {
        guard deps.filesystem.fileExists(at: deps.paths.configFilePath) else {
            throw InstallError.noExistingInstall
        }
        guard (try? deps.keychain.readAPIKey()) != nil else {
            throw InstallError.noExistingInstall
        }
        let cfg = try readConfig()

        try writePlist()
        try bootstrapAgent()
        return InstallSummary(
            hostID: cfg.hostID,
            cursorEpoch: 0,
            binaryPath: deps.paths.binaryPath,
            plistPath: deps.paths.plistPath)
    }

    // MARK: - shared helpers

    /// Preflight detection — returns true when any of the install
    /// artifacts exist, to refuse fresh-install on top of an existing
    /// one.
    private func existingInstallDetected() -> Bool {
        if deps.filesystem.fileExists(at: deps.paths.binaryPath) { return true }
        if deps.filesystem.fileExists(at: deps.paths.configFilePath) { return true }
        if (try? deps.keychain.readAPIKey()) != nil { return true }
        // Surface launchctl spawn failures (vs non-zero exit) so an
        // unexpected runtime error can't silently mask a leftover
        // registration. Non-zero exit (service unknown) is fine — that's
        // the normal "no install" path.
        do {
            let inv = try deps.launchctl.printService(label: Daemon.label)
            if inv.exitCode == 0 {
                return true
            }
        } catch {
            deps.logger.warning("install: launchctl probe failed; assuming no existing service", metadata: [
                "error": .private(String(describing: error)),
            ])
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

    private func writePlist() throws {
        let plist = LaunchAgentPlist(
            label: Daemon.label,
            binaryPath: deps.paths.binaryPath,
            configDirPath: deps.paths.configDirPath,
            stdoutPath: deps.paths.stdoutLogPath,
            stderrPath: deps.paths.stderrLogPath).render()
        do {
            try deps.filesystem.write(Data(plist.utf8), to: deps.paths.plistPath)
        } catch {
            throw InstallError.filesystemFailed("write plist: \(error)")
        }
    }

    private func bootstrapAgent() throws {
        let inv: LaunchctlInvocation
        do {
            inv = try deps.launchctl.bootstrap(plistPath: deps.paths.plistPath)
        } catch {
            throw InstallError.launchctlFailed(exitCode: -1, stderr: String(describing: error))
        }
        if inv.exitCode != 0 {
            throw InstallError.launchctlFailed(exitCode: inv.exitCode, stderr: inv.stderr)
        }
    }
}
