// Repairer is the in-place pair-key rotation flow. Unlike Installer
// (which assembles a bundle, registers launchd, creates the mac_host
// row, initializes state) the Repairer ONLY:
//   - reads existing config + api-key
//   - calls POST /api/v1/host/:id/rotate-key with current creds + new
//     token
//   - atomically writes the new api-key to the existing file
//   - issues `launchctl kickstart -k gui/<uid>/<label>` (the launchd
//     plist sets KeepAlive={Crashed:true}, so a clean SIGTERM would
//     NOT respawn the daemon; kickstart -k is the documented restart
//     primitive that works regardless of KeepAlive policy)
//   - the respawned daemon re-reads the new api-key via
//     DaemonStartup.run
//
// The launchd registration, bundle, state.json, cursor state, and TCC
// grants are all left untouched — that's the entire point. The
// motivating UX problem is that `uninstall --purge` + re-install
// re-triggers every macOS-side approval dialog.
import Foundation
import CRMMacCore
import CRMMacPiClient

public struct RepairerDependencies {
    public let paths: LifecyclePaths
    public let keychain: KeychainStore
    public let configStoreFactory: @Sendable (URL) -> ConfigStore
    /// Matches Installer's signature: takes a `URL` (the
    /// `DaemonConfig.piURL` is typed as `URL`, not `String`).
    public let piClientFactory: @Sendable (URL) -> PiClient
    public let launchctl: LaunchctlRunner
    public let logger: LoggerProtocol

    public init(
        paths: LifecyclePaths,
        keychain: KeychainStore,
        configStoreFactory: @escaping @Sendable (URL) -> ConfigStore,
        piClientFactory: @escaping @Sendable (URL) -> PiClient,
        launchctl: LaunchctlRunner,
        logger: LoggerProtocol
    ) {
        self.paths = paths
        self.keychain = keychain
        self.configStoreFactory = configStoreFactory
        self.piClientFactory = piClientFactory
        self.launchctl = launchctl
        self.logger = logger
    }
}

public enum RepairerError: Error, CustomStringConvertible {
    case noExistingInstall(reason: String)
    case rotateRequestFailed(underlying: PiClientError)
    /// Persistent failure AFTER a successful server-side rotation.
    /// At this point the server has committed the new api-key hash
    /// and consumed the pairing token; the daemon's old key is
    /// invalid. The caller MUST surface `newPlaintextAPIKey` to the
    /// operator so they can recover by hand. Logs MUST NOT include
    /// the plaintext.
    case persistFailedAfterRotation(
        underlying: String,
        newPlaintextAPIKey: String)
    /// Date parsing failure on the server's `api_key_rotated_at`
    /// response field. Surfaces as a clear typed error rather than
    /// being silently swallowed. The api-key has already been written
    /// to disk at this point; only the response formatting failed.
    case responseDateParseFailed(raw: String)

    public var description: String {
        switch self {
        case .noExistingInstall(let r): return "no existing install: \(r)"
        case .rotateRequestFailed(let e): return "rotate request failed: \(e)"
        case .persistFailedAfterRotation:
            // Deliberately redact the plaintext — the CLI wrapper is
            // the only thing allowed to print it.
            return "persist failed AFTER server-side rotation; new key was lost (recovery path: re-pair from a fresh token)"
        case .responseDateParseFailed(let raw):
            return "response date parse failed: \(raw)"
        }
    }
}

public struct RepairResult: Equatable {
    public let hostID: UUID
    public let apiKeyRotatedAt: Date
    public let daemonRestartIssued: Bool
    /// Non-nil iff the kickstart attempt failed; carries a redacted
    /// description for the CLI wrapper to print. The rotation itself
    /// still succeeded; this only signals that the operator may need
    /// to run `crm-mac stop && crm-mac start` manually.
    public let restartWarning: String?

    public init(
        hostID: UUID,
        apiKeyRotatedAt: Date,
        daemonRestartIssued: Bool,
        restartWarning: String?
    ) {
        self.hostID = hostID
        self.apiKeyRotatedAt = apiKeyRotatedAt
        self.daemonRestartIssued = daemonRestartIssued
        self.restartWarning = restartWarning
    }
}

public struct Repairer {
    let deps: RepairerDependencies

    public init(_ deps: RepairerDependencies) {
        self.deps = deps
    }

    /// Run the re-pair flow.
    ///
    /// Preconditions:
    /// - `deps.paths.configFilePath` exists and is readable
    ///   (otherwise `RepairerError.noExistingInstall`).
    /// - `deps.keychain.readAPIKey()` returns a value (otherwise
    ///   `RepairerError.noExistingInstall`).
    public func run(newPairingToken: String) async throws -> RepairResult {
        // 1. Read existing config.
        let configStore = deps.configStoreFactory(
            URL(fileURLWithPath: deps.paths.configFilePath))
        let config: DaemonConfig
        do {
            config = try configStore.load()
        } catch {
            throw RepairerError.noExistingInstall(
                reason: "config.json missing or unreadable: \(error)")
        }

        // 2. Read existing api-key.
        let currentAPIKey: String
        do {
            currentAPIKey = try deps.keychain.readAPIKey()
        } catch {
            throw RepairerError.noExistingInstall(
                reason: "api-key file missing or unreadable: \(error)")
        }

        // 3. Call rotate endpoint with CURRENT creds + NEW token.
        let client = deps.piClientFactory(config.piURL)
        let currentAuth = PiAuth(hostID: config.hostID, apiKey: currentAPIKey)
        let rotated: RotateAPIKeyData
        do {
            rotated = try await client.rotateAPIKey(
                auth: currentAuth, newPairingToken: newPairingToken)
        } catch let piErr as PiClientError {
            throw RepairerError.rotateRequestFailed(underlying: piErr)
        } catch {
            throw RepairerError.rotateRequestFailed(
                underlying: .transport(underlying: String(describing: error)))
        }

        // 4. Persist new api-key. FileAPIKeyStore.writeAPIKey already
        //    does temp-file + rename, so a crash mid-write leaves the
        //    OLD key intact on disk. But the OLD key is invalid
        //    server-side now — if write fails, the daemon is stranded.
        //    Throw the typed error with the new plaintext so the CLI
        //    wrapper can print the recovery prompt.
        do {
            try deps.keychain.writeAPIKey(rotated.apiKey)
        } catch {
            throw RepairerError.persistFailedAfterRotation(
                underlying: String(describing: error),
                newPlaintextAPIKey: rotated.apiKey)
        }

        // 5. Parse the response date BEFORE restarting the daemon.
        //    A parse failure surfaces as RepairerError.responseDateParseFailed
        //    and skips the kickstart step — the daemon may need to
        //    stay up so the operator can investigate before restart,
        //    and kicking it would mask the issue.
        let parsedRotatedAt: Date
        let isoFormatter = ISO8601DateFormatter()
        isoFormatter.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        if let d = isoFormatter.date(from: rotated.apiKeyRotatedAt) {
            parsedRotatedAt = d
        } else {
            // Try without fractional seconds (Go's time.Time may emit
            // either form depending on monotonic-clock state).
            let fallback = ISO8601DateFormatter()
            fallback.formatOptions = [.withInternetDateTime]
            if let d = fallback.date(from: rotated.apiKeyRotatedAt) {
                parsedRotatedAt = d
            } else {
                throw RepairerError.responseDateParseFailed(raw: rotated.apiKeyRotatedAt)
            }
        }

        // 6. Restart the daemon via `launchctl kickstart -k`. Cannot
        //    use a clean SIGTERM because the launchd plist sets
        //    KeepAlive={Crashed:true} — clean exits are NOT respawned.
        //    `kickstart -k` is launchctl's documented kill-and-restart
        //    primitive and works regardless of KeepAlive policy.
        //
        //    Failure is non-fatal: the rotation already committed and
        //    the new api-key is on disk. If kickstart fails (e.g.
        //    service not registered, launchctl returned non-zero),
        //    surface a warning so the CLI wrapper can print clear
        //    operator-side remediation (`crm-mac stop && crm-mac
        //    start` — a stop+start cycle is required because the
        //    running daemon has the old key cached in memory).
        var restartIssued = false
        var restartWarning: String?
        do {
            let invocation = try deps.launchctl.kickstart(label: Daemon.label)
            if invocation.exitCode == 0 {
                restartIssued = true
            } else {
                restartWarning = "launchctl kickstart exit=\(invocation.exitCode), stderr=\(invocation.stderr)"
                deps.logger.warning("re-pair: kickstart returned non-zero", metadata: [
                    "exit_code": .public("\(invocation.exitCode)"),
                    "stderr": .private(invocation.stderr),
                ])
            }
        } catch {
            restartWarning = "launchctl kickstart threw: \(error)"
            deps.logger.warning("re-pair: kickstart threw", metadata: [
                "error": .private(String(describing: error)),
            ])
        }

        deps.logger.info("re-pair: complete", metadata: [
            "host_id": .private(config.hostID.uuidString),
            "restart_issued": .public("\(restartIssued)"),
        ])

        return RepairResult(
            hostID: config.hostID,
            apiKeyRotatedAt: parsedRotatedAt,
            daemonRestartIssued: restartIssued,
            restartWarning: restartWarning)
    }
}
