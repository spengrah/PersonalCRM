// Installer implements `crm-mac install`, `--upgrade`, and
// `--register-only` per plan D9 + D9.5.
//
// The fresh-install sequence (D9 steps 1-11):
//   1. Validate inputs.
//   2. (D9.5 preflight) refuse if existing install detected unless
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
    case pairFailed(PiClientError)
    case persistFailed(String)
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
        case .persistFailed(let m):
            return "persist failed: \(m). To recover: (1) run `crm-mac uninstall --purge`, (2) on the Pi run `crm-admin --revoke-host <id>`, (3) re-mint a token and re-install."
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

        // Step 3: create supporting directories.
        for dir in [deps.paths.configDirPath, deps.paths.binDirPath, deps.paths.logsDirPath, deps.paths.launchAgentsDirPath] {
            do {
                try deps.filesystem.createDirectory(at: dir)
            } catch {
                throw InstallError.filesystemFailed("mkdir \(dir): \(error)")
            }
        }

        // Step 4: stage the running executable to a temp path.
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

        // Step 5: pair with the Pi.
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
            deps.logger.error("pair failed", metadata: ["error": .public(String(describing: piErr))])
            throw InstallError.pairFailed(piErr)
        } catch {
            try? deps.filesystem.remove(at: tempPath)
            throw InstallError.pairFailed(.transport(underlying: String(describing: error)))
        }

        // Step 6: persist config + Keychain + state. On ANY failure
        // unlink the temp binary and surface persistFailed so the
        // operator's recovery path is clear.
        do {
            let cfg = DaemonConfig(
                piURL: request.piURL,
                hostID: pairResult.hostID,
                hostname: request.hostname,
                installedAt: deps.clock.now())
            try writeConfig(cfg)
            try deps.keychain.writeAPIKey(pairResult.apiKey)
            try writeInitialState(hostID: pairResult.hostID)
        } catch let pErr as InstallError {
            try? deps.filesystem.remove(at: tempPath)
            throw pErr
        } catch {
            try? deps.filesystem.remove(at: tempPath)
            throw InstallError.persistFailed(String(describing: error))
        }

        // Step 7: atomic-rename the temp binary into place.
        do {
            try deps.filesystem.rename(from: tempPath, to: deps.paths.binaryPath)
        } catch {
            try? deps.filesystem.remove(at: tempPath)
            throw InstallError.persistFailed("rename temp -> install: \(error)")
        }

        // Step 8: write plist + launchctl bootstrap.
        try writePlist()
        try bootstrapAgent()

        deps.logger.info("install: complete", metadata: [
            "host_id": .private(pairResult.hostID.uuidString),
            "binary": .public(deps.paths.binaryPath),
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
    /// artifacts exist. Used by D9.5 to refuse fresh-install on top
    /// of an existing one.
    private func existingInstallDetected() -> Bool {
        if deps.filesystem.fileExists(at: deps.paths.binaryPath) { return true }
        if deps.filesystem.fileExists(at: deps.paths.configFilePath) { return true }
        if (try? deps.keychain.readAPIKey()) != nil { return true }
        if let inv = try? deps.launchctl.printService(label: Daemon.label), inv.exitCode == 0 {
            return true
        }
        return false
    }

    private func writeConfig(_ cfg: DaemonConfig) throws {
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
        encoder.dateEncodingStrategy = .iso8601
        let data: Data
        do {
            data = try encoder.encode(cfg)
        } catch {
            throw InstallError.persistFailed("encode config: \(error)")
        }
        do {
            try deps.filesystem.write(data, to: deps.paths.configFilePath)
        } catch {
            throw InstallError.persistFailed("write config: \(error)")
        }
    }

    private func readConfig() throws -> DaemonConfig {
        let data: Data
        do {
            data = try deps.filesystem.read(from: deps.paths.configFilePath)
        } catch {
            throw InstallError.persistFailed("read config: \(error)")
        }
        let decoder = JSONDecoder()
        decoder.dateDecodingStrategy = .iso8601
        do {
            return try decoder.decode(DaemonConfig.self, from: data)
        } catch {
            throw InstallError.persistFailed("decode config: \(error)")
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
            throw InstallError.persistFailed("encode state: \(error)")
        }
        do {
            try deps.filesystem.write(data, to: deps.paths.stateFilePath)
        } catch {
            throw InstallError.persistFailed("write state: \(error)")
        }
    }

    private func writePlist() throws {
        let plist = """
        <?xml version="1.0" encoding="UTF-8"?>
        <!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
        <plist version="1.0">
        <dict>
            <key>Label</key>
            <string>\(Daemon.label)</string>
            <key>ProgramArguments</key>
            <array>
                <string>\(deps.paths.binaryPath)</string>
                <string>daemon</string>
            </array>
            <key>RunAtLoad</key>
            <true/>
            <key>KeepAlive</key>
            <dict>
                <key>Crashed</key>
                <true/>
            </dict>
            <key>ProcessType</key>
            <string>Background</string>
            <key>StandardOutPath</key>
            <string>\(deps.paths.stdoutLogPath)</string>
            <key>StandardErrorPath</key>
            <string>\(deps.paths.stderrLogPath)</string>
            <key>EnvironmentVariables</key>
            <dict>
                <key>CRM_MAC_CONFIG_DIR</key>
                <string>\(deps.paths.configDirPath)</string>
            </dict>
        </dict>
        </plist>

        """
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
