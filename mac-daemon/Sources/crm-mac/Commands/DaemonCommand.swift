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
import CRMMacMessagesSource
import CRMMacPiClient
import CRMMacSystem

struct DaemonCommand: AsyncParsableCommand {
    static let configuration = CommandConfiguration(
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

        // PidfileLock — defense-in-depth + serializes with CLI ops.
        let pidfileURL = URL(fileURLWithPath: ctx.paths.runtimeDirPath)
            .appendingPathComponent("daemon.pid")
        let pidfileLock = PidfileLock(path: pidfileURL)
        do {
            try pidfileLock.acquire()
        } catch let err as PidfileError {
            FileHandle.standardError.write(Data("daemon already running: \(err.description)\n".utf8))
            throw ExitCode(6)
        }
        // Best-effort release on shutdown. The defer here runs even on
        // throw paths after this point.
        defer { pidfileLock.release() }

        let piClient = PiClient(baseURL: artifacts.config.piURL, logger: logger)
        let auth = PiAuth(hostID: artifacts.config.hostID, apiKey: artifacts.apiKey)
        let stateMutator = StateMutator(store: artifacts.stateStore)
        let stateWriter = OnDiskHeartbeatStateWriter(
            mutator: stateMutator,
            logger: logger)

        // Shared source-health registry: the messages plugin writes to
        // it after every tick; the heartbeat reads from it to populate
        // source_health in the heartbeat body.
        let healthRegistry = SourceHealthRegistry()

        // Messages source: real chat.db reader + cursor commit + publish.
        let chatDBPath = URL(fileURLWithPath: NSHomeDirectory())
            .appendingPathComponent("Library/Messages/chat.db")
        let backfillFloor = MessagesCursorWire.defaultBackfillFloor
        let knownIdentifiersCache = KnownIdentifiersCache()
        let publisher = MessagesPublisher(
            sender: { [piClient] auth, body in
                try await piClient.ingestEvents(auth: auth, body: body)
            },
            auth: auth,
            logger: logger)
        let messagesPlugin = MessagesSourcePlugin(
            tickInterval: 60,
            config: MessagesSourceConfig(
                chatDBPath: chatDBPath,
                backfillFloor: backfillFloor),
            piClient: piClient,
            auth: auth,
            mutator: stateMutator,
            publisher: publisher,
            cache: knownIdentifiersCache,
            healthRegistry: healthRegistry,
            logger: logger)

        // KnownIdentifiers refresher: after each heartbeat, fetch the
        // canonical phone+email set + replace the cache.
        let refresher: KnownIdentifiersRefresher = { [piClient] in
            let result = try await piClient.knownIdentifiers(auth: auth)
            var canonical: Set<String> = []
            for phone in result.phones {
                let n = HandleNormalization.canonicalize(phone)
                if !n.isEmpty { canonical.insert(n) }
            }
            for email in result.emails {
                let n = HandleNormalization.canonicalize(email)
                if !n.isEmpty { canonical.insert(n) }
            }
            _ = await knownIdentifiersCache.replace(with: canonical)
        }

        let healthProvider = RegistryHealthProvider(
            registry: healthRegistry, logger: logger)
        let heartbeat = HeartbeatLoop(
            piClient: piClient,
            auth: auth,
            stateWriter: stateWriter,
            exitHandler: ctx.exitHandler,
            logger: logger,
            clock: ctx.clock,
            refresher: refresher,
            sourceHealthProvider: healthProvider)

        let stubContext = SourceContext(logger: logger)
        let plugins: [SourcePlugin] = [
            messagesPlugin,
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
        let shutdownSignal = ShutdownSignal()
        let sigtermSource = DispatchSource.makeSignalSource(signal: SIGTERM, queue: .main)
        sigtermSource.setEventHandler { shutdownSignal.signal() }
        sigtermSource.resume()
        let sigintSource = DispatchSource.makeSignalSource(signal: SIGINT, queue: .main)
        sigintSource.setEventHandler { shutdownSignal.signal() }
        sigintSource.resume()
        signal(SIGTERM, SIG_IGN)
        signal(SIGINT, SIG_IGN)

        await runner.run(awaitShutdown: shutdownSignal.wait)
    }
}
