// `crm-mac install` parses options and delegates to
// CRMMacLifecycle.Installer. The branchy validation lives in
// InstallRequestParser so it can be unit-tested without standing up
// the executable target (which has no test target by design).
//
// After a successful pair the install flow also requests Contacts
// permission and runs the iCloud Contacts container picker.
// `--re-request-permission` re-runs only the permission + picker
// flow (used when the initial install was aborted at the permission
// step). `--containers <id1>,<id2>` is the non-interactive form for
// scripted installs that know the exact CNContainer identifiers
// from a prior `crm-mac configure containers --list` run.
import Foundation
import ArgumentParser
import CRMMacCore
import CRMMacIcloudContactsSource
import CRMMacLifecycle

struct InstallCommand: AsyncParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "install",
        abstract: "Pair with the Pi and register the launchd agent.")

    @Option(name: .long, help: "Pi base URL (e.g. https://pi.example.ts.net)")
    var piURL: String = ""

    @Option(name: .long, help: "Single-use pairing token from `crm-admin --mint-pairing-token` on the Pi")
    var pair: String = ""

    @Option(name: .long, help: "Non-PII hostname label (e.g. mac-1). REQUIRED on first install.")
    var hostname: String = ""

    @Flag(name: .long, help: "Re-install the binary in place. Does NOT re-pair.")
    var upgrade: Bool = false

    @Flag(name: .long, help: "Re-register an existing install with launchd. Does NOT replace the binary or re-pair.")
    var registerOnly: Bool = false

    @Flag(name: .long, help: "Re-run only the Contacts-permission prompt + iCloud container picker. Skips pairing.")
    var reRequestPermission: Bool = false

    @Option(
        name: .long,
        help: "Non-interactive iCloud Contacts allowlist: comma-separated CNContainer identifiers.")
    var containers: String = ""

    mutating func run() async throws {
        let ctx = ProductionContext()

        if reRequestPermission {
            try await runReRequestPermissionOnly(ctx: ctx)
            return
        }

        let input = InstallRequestParserInput(
            piURL: piURL,
            pair: pair,
            hostname: hostname,
            upgrade: upgrade,
            registerOnly: registerOnly)
        let request: InstallRequest
        let warnings: InstallRequestParseWarnings
        do {
            (request, warnings) = try InstallRequestParser.parseWithWarnings(input)
        } catch let e as InstallRequestParseError {
            throw ValidationError(String(describing: e))
        }

        if warnings.plaintextHTTPNonLoopback {
            FileHandle.standardError.write(Data(
                "WARNING: --pi-url uses http:// to a non-loopback host. The API key is sent in a Bearer header on every request; plaintext over the network leaks it to anyone in the path. Use https:// (e.g. via Tailscale) for real installs.\n".utf8))
        }

        let installer = ctx.installer()
        let summary = try await installer.run(request)
        print("install complete")
        print("  host_id=\(summary.hostID.uuidString.lowercased())")
        print("  binary=\(summary.binaryPath)")
        print("  plist=\(summary.plistPath)")
        print("")

        // Post-pair: request Contacts permission + run the container
        // picker. Fail the install if permission is denied or the
        // picker errors out — the operator either has a working
        // icloud source or they don't; a half-installed state is
        // worse than a clean failure because the post-success
        // messages would suggest setup was complete.
        // Skipped on --upgrade / --register-only — those flows
        // assume the user already accepted the permission prompt.
        let isFreshInstall = !(upgrade || registerOnly)
        if isFreshInstall {
            try await runContactsPermissionFlow(
                ctx: ctx, mutatingExistingConfig: false)
        }

        print("")
        print("First launch on this Mac may be blocked by Gatekeeper. If so:")
        print("  System Settings -> Privacy & Security -> scroll to \"crm-mac was blocked...\" -> Open Anyway")
    }

    /// Permission + picker flow. Called as part of fresh install AND
    /// via --re-request-permission. When the call site is updating
    /// an existing config (i.e. the daemon may already be running
    /// with the old allowlist), the writer sets the recovery flag
    /// FIRST so the next tick reconciles against the new allowlist
    /// via /known-ids rather than silently advancing past stale
    /// state.
    private func runContactsPermissionFlow(
        ctx: ProductionContext,
        mutatingExistingConfig: Bool
    ) async throws {
        let authAdapter = ctx.contactsAuthAdapter()
        let granted = try await authAdapter.requestAccess()
        if !granted {
            FileHandle.standardError.write(Data(
                """
                Contacts permission denied.
                Open: System Settings -> Privacy & Security -> Contacts -> enable crm-mac
                Then run: crm-mac install --re-request-permission
                """.utf8))
            throw ExitCode(1)
        }
        // Permission granted. Run picker.
        let enumerator = ctx.contactContainerEnumerator()
        let visible: [ContainerInfo]
        do {
            visible = try enumerator.listContainers()
        } catch {
            FileHandle.standardError.write(Data(
                "container enumeration failed: \(error)\n".utf8))
            throw ExitCode(1)
        }
        let pickedIDs: [String]
        if !containers.isEmpty {
            pickedIDs = try Self.resolveNonInteractive(raw: containers, visible: visible)
        } else {
            FileHandle.standardOutput.write(Data(
                ContainerPicker.render(visible).utf8))
            let line = readLine() ?? ""
            do {
                pickedIDs = try ContainerPicker.parse(input: line, containers: visible)
            } catch {
                FileHandle.standardError.write(Data("\(error)\n".utf8))
                throw ExitCode(2)
            }
        }
        let configStore = ConfigStore(
            fileURL: URL(fileURLWithPath: ctx.paths.configFilePath))
        if mutatingExistingConfig {
            // State-write FIRST per the same crash-safety contract
            // the `configure containers` subcommand follows. A crash
            // between state and config writes leaves the daemon
            // recovering against the OLD allowlist on the next tick
            // — still correct.
            let stateStore = StateStore(
                fileURL: URL(fileURLWithPath: ctx.paths.stateFilePath))
            let mutator = StateMutator(store: stateStore)
            try await mutator.mutate { state in
                var src = state.sources["icloud_contacts"] ?? SourceState()
                src.lastError = "recovery_requested:allowlist_changed"
                src.lastErrorAt = Date()
                state.sources["icloud_contacts"] = src
            }
        }
        try configStore.saveICloudContactsConfig(
            ICloudContactsConfig(containers: pickedIDs))
        print("iCloud Contacts allowlist saved (\(pickedIDs.count) container(s)).")
    }

    /// Re-request-permission-only flow. Re-runs the permission prompt
    /// and picker WITHOUT touching the existing pair. Used when an
    /// initial install was aborted at the permission step. Treated
    /// as an existing-config mutation so the recovery flag is set
    /// before any allowlist write.
    private func runReRequestPermissionOnly(ctx: ProductionContext) async throws {
        guard FileManager.default.fileExists(atPath: ctx.paths.configFilePath) else {
            FileHandle.standardError.write(Data(
                "--re-request-permission requires a prior pair; no config.json at \(ctx.paths.configFilePath)\n".utf8))
            throw ExitCode(3)
        }
        try await runContactsPermissionFlow(
            ctx: ctx, mutatingExistingConfig: true)
    }

    private static func resolveNonInteractive(
        raw: String,
        visible: [ContainerInfo]
    ) throws -> [String] {
        let visibleIDs = Set(visible.map(\.identifier))
        let parts = raw.split(separator: ",").map {
            $0.trimmingCharacters(in: .whitespaces)
        }
        for id in parts {
            if !visibleIDs.contains(id) {
                FileHandle.standardError.write(Data(
                    "container identifier \(id) is not in the visible CNContainer list. Run `crm-mac configure containers --list` to see available IDs.\n".utf8))
                throw ExitCode(2)
            }
        }
        return parts
    }
}
