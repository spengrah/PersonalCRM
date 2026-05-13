// Doctor implements `crm-mac doctor`.
//
// Four checks:
//   1. Keychain access (read api-key)
//   2. Launchd agent presence (`launchctl print` exit 0)
//   3. Pi reachability (GET /known-identifiers; 200=PASS, 401=FAIL,
//      5xx=WARN, network=WARN)
//   4. Config + state file presence (both parse; state has correct
//      schemaVersion)
//
// Output: array of CheckResult { name, status, details }; exit code
// equals the number of FAIL entries.
import Foundation
import CRMMacCore
import CRMMacPiClient

public enum CheckStatus: String, Equatable {
    case pass = "PASS"
    case warn = "WARN"
    case fail = "FAIL"
}

public struct CheckResult: Equatable {
    public let name: String
    public let status: CheckStatus
    public let details: String

    public init(name: String, status: CheckStatus, details: String) {
        self.name = name
        self.status = status
        self.details = details
    }
}

public struct DoctorReport: Equatable {
    public let results: [CheckResult]

    public init(results: [CheckResult]) {
        self.results = results
    }

    public var failCount: Int {
        results.filter { $0.status == .fail }.count
    }
}

public struct DoctorDependencies {
    public let paths: LifecyclePaths
    public let filesystem: FilesystemAdapter
    public let keychain: KeychainStore
    public let launchctl: LaunchctlRunner
    public let piClientFactory: (URL) -> PiClient
    public let logger: LoggerProtocol

    public init(
        paths: LifecyclePaths,
        filesystem: FilesystemAdapter,
        keychain: KeychainStore,
        launchctl: LaunchctlRunner,
        piClientFactory: @escaping (URL) -> PiClient,
        logger: LoggerProtocol
    ) {
        self.paths = paths
        self.filesystem = filesystem
        self.keychain = keychain
        self.launchctl = launchctl
        self.piClientFactory = piClientFactory
        self.logger = logger
    }
}

public struct Doctor {
    public let deps: DoctorDependencies
    public init(_ deps: DoctorDependencies) {
        self.deps = deps
    }

    public func run() async -> DoctorReport {
        var results: [CheckResult] = []
        results.append(checkKeychain())
        results.append(checkLaunchctl())

        let configCheck = checkConfigAndState()
        results.append(configCheck.result)
        // The Pi reachability probe needs the host ID + api key, both
        // of which depend on config + Keychain. If those failed, surface
        // a single derived FAIL for reachability without making the
        // network call.
        if let auth = configCheck.auth {
            let reach = await checkPiReachability(auth: auth, piURL: configCheck.piURL)
            results.append(reach)
        } else {
            results.append(CheckResult(
                name: "pi_reachability",
                status: .fail,
                details: "skipped — config or keychain unavailable"))
        }
        return DoctorReport(results: results)
    }

    /// Aggregate of the config+state check that downstream callers
    /// (Pi reachability) need to inspect. Named for self-documentation
    /// where the prior 3-tuple shape required positional unwrap.
    private struct ConfigAndStateResult {
        let result: CheckResult
        let auth: PiAuth?
        let piURL: URL
    }

    private func checkKeychain() -> CheckResult {
        do {
            _ = try deps.keychain.readAPIKey()
            return CheckResult(name: "keychain", status: .pass, details: "api-key present")
        } catch let e as KeychainStoreError where e == .notFound {
            return CheckResult(name: "keychain", status: .fail, details: "api-key not present")
        } catch {
            return CheckResult(name: "keychain", status: .fail, details: String(describing: error))
        }
    }

    private func checkLaunchctl() -> CheckResult {
        do {
            let inv = try deps.launchctl.printService(label: Daemon.label)
            if inv.exitCode == 0 {
                return CheckResult(name: "launchctl", status: .pass, details: "service registered")
            }
            return CheckResult(
                name: "launchctl",
                status: .warn,
                details: "service not registered (exit \(inv.exitCode))")
        } catch {
            return CheckResult(
                name: "launchctl",
                status: .warn,
                details: "launchctl invocation failed: \(error)")
        }
    }

    private func checkConfigAndState() -> ConfigAndStateResult {
        let placeholderURL = URL(string: "https://localhost")!
        guard deps.filesystem.fileExists(at: deps.paths.configFilePath) else {
            return ConfigAndStateResult(
                result: CheckResult(name: "config_state", status: .fail, details: "config.json missing"),
                auth: nil,
                piURL: placeholderURL)
        }
        let configData: Data
        do {
            configData = try deps.filesystem.read(from: deps.paths.configFilePath)
        } catch {
            return ConfigAndStateResult(
                result: CheckResult(name: "config_state", status: .fail, details: "read config.json: \(error)"),
                auth: nil,
                piURL: placeholderURL)
        }
        let decoder = JSONDecoder()
        decoder.dateDecodingStrategy = .iso8601
        let cfg: DaemonConfig
        do {
            cfg = try decoder.decode(DaemonConfig.self, from: configData)
        } catch {
            return ConfigAndStateResult(
                result: CheckResult(name: "config_state", status: .fail, details: "decode config.json: \(error)"),
                auth: nil,
                piURL: placeholderURL)
        }

        guard deps.filesystem.fileExists(at: deps.paths.stateFilePath) else {
            return ConfigAndStateResult(
                result: CheckResult(name: "config_state", status: .fail, details: "state.json missing"),
                auth: nil,
                piURL: cfg.piURL)
        }
        let stateData: Data
        do {
            stateData = try deps.filesystem.read(from: deps.paths.stateFilePath)
        } catch {
            return ConfigAndStateResult(
                result: CheckResult(name: "config_state", status: .fail, details: "read state.json: \(error)"),
                auth: nil,
                piURL: cfg.piURL)
        }
        let state: DaemonState
        do {
            state = try decoder.decode(DaemonState.self, from: stateData)
        } catch {
            return ConfigAndStateResult(
                result: CheckResult(name: "config_state", status: .fail, details: "decode state.json: \(error)"),
                auth: nil,
                piURL: cfg.piURL)
        }
        if state.schemaVersion != DaemonState.currentSchemaVersion {
            return ConfigAndStateResult(
                result: CheckResult(
                    name: "config_state",
                    status: .fail,
                    details: "state schemaVersion=\(state.schemaVersion); expected \(DaemonState.currentSchemaVersion)"),
                auth: nil,
                piURL: cfg.piURL)
        }
        let apiKey: String
        do {
            apiKey = try deps.keychain.readAPIKey()
        } catch {
            return ConfigAndStateResult(
                result: CheckResult(
                    name: "config_state",
                    status: .pass,
                    details: "config + state OK (keychain probed separately)"),
                auth: nil,
                piURL: cfg.piURL)
        }
        return ConfigAndStateResult(
            result: CheckResult(
                name: "config_state",
                status: .pass,
                details: "host=\(cfg.hostname) schemaVersion=\(state.schemaVersion)"),
            auth: PiAuth(hostID: cfg.hostID, apiKey: apiKey),
            piURL: cfg.piURL)
    }

    private func checkPiReachability(auth: PiAuth, piURL: URL) async -> CheckResult {
        let client = deps.piClientFactory(piURL)
        do {
            let result = try await client.knownIdentifiers(auth: auth)
            return CheckResult(
                name: "pi_reachability",
                status: .pass,
                details: "phones=\(result.phones.count) emails=\(result.emails.count)")
        } catch let pi as PiClientError {
            switch pi {
            case .authenticationRevoked:
                return CheckResult(name: "pi_reachability", status: .fail, details: "401 auth revoked")
            case .serverError(let status, _):
                return CheckResult(name: "pi_reachability", status: .warn, details: "5xx \(status)")
            default:
                return CheckResult(name: "pi_reachability", status: .warn, details: String(describing: pi))
            }
        } catch {
            return CheckResult(name: "pi_reachability", status: .warn, details: String(describing: error))
        }
    }
}
