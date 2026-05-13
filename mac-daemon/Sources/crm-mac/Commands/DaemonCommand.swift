// `crm-mac daemon` is the long-running entry point launchd invokes
// via the LaunchAgent plist. The branchy startup sequence (config /
// keychain / state load) lives in CRMMacLifecycle.DaemonStartup so
// it can be unit-tested without the CLI shim.
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

        let artifacts: DaemonStartupArtifacts
        do {
            artifacts = try DaemonStartup(
                paths: ctx.paths,
                keychain: ctx.keychain,
                logger: logger).run()
        } catch let startupErr as DaemonStartupError {
            throw ExitCode(startupErr.exitCode.rawValue)
        }

        let piClient = PiClient(baseURL: artifacts.config.piURL, logger: logger)
        let auth = PiAuth(hostID: artifacts.config.hostID, apiKey: artifacts.apiKey)
        let stateWriter = OnDiskHeartbeatStateWriter(
            stateStore: artifacts.stateStore,
            logger: logger)
        let heartbeat = HeartbeatLoop(
            piClient: piClient,
            auth: auth,
            stateWriter: stateWriter,
            exitHandler: ctx.exitHandler,
            logger: logger,
            clock: ctx.clock)

        let stubContext = SourceContext(logger: logger)
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

        // Block until SIGTERM / SIGINT. DispatchSource lets the actor
        // wake on signal delivery; the explicit SIG_IGN on each is
        // required so the default handler doesn't terminate the
        // process before our source observes it.
        let shutdownToken = ShutdownToken()
        let sigtermSource = DispatchSource.makeSignalSource(signal: SIGTERM, queue: .main)
        sigtermSource.setEventHandler { shutdownToken.signal() }
        sigtermSource.resume()
        let sigintSource = DispatchSource.makeSignalSource(signal: SIGINT, queue: .main)
        sigintSource.setEventHandler { shutdownToken.signal() }
        sigintSource.resume()
        signal(SIGTERM, SIG_IGN)
        signal(SIGINT, SIG_IGN)

        await runner.run(awaitShutdown: shutdownToken.wait)
    }
}
