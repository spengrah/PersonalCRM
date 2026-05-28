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
// ZERO Contacts framework calls (the shell-context TCC
// attribution would otherwise fail with a misleading error), which
// is why the dispatch lives in the testable lifecycle library
// rather than inline here.
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

    @Flag(name: .long, help: "Rotate this Mac's pair-key in place. Requires --pair <TOKEN>. Preserves host_id, launchd registration, TCC grants, and daemon state.")
    var rePair: Bool = false

    @Option(
        name: .long,
        help: "Non-interactive iCloud Contacts allowlist: comma-separated CNContainer identifiers.")
    var containers: String = ""

    mutating func run() async throws {
        let ctx = ProductionContext()

        if rePair {
            try await runRePairFlow(ctx: ctx)
            return
        }

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
            configStore: ConfigStore(fileURL: URL(fileURLWithPath: ctx.paths.configFilePath)),
            stateStore: StateStore(fileURL: URL(fileURLWithPath: ctx.paths.stateFilePath)),
            authAdapter: authFactory,
            enumerator: enumeratorFactory,
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
        } catch let e as NonInteractiveAllowlistWriteError {
            // Phase-specific recovery guidance for state/config
            // partial-writes. Mirrors the configure-containers
            // wrapper so a `--re-request-permission` run that
            // half-completes points the operator at the right
            // retry path.
            switch e {
            case .stateWriteFailed(let underlying):
                FileHandle.standardError.write(Data(
                    "Failed to set recovery flag in state.json: \(underlying)\n".utf8))
            case .configWriteFailedAfterStateWrite(let underlying):
                FileHandle.standardError.write(Data(
                    "Failed to write config.json: \(underlying)\n  (Recovery flag is set; re-run `crm-mac install --re-request-permission` to retry.)\n".utf8))
            case .configWriteFailed(let underlying):
                FileHandle.standardError.write(Data(
                    "Failed to write config.json: \(underlying)\n".utf8))
            }
            throw ExitCode(1)
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

    /// In-place pair-key rotation flow. Reads the existing config +
    /// api-key, calls the rotate endpoint with the current creds + a
    /// fresh pairing token, atomically writes the new api-key to the
    /// existing file, and `launchctl kickstart -k` the daemon so it
    /// re-reads the rotated key. Preserves host_id, launchd
    /// registration, TCC grants, and daemon state.
    private func runRePairFlow(ctx: ProductionContext) async throws {
        // Validation: --re-pair requires --pair, and forbids every
        // other flag that would imply a different operation.
        if pair.isEmpty {
            throw ValidationError("--re-pair requires --pair <TOKEN>")
        }
        if !piURL.isEmpty {
            throw ValidationError("--re-pair reads pi_url from existing config; do not pass --pi-url")
        }
        if !hostname.isEmpty {
            throw ValidationError("--re-pair reads hostname from existing config; do not pass --hostname")
        }
        if upgrade {
            throw ValidationError("--re-pair and --upgrade are mutually exclusive")
        }
        if registerOnly {
            throw ValidationError("--re-pair and --register-only are mutually exclusive")
        }
        if !containers.isEmpty {
            throw ValidationError("--re-pair does not touch the Contacts allowlist; do not pass --containers")
        }
        if reRequestPermission {
            throw ValidationError("--re-pair and --re-request-permission are mutually exclusive")
        }

        let repairer = ctx.repairer()
        do {
            let result = try await repairer.run(newPairingToken: pair)
            print("re-pair complete")
            print("  host_id=\(result.hostID.uuidString.lowercased())")
            print("  api_key_rotated_at=\(ISO8601DateFormatter().string(from: result.apiKeyRotatedAt))")
            print("  daemon_restart_issued=\(result.daemonRestartIssued)")
            if !result.daemonRestartIssued {
                print("")
                if let warning = result.restartWarning {
                    print("Daemon restart was attempted but failed: \(warning)")
                } else {
                    print("Daemon restart was skipped.")
                }
                print("If a daemon is currently running it is still holding the OLD key in memory and will start returning 401 on its next heartbeat.")
                print("Run `crm-mac stop && crm-mac start` to force the daemon to restart and re-read the new api-key.")
            }
        } catch RepairerError.noExistingInstall(let reason) {
            FileHandle.standardError.write(Data(
                "--re-pair requires a prior install. \(reason)\nIf this is a first-time install, omit --re-pair.\n".utf8))
            throw ExitCode(3)
        } catch RepairerError.rotateRequestFailed(let piErr) {
            FileHandle.standardError.write(Data(
                "Rotate request failed: \(piErr)\n".utf8))
            switch piErr {
            case .authenticationRevoked:
                FileHandle.standardError.write(Data(
                    "The current api-key was rejected. Either the Pi has revoked this host (run `crm-mac uninstall --purge` and start fresh), or the local api-key file is out of sync with the Pi (try `crm-mac doctor`).\n".utf8))
            case .clientError(_, let code, _):
                switch code {
                case "INVALID_PAIRING_TOKEN":
                    FileHandle.standardError.write(Data(
                        "The pairing token was not recognized. Mint a fresh one from the Pi UI: Settings → Mac → Rotate Key.\n".utf8))
                case "TOKEN_ALREADY_USED":
                    FileHandle.standardError.write(Data(
                        "The pairing token has already been consumed. Mint a fresh one.\n".utf8))
                case "TOKEN_EXPIRED":
                    FileHandle.standardError.write(Data(
                        "The pairing token expired (10-minute TTL). Mint a fresh one.\n".utf8))
                case "STALE_AUTH":
                    FileHandle.standardError.write(Data(
                        "Another rotation committed before this one (api-key is now stale). The local api-key file is out of sync with the Pi; run `crm-mac doctor` to confirm.\n".utf8))
                case "ROUTE_NOT_FOUND":
                    FileHandle.standardError.write(Data(
                        "The Pi does not have the rotate-key endpoint. Upgrade the Pi backend, then retry.\n".utf8))
                case "NOT_FOUND":
                    FileHandle.standardError.write(Data(
                        "The Pi reports this host id is no longer active (likely revoked). Run `crm-mac uninstall --purge` and start fresh.\n".utf8))
                default:
                    break
                }
            default:
                break
            }
            throw ExitCode(1)
        } catch RepairerError.persistFailedAfterRotation(let underlying, let newKey) {
            // PRINT THE PLAINTEXT — this is the recovery prompt. The
            // operator HAS to see the key value because the daemon is
            // stranded (server-side rotation committed, but the new
            // key never reached disk).
            FileHandle.standardError.write(Data(
                """
                ROTATION SUCCEEDED SERVER-SIDE but local persistence failed (\(underlying)).
                The daemon will start returning 401 on its next heartbeat.

                Recovery:
                  1. Write the api-key below into \(ctx.paths.apiKeyFilePath) (mode 0600).
                  2. Run `crm-mac stop && crm-mac start` (NOT just `crm-mac start`) — the running daemon is still holding the now-revoked old key in memory and must restart to re-read the new key.
                If that fails, run `crm-mac install --re-pair --pair <NEW_TOKEN>` from a freshly-minted token (the current rotation already consumed the previous token).

                api-key: \(newKey)
                """.utf8))
            throw ExitCode(4)
        } catch RepairerError.responseDateParseFailed(let raw) {
            FileHandle.standardError.write(Data(
                "Pi response had unparseable api_key_rotated_at value: \(raw)\nThe new api-key was written to disk and the rotation likely succeeded server-side. Verify the daemon's next heartbeat returns 200 via `crm-mac status`.\n".utf8))
            throw ExitCode(6)
        }
    }
}
