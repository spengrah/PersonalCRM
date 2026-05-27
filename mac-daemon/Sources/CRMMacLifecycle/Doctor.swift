// Doctor implements `crm-mac doctor`.
//
// Four core checks:
//   1. api-key file (read api-key file)
//   2. Agent service registration (AgentService.currentStatus)
//   3. Pi reachability (GET /known-identifiers; 200=PASS, 401=FAIL,
//      5xx=WARN, network=WARN)
//   4. Config + state file presence (both parse; state has correct
//      schemaVersion)
// Plus three icloud_contacts checks (permission, allowlist, last-tick).
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
    public let agentService: AgentService
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
        agentService: AgentService,
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
        self.agentService = agentService
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
        results.append(checkAgentService())

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
        results.append(contentsOf: checkAnarlog(
            config: configCheck.anarlogConfig,
            humansSource: configCheck.anarlogHumansSourceState,
            sessionsSource: configCheck.anarlogSessionsSourceState))
        return DoctorReport(results: results)
    }

    /// Composite check for the anarlog reader sources. Each enable
    /// flag flips independently between not_configured + active. Path
    /// + subdir + permission probes happen once and surface as
    /// shared `anarlog:*` results so the operator sees one row per
    /// failure instead of two near-identical rows.
    private func checkAnarlog(
        config: AnarlogConfig?,
        humansSource: SourceState?,
        sessionsSource: SourceState?
    ) -> [CheckResult] {
        var results: [CheckResult] = []

        // Per-source enable flag — emit one info/warn row per source.
        guard let cfg = config else {
            results.append(CheckResult(
                name: "anarlog_humans",
                status: .warn,
                details: "not_configured (run `crm-mac configure anarlog --path <abs> --enable both`)"))
            results.append(CheckResult(
                name: "anarlog_sessions",
                status: .warn,
                details: "not_configured (run `crm-mac configure anarlog --path <abs> --enable both`)"))
            return results
        }
        results.append(CheckResult(
            name: "anarlog_humans",
            status: cfg.humansEnabled ? .pass : .warn,
            details: cfg.humansEnabled
                ? "enabled (root=\(cfg.rootPath))"
                : "not_configured (disabled)"))
        results.append(CheckResult(
            name: "anarlog_sessions",
            status: cfg.sessionsEnabled ? .pass : .warn,
            details: cfg.sessionsEnabled
                ? "enabled (root=\(cfg.rootPath))"
                : "not_configured (disabled)"))

        // Only run filesystem probes if at least one source is enabled.
        guard cfg.humansEnabled || cfg.sessionsEnabled else {
            return results
        }

        let rootPath = (cfg.rootPath as NSString).expandingTildeInPath
        guard deps.filesystem.fileExists(at: rootPath) else {
            results.append(CheckResult(
                name: "anarlog:path_missing",
                status: .fail,
                details: "configured path does not exist: \(rootPath)"))
            return results
        }

        if cfg.humansEnabled {
            let humansPath = (rootPath as NSString).appendingPathComponent("humans")
            if !deps.filesystem.fileExists(at: humansPath) {
                results.append(CheckResult(
                    name: "anarlog:humans_subdir_missing",
                    status: .warn,
                    details: "humans/ subdirectory not found under \(rootPath)"))
            } else {
                // Probe readability and count files via listDirectory.
                // A FilesystemError.permissionDenied is the TCC
                // Files & Folders rejection the plan (D20) calls out
                // as `anarlog:files_folders_permission_denied`.
                do {
                    let entries = try deps.filesystem.listDirectory(at: humansPath)
                    let mdCount = entries.filter { $0.hasSuffix(".md") }.count
                    results.append(CheckResult(
                        name: "anarlog:humans_count",
                        status: .pass,
                        details: "\(mdCount) human file(s) in \(humansPath)"))
                } catch FilesystemError.permissionDenied {
                    results.append(CheckResult(
                        name: "anarlog:files_folders_permission_denied",
                        status: .fail,
                        details: "EACCES on \(humansPath); grant Files & Folders to crm-mac in System Settings"))
                } catch {
                    results.append(CheckResult(
                        name: "anarlog:humans_count",
                        status: .warn,
                        details: "list humans/ failed: \(error)"))
                }
                if let humansSource {
                    results.append(lastTickResult(
                        sourceName: "anarlog_humans.last_tick",
                        state: humansSource,
                        intervalSeconds: 5 * 60))
                }
            }
        }
        if cfg.sessionsEnabled {
            let sessionsPath = (rootPath as NSString).appendingPathComponent("sessions")
            if !deps.filesystem.fileExists(at: sessionsPath) {
                results.append(CheckResult(
                    name: "anarlog:sessions_subdir_missing",
                    status: .warn,
                    details: "sessions/ subdirectory not found under \(rootPath)"))
            } else {
                do {
                    let entries = try deps.filesystem.listDirectory(at: sessionsPath)
                    // Sessions are UUID-named directories. We
                    // best-effort count UUID-shaped entries rather
                    // than every entry (Anarlog drops settings.json,
                    // etc. in the root) — uses a 36-char + hyphen
                    // shape probe rather than the full UUID
                    // validator to keep Doctor free of any anarlog-
                    // target dependency.
                    let sessionCount = entries.filter {
                        $0.count == 36 && $0.filter { $0 == "-" }.count == 4
                    }.count
                    results.append(CheckResult(
                        name: "anarlog:sessions_count",
                        status: .pass,
                        details: "\(sessionCount) session(s) in \(sessionsPath)"))
                } catch FilesystemError.permissionDenied {
                    results.append(CheckResult(
                        name: "anarlog:files_folders_permission_denied",
                        status: .fail,
                        details: "EACCES on \(sessionsPath); grant Files & Folders to crm-mac in System Settings"))
                } catch {
                    results.append(CheckResult(
                        name: "anarlog:sessions_count",
                        status: .warn,
                        details: "list sessions/ failed: \(error)"))
                }
                if let sessionsSource {
                    results.append(lastTickResult(
                        sourceName: "anarlog_sessions.last_tick",
                        state: sessionsSource,
                        intervalSeconds: 60 * 60))
                }
            }
        }
        return results
    }

    private func lastTickResult(
        sourceName: String,
        state: SourceState,
        intervalSeconds: TimeInterval
    ) -> CheckResult {
        let bumps = [state.lastScheduledAt, state.lastPushedAt].compactMap { $0 }
        guard let latest = bumps.max() else {
            return CheckResult(
                name: sourceName,
                status: .warn,
                details: "no tick recorded yet")
        }
        let age = deps.clock.now().timeIntervalSince(latest)
        if age > 2 * intervalSeconds {
            return CheckResult(
                name: sourceName,
                status: .warn,
                details: "last activity \(Int(age))s ago (threshold \(Int(2 * intervalSeconds))s)")
        }
        return CheckResult(
            name: sourceName,
            status: .pass,
            details: "last activity \(Int(age))s ago")
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
        //
        // `.denied` and `.notDetermined` reads from a shell-spawned
        // doctor process carry NO information about the daemon's
        // actual TCC state — they're attributed to the parent
        // terminal, which typically has no Contacts grant. The
        // launchd-attributed daemon is authoritative, so we WARN with
        // a message that points the operator at `crm-mac status`
        // instead of telling them to grant the terminal Contacts
        // access (which would defeat the bundle-attribution model).
        // `.restricted` stays FAIL because MDM / parental-controls
        // really is process-independent.
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
                status: .warn,
                details: "indeterminate from shell context — daemon is authoritative. Check `crm-mac status` for the daemon's last-tick auth state."))
        case .restricted:
            results.append(CheckResult(
                name: "icloud_contacts.permission",
                status: .fail,
                details: "restricted by MDM / parental controls"))
        case .notDetermined:
            results.append(CheckResult(
                name: "icloud_contacts.permission",
                status: .warn,
                details: "indeterminate from shell context — daemon is authoritative. If the daemon has never ticked, run `crm-mac install --re-request-permission` from this same shell."))
        }

        // 2. Allowlist sanity.
        //
        // Same shell-attribution caveat as the permission read —
        // `listContainers()` from a shell-spawned doctor can fail
        // with `.notAuthorized` even when the daemon under launchd
        // is fully authorized and ticking. The previous catch
        // assigned `visible = []`, which made every configured ID
        // look like an "orphan" — a high-friction false positive.
        // Now we report enumeration unavailability as its own WARN
        // state and skip the orphan computation. Either error branch
        // must fall through to the last-tick check below; neither
        // may early-return, so a transient enumeration failure
        // doesn't suppress the unrelated last-tick result.
        if allowlist.isEmpty {
            results.append(CheckResult(
                name: "icloud_contacts.allowlist",
                status: .warn,
                details: "no containers configured; run `crm-mac configure containers`"))
        } else {
            let allowlistCheck: CheckResult
            do {
                let visible = try deps.containerEnumerator.listContainers()
                let visibleIDs = Set(visible.map(\.identifier))
                let orphans = allowlist.filter { !visibleIDs.contains($0) }
                if orphans.isEmpty {
                    allowlistCheck = CheckResult(
                        name: "icloud_contacts.allowlist",
                        status: .pass,
                        details: "\(allowlist.count) container(s) configured; all visible")
                } else {
                    let orphanList = orphans.joined(separator: ",")
                    allowlistCheck = CheckResult(
                        name: "icloud_contacts.allowlist",
                        status: .warn,
                        details: "\(orphans.count) orphaned identifier(s) (no longer visible): \(orphanList)")
                }
            } catch ContactContainerEnumeratorError.notAuthorized {
                // `.restricted` blocks Contacts process-wide (MDM /
                // parental controls), NOT just from the shell. When
                // the auth read just told us `.restricted`,
                // attributing the enumeration failure to "shell
                // context" would lull the operator into ignoring a
                // genuine hard failure. Report it as a restriction
                // instead.
                if status == .restricted {
                    allowlistCheck = CheckResult(
                        name: "icloud_contacts.allowlist",
                        status: .fail,
                        details: "\(allowlist.count) configured (visibility check blocked by MDM / parental controls)")
                } else {
                    allowlistCheck = CheckResult(
                        name: "icloud_contacts.allowlist",
                        status: .warn,
                        details: "\(allowlist.count) configured (visibility check unavailable from shell context — daemon is authoritative)")
                }
            } catch {
                allowlistCheck = CheckResult(
                    name: "icloud_contacts.allowlist",
                    status: .warn,
                    details: "container enumeration failed: \(error)")
            }
            results.append(allowlistCheck)
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
    /// (Pi reachability + icloud_contacts + anarlog) need to inspect.
    /// Named for self-documentation where the prior 3-tuple shape
    /// required positional unwrap.
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
        /// Configured anarlog config (path + enable flags). Nil when
        /// the config file lacks the `sources.anarlog` key.
        let anarlogConfig: AnarlogConfig?
        /// Per-source state for the anarlog reader plugins. Nil when
        /// the source has never ticked.
        let anarlogHumansSourceState: SourceState?
        let anarlogSessionsSourceState: SourceState?
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

    private func checkAgentService() -> CheckResult {
        switch deps.agentService.currentStatus() {
        case .enabled:
            return CheckResult(
                name: "agent_service",
                status: .pass,
                details: "registered (enabled)")
        case .requiresApproval:
            return CheckResult(
                name: "agent_service",
                status: .warn,
                details: "requires approval — System Settings → General → Login Items → Allow in Background → crm-mac")
        case .notRegistered:
            return CheckResult(
                name: "agent_service",
                status: .warn,
                details: "not registered; run `crm-mac install --register-only`")
        case .notFound:
            return CheckResult(
                name: "agent_service",
                status: .fail,
                details: "bundle missing at \(deps.paths.bundleAppPath); re-run `crm-mac install --upgrade`")
        }
    }

    private func checkConfigAndState() -> ConfigAndStateResult {
        let placeholderURL = URL(string: "https://localhost")!
        // Local helper closure so each early-return path produces a
        // fully-populated ConfigAndStateResult without 9 near-identical
        // call sites going stale when a new field lands on the struct.
        func failure(
            details: String,
            cfg: DaemonConfig? = nil,
            state: DaemonState? = nil
        ) -> ConfigAndStateResult {
            let icloud = cfg?.sources?.icloudContacts?.containers ?? []
            let anarlog = cfg?.sources?.anarlog
            return ConfigAndStateResult(
                result: CheckResult(
                    name: "config_state", status: .fail, details: details),
                auth: nil,
                piURL: cfg?.piURL ?? placeholderURL,
                icloudAllowlist: icloud,
                icloudSourceState: state?.sources["icloud_contacts"],
                anarlogConfig: anarlog,
                anarlogHumansSourceState: state?.sources["anarlog_humans"],
                anarlogSessionsSourceState: state?.sources["anarlog_sessions"])
        }

        guard deps.filesystem.fileExists(at: deps.paths.configFilePath) else {
            return failure(details: "config.json missing")
        }
        let configData: Data
        do {
            configData = try deps.filesystem.read(from: deps.paths.configFilePath)
        } catch {
            return failure(details: "read config.json: \(error)")
        }
        let decoder = JSONDecoder()
        decoder.dateDecodingStrategy = .iso8601
        let cfg: DaemonConfig
        do {
            cfg = try decoder.decode(DaemonConfig.self, from: configData)
        } catch {
            return failure(details: "decode config.json: \(error)")
        }
        let icloudAllowlist = cfg.sources?.icloudContacts?.containers ?? []
        let anarlogConfig = cfg.sources?.anarlog

        guard deps.filesystem.fileExists(at: deps.paths.stateFilePath) else {
            return failure(details: "state.json missing", cfg: cfg)
        }
        let stateData: Data
        do {
            stateData = try deps.filesystem.read(from: deps.paths.stateFilePath)
        } catch {
            return failure(details: "read state.json: \(error)", cfg: cfg)
        }
        let state: DaemonState
        do {
            state = try decoder.decode(DaemonState.self, from: stateData)
        } catch {
            return failure(details: "decode state.json: \(error)", cfg: cfg)
        }
        if state.schemaVersion != DaemonState.currentSchemaVersion {
            return failure(
                details: "state schemaVersion=\(state.schemaVersion); expected \(DaemonState.currentSchemaVersion)",
                cfg: cfg, state: state)
        }
        let icloudSourceState = state.sources["icloud_contacts"]
        let anarlogHumansState = state.sources["anarlog_humans"]
        let anarlogSessionsState = state.sources["anarlog_sessions"]
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
                icloudSourceState: icloudSourceState,
                anarlogConfig: anarlogConfig,
                anarlogHumansSourceState: anarlogHumansState,
                anarlogSessionsSourceState: anarlogSessionsState)
        }
        return ConfigAndStateResult(
            result: CheckResult(
                name: "config_state",
                status: .pass,
                details: "host=\(cfg.hostname) schemaVersion=\(state.schemaVersion)"),
            auth: PiAuth(hostID: cfg.hostID, apiKey: apiKey),
            piURL: cfg.piURL,
            icloudAllowlist: icloudAllowlist,
            icloudSourceState: icloudSourceState,
            anarlogConfig: anarlogConfig,
            anarlogHumansSourceState: anarlogHumansState,
            anarlogSessionsSourceState: anarlogSessionsState)
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
