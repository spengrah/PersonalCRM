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
//
// The post-pair flow is dispatched through `AllowlistConfigureFlow`
// (in CRMMacLifecycle). The flow's Mode encodes whether this is a
// fresh install or `--re-request-permission`, and whether the
// operator supplied `--containers` (non-interactive) or wants the
// picker (interactive). Non-interactive modes deliberately make
// ZERO Contacts framework calls — see issue #322 — which is why
// the dispatch lives in the testable lifecycle library rather than
// inline here.
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
        print("  bundle=\(summary.bundleAppPath)")
        print("  binary=\(summary.bundleBinaryPath)")
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
        print("If launchd reports the bundle requires approval, open:")
        print("  System Settings -> General -> Login Items -> Allow in Background -> enable crm-mac")
        print("Then re-run `crm-mac install --register-only`.")
    }

    /// Permission + picker flow. Called as part of fresh install AND
    /// via --re-request-permission. The dispatch (interactive vs
    /// non-interactive, fresh vs re-request) happens inside
    /// `AllowlistConfigureFlow`; this wrapper only constructs the
    /// flow with the appropriate `Mode` and surfaces flow errors as
    /// stderr + exit codes that match the prior behaviour.
    private func runContactsPermissionFlow(
        ctx: ProductionContext,
        mutatingExistingConfig: Bool
    ) async throws {
        let mode: AllowlistConfigureFlow.Mode
        if mutatingExistingConfig {
            mode = containers.isEmpty
                ? .reRequestPermissionInteractive
                : .reRequestPermissionNonInteractive(rawContainers: containers)
        } else {
            mode = containers.isEmpty
                ? .freshInstallInteractive
                : .freshInstallNonInteractive(rawContainers: containers)
        }
        // Bind production adapters into Sendable-conformant locals
        // so the closures captured by `AllowlistConfigureFlow` don't
        // need to retain the non-Sendable ProductionContext.
        let auth = ctx.contactsAuthAdapter()
        let enumerator = ctx.contactContainerEnumerator()
        let flow = AllowlistConfigureFlow(
            mode: mode,
            configStore: ConfigStore(fileURL: URL(fileURLWithPath: ctx.paths.configFilePath)),
            stateStore: StateStore(fileURL: URL(fileURLWithPath: ctx.paths.stateFilePath)),
            authAdapter: { auth },
            enumerator: { enumerator },
            interactivePicker: { visible in
                FileHandle.standardOutput.write(Data(
                    ContainerPicker.render(visible).utf8))
                let line = readLine() ?? ""
                return try ContainerPicker.parse(input: line, containers: visible)
            })
        do {
            let outcome = try await flow.run()
            switch outcome {
            case .wrote(let ids), .completedInteractive(let ids):
                print("iCloud Contacts allowlist saved (\(ids.count) container(s)).")
            case .noOp:
                print("No allowlist changes detected.")
            case .listed:
                // Unreachable: install flow never uses .configureList.
                break
            }
        } catch AllowlistConfigureFlowError.permissionDenied {
            FileHandle.standardError.write(Data(
                """
                Contacts permission denied.
                Open: System Settings -> Privacy & Security -> Contacts -> enable crm-mac
                Then run: crm-mac install --re-request-permission
                """.utf8))
            throw ExitCode(1)
        } catch let e as ContactContainerEnumeratorError {
            // Enumeration failure on the interactive path (the
            // non-interactive flow doesn't call the enumerator).
            FileHandle.standardError.write(Data(
                "container enumeration failed: \(e)\n".utf8))
            throw ExitCode(1)
        } catch let e as ContainerPickerError {
            // Interactive picker input parsing failed.
            FileHandle.standardError.write(Data("\(e)\n".utf8))
            throw ExitCode(2)
        }
    }

    /// Re-request-permission-only flow. Re-runs the permission prompt
    /// and picker WITHOUT touching the existing pair. Used when an
    /// initial install was aborted at the permission step. Treated
    /// as an existing-config mutation so the recovery flag is set
    /// before any allowlist write (when the diff is non-empty).
    private func runReRequestPermissionOnly(ctx: ProductionContext) async throws {
        guard FileManager.default.fileExists(atPath: ctx.paths.configFilePath) else {
            FileHandle.standardError.write(Data(
                "--re-request-permission requires a prior pair; no config.json at \(ctx.paths.configFilePath)\n".utf8))
            throw ExitCode(3)
        }
        try await runContactsPermissionFlow(
            ctx: ctx, mutatingExistingConfig: true)
    }
}
