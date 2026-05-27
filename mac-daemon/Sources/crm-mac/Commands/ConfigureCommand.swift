// `crm-mac configure` hosts subcommands that mutate non-pair
// configuration. Currently: `containers` for the iCloud Contacts
// CNContainer allowlist. Future per-source configs land as sibling
// subcommands.
//
// Allowlist-mutation flow:
//   - List mode (`--list`): enumerate CNContainers, print TSV, exit.
//     Requires Contacts permission to enumerate.
//   - Interactive: enumerate → picker UI → diff → y/N → state-then-
//     config write.
//   - Non-interactive (`--containers <uuid,…>`): SKIP enumeration
//     entirely; trust the operator-supplied UUIDs and write the
//     allowlist. Skipping enumeration is what makes this path work
//     from a shell where Contacts permission is attributed to the
//     parent terminal rather than the daemon's bundle. The daemon's
//     next tick under launchd validates against the live container
//     list and surfaces invalid UUIDs as a recovery-requested
//     failure.
//
// The dispatch (which mode runs which sequence) lives in
// `AllowlistConfigureFlow` in CRMMacLifecycle so the zero-Contacts-
// calls contract on the non-interactive paths can be unit-tested.
// The executable target has no test target by design.
//
// Crash-safety: on a non-empty diff, the recovery flag in
// `state.json` is bumped FIRST, then `config.json` is replaced.
// A crash between the two leaves the daemon recovering against
// the OLD allowlist on next tick — still correct. The
// state-then-config ordering lives inside
// `NonInteractiveAllowlistWriter`.
import Foundation
import ArgumentParser
import CRMMacAnarlogSource
import CRMMacCore
import CRMMacIcloudContactsSource
import CRMMacLifecycle
import CRMMacPiClient

struct ConfigureCommand: ParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "configure",
        abstract: "Interactive configuration (per-source subcommands).",
        subcommands: [ContainersSubcommand.self, AnarlogSubcommand.self])

    mutating func run() throws {
        print("crm-mac configure: pass a subcommand.")
        print("  containers   Manage the iCloud Contacts allowlist.")
        print("  anarlog      Configure the Anarlog notes path + enable flags.")
        print("")
        print("Run `crm-mac configure <subcommand> --help` for details.")
    }
}

struct ContainersSubcommand: ParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "containers",
        abstract: "Pick or update the iCloud Contacts CNContainer allowlist.")

    @Flag(name: .long, help: "Print the current CNContainer list in script-consumable TSV format; no prompts.")
    var list: Bool = false

    @Option(
        name: .long,
        help: "Non-interactive: comma-separated CNContainer identifiers (UUIDs).")
    var containers: String = ""

    mutating func run() throws {
        let ctx = ProductionContext()

        // Refuse to run while the daemon is up so the allowlist
        // mutation doesn't race against an in-flight tick.
        try requireDaemonNotRunning(paths: ctx.paths)

        let mode: AllowlistConfigureFlow.Mode = list
            ? .configureList
            : (containers.isEmpty
                ? .configureInteractive
                : .configureNonInteractive(rawContainers: containers))
        let configURL = URL(fileURLWithPath: ctx.paths.configFilePath)
        // Wire the production adapter factories as lazy closures.
        // The flow only invokes them on interactive / list modes;
        // the non-interactive branch never constructs an adapter,
        // which is the structural regression guard for the
        // shell-context Contacts permission issue.
        let authFactory: @Sendable () -> ContactsAuthorizationAdapter = {
            ProductionContext().contactsAuthAdapter()
        }
        let enumeratorFactory: @Sendable () -> ContactContainerEnumerator = {
            ProductionContext().contactContainerEnumerator()
        }
        let flow = AllowlistConfigureFlow(
            mode: mode,
            configStore: ConfigStore(fileURL: configURL),
            stateStore: StateStore(fileURL: URL(fileURLWithPath: ctx.paths.stateFilePath)),
            authAdapter: authFactory,
            enumerator: enumeratorFactory,
            interactivePicker: { visible in
                try Self.confirmAndPick(visible: visible, configURL: configURL)
            })
        do {
            let outcome = try Self.runFlowSync(flow: flow)
            switch outcome {
            case .listed(let visible):
                for c in visible {
                    let typeLabel = Self.label(for: c.type)
                    let defaultFlag = c.defaultIncluded ? "default" : "skip"
                    print("\(c.identifier)\t\(typeLabel)\t\(c.name)\t\(defaultFlag)")
                }
            case .wrote, .completedInteractive:
                print("Allowlist updated. Run `crm-mac status` to confirm the next tick performs the reconciliation.")
            case .noOp:
                print("No allowlist changes detected.")
            }
        } catch AllowlistConfigureFlowError.permissionDenied {
            // Interactive picker path only — non-interactive bypasses
            // the auth prompt.
            FileHandle.standardError.write(Data(
                "Contacts permission denied. Open: System Settings -> Privacy & Security -> Contacts -> enable crm-mac\n".utf8))
            throw ExitCode(1)
        } catch ContactContainerEnumeratorError.notAuthorized {
            // Interactive or --list modes only — non-interactive
            // doesn't call the enumerator. Preserves prior wording.
            FileHandle.standardError.write(Data(
                "Contacts permission missing. Run `crm-mac install --re-request-permission` to grant.\n".utf8))
            throw ExitCode(1)
        } catch let e as ContactContainerEnumeratorError {
            FileHandle.standardError.write(Data(
                "container enumeration failed: \(e)\n".utf8))
            throw ExitCode(1)
        } catch AllowlistConfigureFlowError.userAborted {
            print("Aborted; no changes written.")
        } catch let e as ContainerPickerError {
            FileHandle.standardError.write(Data("\(e)\n".utf8))
            throw ExitCode(2)
        } catch let e as NonInteractiveAllowlistWriteError {
            // Phase-specific recovery guidance for the partial-write
            // case: the recovery flag is set in state.json but
            // config.json was not replaced. The operator should
            // re-run this command to complete the swap; the next
            // tick will reconcile against the OLD allowlist
            // meanwhile (still correct, idempotent).
            switch e {
            case .stateWriteFailed(let underlying):
                FileHandle.standardError.write(Data(
                    "Failed to set recovery flag in state.json: \(underlying)\n".utf8))
            case .configWriteFailedAfterStateWrite(let underlying):
                FileHandle.standardError.write(Data(
                    "Failed to write config.json: \(underlying)\n  (Recovery flag is set; re-run `crm-mac configure containers` to retry.)\n".utf8))
            case .configWriteFailed(let underlying):
                FileHandle.standardError.write(Data(
                    "Failed to write config.json: \(underlying)\n".utf8))
            }
            throw ExitCode(1)
        } catch {
            FileHandle.standardError.write(Data("\(error)\n".utf8))
            throw ExitCode(2)
        }
    }

    /// Bridge async `flow.run()` into the synchronous
    /// `ParsableCommand.run()` flow via a DispatchSemaphore.
    /// The Result is captured through a mutable holder marked
    /// `@unchecked Sendable`: the Task body owns the write and the
    /// outer thread reads only after `sem.wait()`, so no actual race
    /// is possible. Swift 6's strict concurrency check cannot prove
    /// this happens-before relationship across the
    /// DispatchSemaphore, so the holder is the smallest unchecked
    /// boundary.
    /// Internal-only sentinel for the (effectively unreachable)
    /// case where the Task body neither succeeds nor throws but
    /// the semaphore is somehow signalled. Throwing a distinct
    /// error makes a future regression surface at the actual fault
    /// point instead of looking like a user-aborted picker run.
    private enum RunFlowSyncFailure: Error {
        case taskDidNotComplete
    }

    private static func runFlowSync(
        flow: AllowlistConfigureFlow
    ) throws -> AllowlistConfigureFlow.Outcome {
        final class ResultBox: @unchecked Sendable {
            var result: Result<AllowlistConfigureFlow.Outcome, Error>?
        }
        let box = ResultBox()
        let sem = DispatchSemaphore(value: 0)
        Task {
            do {
                let outcome = try await flow.run()
                box.result = .success(outcome)
            } catch {
                box.result = .failure(error)
            }
            sem.signal()
        }
        sem.wait()
        switch box.result {
        case .success(let outcome): return outcome
        case .failure(let error): throw error
        case .none:
            // Defensive — the Task body always sets one of the
            // two before signalling. If it didn't, we want a
            // distinct error that surfaces the fault point.
            throw RunFlowSyncFailure.taskDidNotComplete
        }
    }

    /// Interactive picker for the `configure containers` flow:
    /// renders the picker UI, parses operator input, diffs against
    /// the existing allowlist, and prompts y/N. Throws
    /// `AllowlistConfigureFlowError.userAborted` on a no-confirm
    /// answer; throws `ContainerPickerError` on bad input.
    private static func confirmAndPick(
        visible: [ContainerInfo],
        configURL: URL
    ) throws -> [String] {
        FileHandle.standardOutput.write(Data(
            ContainerPicker.render(visible).utf8))
        let line = readLine() ?? ""
        let pickedIDs = try ContainerPicker.parse(input: line, containers: visible)

        // Diff against the existing allowlist.
        let existingAllowlist = (try? ConfigStore(fileURL: configURL)
            .loadICloudContactsConfig()?.containers) ?? []
        let existing = Set(existingAllowlist)
        let picked = Set(pickedIDs)
        let added = picked.subtracting(existing)
        let removed = existing.subtracting(picked)
        if added.isEmpty && removed.isEmpty {
            // No-op: still return the picked IDs so the writer
            // observes picked == existing and yields `.noOp`,
            // which the outer switch maps to the "No allowlist
            // changes detected." line. Avoids a custom early-exit
            // path here.
            return pickedIDs
        }

        print("Planned allowlist changes:")
        if !added.isEmpty {
            print("  Added:")
            for id in added.sorted() { print("    + \(id)") }
        }
        if !removed.isEmpty {
            print("  Removed:")
            for id in removed.sorted() { print("    - \(id)") }
        }
        print("")
        print("On confirm, the icloud_contacts source will perform a full /known-ids")
        print("reconciliation on its next tick: contacts removed from the allowlist")
        print("get tombstoned on the Pi; contacts added to it get upserted.")
        print("The local hash cache is preserved so deletes carry deterministic")
        print("prior hashes.")
        print("")
        print("Continue? [y/N] ", terminator: "")
        let answer = (readLine() ?? "").lowercased()
        if answer != "y" && answer != "yes" {
            throw AllowlistConfigureFlowError.userAborted
        }
        return pickedIDs
    }

    private static func label(for kind: ContainerKind) -> String {
        switch kind {
        case .local: return "local"
        case .cardDAV: return "cardDAV"
        case .exchange: return "exchange"
        case .unassigned: return "unassigned"
        case .unknown(let raw): return "unknown(\(raw))"
        }
    }
}

/// Refuses to proceed when the daemon's pidfile is present.
private func requireDaemonNotRunning(paths: LifecyclePaths) throws {
    let pidPath = URL(fileURLWithPath: paths.runtimeDirPath)
        .appendingPathComponent("daemon.pid").path
    guard FileManager.default.fileExists(atPath: pidPath) else { return }
    FileHandle.standardError.write(Data(
        "crm-mac daemon appears to be running (pidfile at \(pidPath)). Stop it first: `crm-mac stop`.\n".utf8))
    throw ExitCode(3)
}

// MARK: - Anarlog subcommand

/// `crm-mac configure anarlog [--path <abs>] [--enable …] [--disable …]
/// [--reset-cursor …]` — manage the Anarlog notes path + per-source
/// enable flags. The daemon must be stopped for any mutation.
///
/// --enable / --disable / --reset-cursor accept one of:
///   humans | sessions | both
struct AnarlogSubcommand: ParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "anarlog",
        abstract: "Configure the Anarlog notes path and enable flags.")

    enum Target: String, ExpressibleByArgument {
        case humans
        case sessions
        case both
    }

    @Option(name: .long, help: "Absolute path to the Anarlog notes root (containing humans/ and sessions/).")
    var path: String?

    @Option(name: .long, help: "Enable a source: humans | sessions | both.")
    var enable: Target?

    @Option(name: .long, help: "Disable a source: humans | sessions | both.")
    var disable: Target?

    @Option(name: .long, help: "Reset the Pi-side cursor for a source: humans | sessions | both. Daemon must be stopped.")
    var resetCursor: Target?

    mutating func run() throws {
        let ctx = ProductionContext()
        try requireDaemonNotRunning(paths: ctx.paths)

        let configStore = ConfigStore(
            fileURL: URL(fileURLWithPath: ctx.paths.configFilePath))

        if resetCursor != nil {
            // Reset is a separate code path — it talks to the Pi to
            // commit an empty cursor; doesn't touch config.json.
            try runResetCursor(target: resetCursor!, ctx: ctx)
            return
        }

        var current: AnarlogConfig
        do {
            if let existing = try configStore.loadAnarlogConfig() {
                current = existing
            } else if let path {
                guard isAbsolutePath(path) else {
                    FileHandle.standardError.write(Data(
                        "anarlog path must be absolute, got: \(path)\n".utf8))
                    throw ExitCode(2)
                }
                current = AnarlogConfig(rootPath: path)
            } else {
                FileHandle.standardError.write(Data(
                    "no anarlog config yet — pass --path <abs-path> on first run.\n".utf8))
                throw ExitCode(2)
            }
        } catch {
            FileHandle.standardError.write(Data(
                "failed to load config.json: \(error)\n".utf8))
            throw ExitCode(2)
        }

        if let path {
            guard isAbsolutePath(path) else {
                FileHandle.standardError.write(Data(
                    "anarlog path must be absolute, got: \(path)\n".utf8))
                throw ExitCode(2)
            }
            current.rootPath = path
        }
        if let enable {
            switch enable {
            case .humans: current.humansEnabled = true
            case .sessions: current.sessionsEnabled = true
            case .both:
                current.humansEnabled = true
                current.sessionsEnabled = true
            }
        }
        if let disable {
            switch disable {
            case .humans: current.humansEnabled = false
            case .sessions: current.sessionsEnabled = false
            case .both:
                current.humansEnabled = false
                current.sessionsEnabled = false
            }
        }
        do {
            try configStore.saveAnarlogConfig(current)
        } catch {
            FileHandle.standardError.write(Data(
                "failed to write config.json: \(error)\n".utf8))
            throw ExitCode(1)
        }
        print("anarlog config updated.")
        print("  root_path:        \(current.rootPath)")
        print("  humans_enabled:   \(current.humansEnabled)")
        print("  sessions_enabled: \(current.sessionsEnabled)")
        print("")
        print("Start the daemon for changes to take effect: `crm-mac start`.")
    }

    /// Anarlog --reset-cursor handshake: load auth from keychain +
    /// config, GET the current cursor (for baseCursor + cursorEpoch),
    /// POST commitCursor with cursor="". On 409: refetch + retry once.
    private func runResetCursor(target: Target, ctx: ProductionContext) throws {
        let configStore = ConfigStore(
            fileURL: URL(fileURLWithPath: ctx.paths.configFilePath))
        let cfg: DaemonConfig
        do {
            cfg = try configStore.load()
        } catch {
            FileHandle.standardError.write(Data(
                "failed to load config.json: \(error)\n".utf8))
            throw ExitCode(1)
        }
        let apiKey: String
        do {
            apiKey = try ctx.keychain.readAPIKey()
        } catch {
            FileHandle.standardError.write(Data(
                "failed to read API key from Keychain: \(error)\n".utf8))
            throw ExitCode(1)
        }
        let auth = PiAuth(hostID: cfg.hostID, apiKey: apiKey)
        let client = PiClient(baseURL: cfg.piURL, logger: ctx.logger)

        let sources: [String]
        switch target {
        case .humans: sources = ["anarlog_humans"]
        case .sessions: sources = ["anarlog_sessions"]
        case .both: sources = ["anarlog_humans", "anarlog_sessions"]
        }

        let sem = DispatchSemaphore(value: 0)
        nonisolated(unsafe) var thrown: Error?
        Task {
            do {
                for source in sources {
                    try await AnarlogCursorReset.resetOne(
                        client: client, auth: auth, source: source)
                }
            } catch {
                thrown = error
            }
            sem.signal()
        }
        sem.wait()
        if let thrown {
            FileHandle.standardError.write(Data(
                "cursor reset failed: \(thrown)\n".utf8))
            throw ExitCode(1)
        }
        for source in sources {
            print("\(source): cursor reset.")
        }
    }

    private func isAbsolutePath(_ s: String) -> Bool {
        // Reject tildes too — operators should expand themselves; the
        // daemon expands when reading config, but the persisted path
        // should be the canonical absolute form.
        s.hasPrefix("/")
    }
}
