// `crm-mac daemon` is the long-running entry point launchd invokes
// via the LaunchAgent plist. It:
//   1. Loads config + Keychain api-key.
//   2. Composes the heartbeat loop + stub source plugins.
//   3. Awaits SIGTERM (delivered by `launchctl bootout`).
//
// Exit-code map:
//   - clean shutdown: 0
//   - 401 from Pi during heartbeat: 1 (via ExitHandler)
//   - 412 from Pi during heartbeat: 2 (via ExitHandler)
//   - config missing/corrupt: 3
//   - Keychain entry missing/access denied: 4
//   - state.json missing/corrupt/schema mismatch: 5
import Foundation
import ArgumentParser
import CRMMacCore
import CRMMacLifecycle
import CRMMacPiClient
import CRMMacSystem

struct DaemonCommand: AsyncParsableCommand {
    static var configuration = CommandConfiguration(
        commandName: "daemon",
        abstract: "Long-running launchd-managed daemon. Don't invoke directly — use `install` first.")

    mutating func run() async throws {
        let ctx = ProductionContext()
        let logger = ctx.logger

        // Load config (exit code 3 on failure).
        let configStore = ConfigStore(fileURL: URL(fileURLWithPath: ctx.paths.configFilePath))
        let config: DaemonConfig
        do {
            config = try configStore.load()
        } catch {
            logger.error("daemon: config load failed", metadata: [
                "error": .private(String(describing: error)),
            ])
            throw ExitCode(3)
        }

        // Load Keychain API key (exit code 4 on failure).
        let apiKey: String
        do {
            apiKey = try ctx.keychain.readAPIKey()
        } catch {
            logger.error("daemon: keychain load failed", metadata: [
                "error": .private(String(describing: error)),
            ])
            throw ExitCode(4)
        }

        // Load state.json (exit code 5 on failure).
        let stateStore = StateStore(fileURL: URL(fileURLWithPath: ctx.paths.stateFilePath))
        do {
            _ = try stateStore.load()
        } catch {
            logger.error("daemon: state load failed", metadata: [
                "error": .private(String(describing: error)),
            ])
            throw ExitCode(5)
        }

        let piClient = PiClient(baseURL: config.piURL, logger: logger)
        let auth = PiAuth(hostID: config.hostID, apiKey: apiKey)
        let stateWriter = OnDiskHeartbeatStateWriter(stateStore: stateStore, logger: logger)
        let heartbeat = HeartbeatLoop(
            piClient: piClient,
            auth: auth,
            stateWriter: stateWriter,
            exitHandler: ctx.exitHandler,
            logger: logger,
            clock: ctx.clock)

        let stubLogger = logger
        let stubContext = SourceContext(logger: stubLogger)
        let plugins: [SourcePlugin] = [
            StubMessagesPlugin(context: stubContext),
            StubICloudContactsPlugin(context: stubContext),
        ]
        let scheduler = NSBackgroundActivityScheduleRunner(logger: logger)
        let runner = DaemonRunner(
            heartbeat: heartbeat,
            plugins: plugins,
            runner: scheduler,
            logger: logger)

        // Block until SIGTERM. signal(SIGTERM, ...) is installed via
        // DispatchSource so we don't override Swift's default handler.
        let shutdownToken = ShutdownToken()
        let sigtermSource = DispatchSource.makeSignalSource(signal: SIGTERM, queue: .main)
        sigtermSource.setEventHandler { shutdownToken.signal() }
        sigtermSource.resume()
        // SIGINT for interactive debugging.
        let sigintSource = DispatchSource.makeSignalSource(signal: SIGINT, queue: .main)
        sigintSource.setEventHandler { shutdownToken.signal() }
        sigintSource.resume()
        // Suppress default SIGTERM/SIGINT termination so DispatchSource sees them.
        signal(SIGTERM, SIG_IGN)
        signal(SIGINT, SIG_IGN)

        await runner.run(awaitShutdown: shutdownToken.wait)
    }
}

/// Shutdown signaler. The daemon parks on `await wait()`; signal()
/// resolves the continuation.
private actor ShutdownToken {
    private var continuation: CheckedContinuation<Void, Never>?
    private var signalled = false

    nonisolated func signal() {
        Task { await self.deliver() }
    }

    private func deliver() {
        signalled = true
        continuation?.resume()
        continuation = nil
    }

    nonisolated func wait() async {
        await waitInternal()
    }

    private func waitInternal() async {
        if signalled { return }
        await withCheckedContinuation { (c: CheckedContinuation<Void, Never>) in
            self.continuation = c
        }
    }
}

/// Writes lastHeartbeatAt to state.json on every successful tick.
/// Best-effort — failures are logged but not surfaced.
private final class OnDiskHeartbeatStateWriter: HeartbeatStateWriter {
    private let stateStore: StateStore
    private let logger: LoggerProtocol

    init(stateStore: StateStore, logger: LoggerProtocol) {
        self.stateStore = stateStore
        self.logger = logger
    }

    func recordSuccessfulHeartbeat(at: Date, cursorEpoch: Int64) {
        do {
            var state = try stateStore.load()
            state.lastHeartbeatAt = at
            try stateStore.save(state)
        } catch {
            logger.warning("heartbeat: state persist failed", metadata: [
                "error": .private(String(describing: error)),
            ])
        }
    }
}
