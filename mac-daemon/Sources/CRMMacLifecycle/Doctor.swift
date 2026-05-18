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
    /// Contacts framework adapters used by the icloud_contacts check.
    /// Tests inject stubs.
    public let contactsAuth: ContactsAuthorizationAdapter
    public let containerEnumerator: ContactContainerEnumerator
    /// Used to compute the staleness threshold for icloud_contacts'
    /// last-tick-age check (2× tickInterval).
    public let tickInterval: TimeInterval
    public let clock: ClockAdapter
    public let logger: LoggerProtocol

    public init(
        paths: LifecyclePaths,
        filesystem: FilesystemAdapter,
        keychain: KeychainStore,
        launchctl: LaunchctlRunner,
        piClientFactory: @escaping (URL) -> PiClient,
        contactsAuth: ContactsAuthorizationAdapter,
        containerEnumerator: ContactContainerEnumerator,
        tickInterval: TimeInterval,
        clock: ClockAdapter,
        logger: LoggerProtocol
    ) {
        self.paths = paths
        self.filesystem = filesystem
        self.keychain = keychain
        self.launchctl = launchctl
        self.piClientFactory = piClientFactory
        self.contactsAuth = contactsAuth
        self.containerEnumerator = containerEnumerator
        self.tickInterval = tickInterval
        self.clock = clock
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
        results.append(contentsOf: checkICloudContacts(
            allowlist: configCheck.icloudAllowlist,
            sourceState: configCheck.icloudSourceState))
        return DoctorReport(results: results)
    }

    /// Composite check for the icloud_contacts source:
    ///   1. Contacts permission via the auth adapter.
    ///   2. Allowlist sanity — every configured CNContainer identifier
    ///      resolves against the live enumerator output.
    ///   3. Last-tick age — max(lastScheduledAt, lastPushedAt) older
    ///      than 2× tickInterval is WARN.
    ///
    /// The gcontacts overlap check (spec line 161) is deliberately
    /// deferred to a follow-up PR — the daemon doesn't know which
    /// Pi-side providers are active.
    private func checkICloudContacts(
        allowlist: [String],
        sourceState: SourceState?
    ) -> [CheckResult] {
        var results: [CheckResult] = []

        // 1. Permission.
        let status = deps.contactsAuth.authorizationStatus()
        switch status {
        case .authorized, .limited:
            results.append(CheckResult(
                name: "icloud_contacts.permission",
                status: .pass,
                details: "contacts \(status)"))
        case .denied:
            results.append(CheckResult(
                name: "icloud_contacts.permission",
                status: .fail,
                details: "denied — re-run `crm-mac install --re-request-permission`"))
        case .restricted:
            results.append(CheckResult(
                name: "icloud_contacts.permission",
                status: .fail,
                details: "restricted by MDM / parental controls"))
        case .notDetermined:
            results.append(CheckResult(
                name: "icloud_contacts.permission",
                status: .warn,
                details: "not determined — run `crm-mac install --re-request-permission`"))
        }

        // 2. Allowlist sanity.
        if allowlist.isEmpty {
            results.append(CheckResult(
                name: "icloud_contacts.allowlist",
                status: .warn,
                details: "no containers configured; run `crm-mac configure containers`"))
        } else {
            let visible: [ContainerInfo]
            do {
                visible = try deps.containerEnumerator.listContainers()
            } catch ContactContainerEnumeratorError.notAuthorized {
                visible = []
            } catch {
                results.append(CheckResult(
                    name: "icloud_contacts.allowlist",
                    status: .warn,
                    details: "container enumeration failed: \(error)"))
                return results
            }
            let visibleIDs = Set(visible.map(\.identifier))
            let orphans = allowlist.filter { !visibleIDs.contains($0) }
            if orphans.isEmpty {
                results.append(CheckResult(
                    name: "icloud_contacts.allowlist",
                    status: .pass,
                    details: "\(allowlist.count) container(s) configured; all visible"))
            } else {
                let orphanList = orphans.joined(separator: ",")
                results.append(CheckResult(
                    name: "icloud_contacts.allowlist",
                    status: .warn,
                    details: "\(orphans.count) orphaned identifier(s) (no longer visible): \(orphanList)"))
            }
        }

        // 3. Last-tick age.
        if let src = sourceState {
            let bumps = [src.lastScheduledAt, src.lastPushedAt].compactMap { $0 }
            if let latest = bumps.max() {
                let age = deps.clock.now().timeIntervalSince(latest)
                if age > 2 * deps.tickInterval {
                    results.append(CheckResult(
                        name: "icloud_contacts.last_tick",
                        status: .warn,
                        details: "last activity \(Int(age))s ago (threshold \(Int(2 * deps.tickInterval))s)"))
                } else {
                    results.append(CheckResult(
                        name: "icloud_contacts.last_tick",
                        status: .pass,
                        details: "last activity \(Int(age))s ago"))
                }
            } else {
                results.append(CheckResult(
                    name: "icloud_contacts.last_tick",
                    status: .warn,
                    details: "no tick recorded yet"))
            }
        } else {
            results.append(CheckResult(
                name: "icloud_contacts.last_tick",
                status: .warn,
                details: "no source state present"))
        }

        return results
    }

    /// Aggregate of the config+state check that downstream callers
    /// (Pi reachability + icloud_contacts) need to inspect. Named for
    /// self-documentation where the prior 3-tuple shape required
    /// positional unwrap.
    private struct ConfigAndStateResult {
        let result: CheckResult
        let auth: PiAuth?
        let piURL: URL
        /// Configured iCloud allowlist from `sources.icloud_contacts.containers`.
        /// Empty when the config file lacks the key OR the operator
        /// hasn't picked any containers yet.
        let icloudAllowlist: [String]
        /// Per-source state for icloud_contacts. Nil when the source
        /// has never ticked.
        let icloudSourceState: SourceState?
    }

    private func checkKeychain() -> CheckResult {
        do {
            _ = try deps.keychain.readAPIKey()
            return CheckResult(name: "api-key", status: .pass, details: "present")
        } catch let e as KeychainStoreError where e == .notFound {
            return CheckResult(name: "api-key", status: .fail, details: "not present")
        } catch {
            return CheckResult(name: "api-key", status: .fail, details: String(describing: error))
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
                piURL: placeholderURL,
                icloudAllowlist: [],
                icloudSourceState: nil)
        }
        let configData: Data
        do {
            configData = try deps.filesystem.read(from: deps.paths.configFilePath)
        } catch {
            return ConfigAndStateResult(
                result: CheckResult(name: "config_state", status: .fail, details: "read config.json: \(error)"),
                auth: nil,
                piURL: placeholderURL,
                icloudAllowlist: [],
                icloudSourceState: nil)
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
                piURL: placeholderURL,
                icloudAllowlist: [],
                icloudSourceState: nil)
        }
        let icloudAllowlist = cfg.sources?.icloudContacts?.containers ?? []

        guard deps.filesystem.fileExists(at: deps.paths.stateFilePath) else {
            return ConfigAndStateResult(
                result: CheckResult(name: "config_state", status: .fail, details: "state.json missing"),
                auth: nil,
                piURL: cfg.piURL,
                icloudAllowlist: icloudAllowlist,
                icloudSourceState: nil)
        }
        let stateData: Data
        do {
            stateData = try deps.filesystem.read(from: deps.paths.stateFilePath)
        } catch {
            return ConfigAndStateResult(
                result: CheckResult(name: "config_state", status: .fail, details: "read state.json: \(error)"),
                auth: nil,
                piURL: cfg.piURL,
                icloudAllowlist: icloudAllowlist,
                icloudSourceState: nil)
        }
        let state: DaemonState
        do {
            state = try decoder.decode(DaemonState.self, from: stateData)
        } catch {
            return ConfigAndStateResult(
                result: CheckResult(name: "config_state", status: .fail, details: "decode state.json: \(error)"),
                auth: nil,
                piURL: cfg.piURL,
                icloudAllowlist: icloudAllowlist,
                icloudSourceState: nil)
        }
        if state.schemaVersion != DaemonState.currentSchemaVersion {
            return ConfigAndStateResult(
                result: CheckResult(
                    name: "config_state",
                    status: .fail,
                    details: "state schemaVersion=\(state.schemaVersion); expected \(DaemonState.currentSchemaVersion)"),
                auth: nil,
                piURL: cfg.piURL,
                icloudAllowlist: icloudAllowlist,
                icloudSourceState: state.sources["icloud_contacts"])
        }
        let icloudSourceState = state.sources["icloud_contacts"]
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
                piURL: cfg.piURL,
                icloudAllowlist: icloudAllowlist,
                icloudSourceState: icloudSourceState)
        }
        return ConfigAndStateResult(
            result: CheckResult(
                name: "config_state",
                status: .pass,
                details: "host=\(cfg.hostname) schemaVersion=\(state.schemaVersion)"),
            auth: PiAuth(hostID: cfg.hostID, apiKey: apiKey),
            piURL: cfg.piURL,
            icloudAllowlist: icloudAllowlist,
            icloudSourceState: icloudSourceState)
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
