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
import CRMMacAnarlogSource
import CRMMacCore
import CRMMacIcloudContactsSource
import CRMMacLifecycle
import CRMMacMessagesSource
import CRMMacOrphanNotifications
import CRMMacPhoneCallsSource
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

        // Known-identifiers cache, shared by messages + phone_calls.
        // Seed each consumer's DIFF baseline from the persisted
        // DaemonState.knownIdentifierBaselines so an offline-restart
        // diff enqueues 30-day scans ONLY for genuine additions. A
        // source ABSENT from the persisted map starts `noBaseline`
        // (upgrade boundary → seed-on-first-fetch, no scans). The
        // refresher is the SOLE writer of the persisted baseline.
        let knownIdentifierConsumers: Set<SourceID> = [.messages, .phoneCalls]
        var seededBaselines: [SourceID: Set<String>] = [:]
        if let persisted = (try? await stateMutator.read())?.knownIdentifierBaselines {
            for (key, value) in persisted {
                seededBaselines[SourceID(rawValue: key)] = value.toSet()
            }
        }
        let knownIdentifiersCache = KnownIdentifiersCache(
            baselines: seededBaselines,
            consumers: knownIdentifierConsumers)
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
        // canonical phone+email set + replace the cache. The refresher
        // is ALSO the SOLE writer of the persisted per-source baseline:
        // after replace, it reads each source's `persistableBaseline`
        // (the observed set MINUS identifiers still owed a durable scan
        // enqueue) and plain-assigns it to
        // DaemonState.knownIdentifierBaselines when it differs. The
        // owed-scan exclusion guarantees the persisted baseline never
        // advances past an identifier whose 30-day scan isn't durable
        // yet. The source tick NEVER writes this field.
        let baselineClock = ctx.clock
        let refresher: KnownIdentifiersRefresher = { [piClient, stateMutator, logger, baselineClock] in
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
            await knownIdentifiersCache.replace(with: canonical)

            // Persist each consumer's writable baseline (single writer).
            for consumer in knownIdentifierConsumers {
                // Capture the immutable observed set OUTSIDE the
                // synchronous mutate closure (no actor read in-closure).
                guard let observed = await knownIdentifiersCache.persistableBaseline(for: consumer) else {
                    continue
                }
                let key = consumer.rawValue
                let sorted = observed.sorted()
                do {
                    try await stateMutator.mutate { state in
                        var map = state.knownIdentifierBaselines ?? [:]
                        let existing = map[key]
                        // No-op when unchanged (idempotent).
                        if existing?.canonical == sorted { return }
                        map[key] = KnownIdentifiersBaseline(
                            canonical: sorted,
                            // Preserve the first-seed time across writes.
                            establishedAt: existing?.establishedAt ?? baselineClock.now())
                        state.knownIdentifierBaselines = map
                    }
                } catch {
                    logger.warning("known-identifiers baseline persist failed", metadata: [
                        "source": .public(key),
                        "error": .private(String(describing: error)),
                    ])
                }
            }
        }

        let healthProvider = RegistryHealthProvider(
            registry: healthRegistry, logger: logger)
        // HeartbeatLoop is constructed below — after the orphan
        // notification center exists — so the FirstSuccessLatch can
        // capture the center for the startup-reconcile callback.

        // iCloud Contacts source. Reads CNContactStore on the host
        // (Contacts permission required), publishes external_contact.*
        // events to the Pi, persists the per-contact content-hash
        // cache locally for deterministic delete source_ids.
        let icloudCachePath = URL(fileURLWithPath: ctx.paths.configDirPath)
            .appendingPathComponent("icloud_contacts_hashes.json")
        let icloudCache = ContactHashCache(fileURL: icloudCachePath)
        do {
            try await icloudCache.load()
        } catch {
            logger.warning("icloud cache load failed; treating as empty", metadata: [
                "error": .private(String(describing: error)),
            ])
        }
        let icloudPublisher = ICloudContactsPublisher(
            sender: { [piClient] auth, body in
                try await piClient.ingestEvents(auth: auth, body: body)
            },
            auth: auth,
            logger: logger)
        let configStore = CRMMacCore.ConfigStore(
            fileURL: URL(fileURLWithPath: ctx.paths.configFilePath))
        let icloudPlugin = ICloudContactsSourcePlugin(
            tickInterval: CRMMacIcloudContactsSource.defaultTickInterval,
            piClient: piClient,
            auth: auth,
            mutator: stateMutator,
            publisher: icloudPublisher,
            cache: icloudCache,
            reader: CNContactStoreReader(),
            authAdapter: ctx.contactsAuthAdapter(),
            configSource: ICloudContactsConfigStoreSource(store: configStore),
            healthRegistry: healthRegistry,
            logger: logger)

        // Anarlog reader sources. Both plugins share the AnarlogConfig
        // slot in config.json; both default-disabled until the
        // operator runs `crm-mac configure anarlog --enable …`.
        // The sessions plugin also gets an FSEvents watcher that
        // fires its tick() when ANY file under the configured
        // sessions/ directory changes; the watcher's start() is gated
        // on the config being present + sessions enabled at startup.
        let anarlogFilesystem = ProductionAnarlogFilesystem()
        let anarlogConfigSource = AnarlogConfigStoreSource(store: configStore)
        let anarlogHumansPublisher = AnarlogHumansPublisher(
            sender: { [piClient] auth, body in
                try await piClient.ingestEvents(auth: auth, body: body)
            },
            auth: auth, logger: logger)
        let anarlogHumansPlugin = AnarlogHumansSourcePlugin(
            piClient: piClient,
            auth: auth,
            mutator: stateMutator,
            publisher: anarlogHumansPublisher,
            filesystem: anarlogFilesystem,
            configSource: anarlogConfigSource,
            healthRegistry: healthRegistry,
            logger: logger)
        let anarlogSessionsPublisher = AnarlogSessionsPublisher(
            sender: { [piClient] auth, body in
                try await piClient.ingestEvents(auth: auth, body: body)
            },
            auth: auth, logger: logger)

        // Orphan-notification subsystem. Wraps the macOS
        // UNUserNotificationCenter; raises persistent alerts when
        // the Pi flags a session as orphan / conflict-pending, and
        // clears them when reconciliation confirms the session has
        // exited the needs-attention queue.
        let orphanPresenter = UserNotificationCenterPresenter()
        let orphanOpener = NSWorkspaceOpener()
        let orphanMetadataLookup = AnarlogSessionMetadataLookup(
            configSource: anarlogConfigSource,
            filesystem: anarlogFilesystem)
        let needsAttentionFetcher: NeedsAttentionFetcher = { [piClient, auth] in
            // Map the PiClient transport DTO to the notification
            // module's domain type at this composition boundary
            // — CRMMacOrphanNotifications doesn't import
            // CRMMacPiClient.
            let wire = try await piClient.needsAttention(auth: auth, hostID: auth.hostID)
            return wire.map { row in
                NotificationReconcileItem(
                    anarlogSessionID: row.anarlogSessionID.uuidString.lowercased(),
                    linkageState: row.linkageState,
                    title: row.title,
                    meetingAt: row.meetingAt)
            }
        }
        let orphanClock = ctx.clock
        let orphanNotificationCenter = OrphanNotificationCenter(
            presenter: orphanPresenter,
            opener: orphanOpener,
            mutator: stateMutator,
            metadataLookup: orphanMetadataLookup,
            piURL: artifacts.config.piURL,
            needsAttentionFetcher: needsAttentionFetcher,
            logger: logger,
            clock: { orphanClock.now() })
        await orphanNotificationCenter.installDelegate()
        // One-time cleanup of any unversioned notifications minted
        // by older daemon builds. Operators upgrading otherwise see
        // ghost notifications the new code can't track.
        await orphanNotificationCenter.cleanupLegacyOSNotifications()
        let reconcileLoopPlugin = NotificationReconcileLoopPlugin(
            center: orphanNotificationCenter,
            logger: logger)

        let anarlogSessionsPlugin = AnarlogSessionsSourcePlugin(
            piClient: piClient,
            auth: auth,
            mutator: stateMutator,
            publisher: anarlogSessionsPublisher,
            filesystem: anarlogFilesystem,
            configSource: anarlogConfigSource,
            healthRegistry: healthRegistry,
            orphanNotificationCenter: orphanNotificationCenter,
            logger: logger)

        // FirstSuccessLatch: fires the orphan center's reconcile()
        // once after the daemon's first successful heartbeat. The
        // 300s NotificationReconcileLoopPlugin handles steady-state
        // drift; this latch handles startup recovery for sessions
        // that arrived while the daemon was offline.
        let orphanStartupReconcileLatch = FirstSuccessLatch {
            await orphanNotificationCenter.reconcile()
        }

        let heartbeat = HeartbeatLoop(
            piClient: piClient,
            auth: auth,
            stateWriter: stateWriter,
            exitHandler: ctx.exitHandler,
            logger: logger,
            clock: ctx.clock,
            refresher: refresher,
            sourceHealthProvider: healthProvider,
            firstSuccessLatch: orphanStartupReconcileLatch)

        // FSEvents watcher for the sessions plugin. We start the
        // watcher only when the config is loadable + sessions is
        // enabled; a runtime config change (operator runs `configure
        // anarlog --enable sessions` while daemon is running) is
        // refused by the configure command (requireDaemonNotRunning)
        // so a stop+start cycle is required to pick up new state.
        var sessionsWatcher: AnarlogFSEventsWatcher?
        if let cfg = try? configStore.loadAnarlogConfig(), cfg.sessionsEnabled {
            let sessionsPath = AnarlogPathResolver
                .sessionsDir(rootPath: cfg.rootPath).path
            let watcher = AnarlogFSEventsWatcher(
                path: sessionsPath,
                logger: logger,
                onChange: { [weak anarlogSessionsPlugin, weak logger] in
                    // Wrap in do/catch so a thrown error from tick()
                    // is logged rather than silently swallowed
                    // (silent FSEvents-trigger error swallowing
                    // would otherwise hide tick failures from the log).
                    Task {
                        do {
                            try await anarlogSessionsPlugin?.tick()
                        } catch {
                            logger?.warning(
                                "anarlog_sessions: FSEvents-triggered tick failed",
                                metadata: [
                                    "error": .private(String(describing: error)),
                                ])
                        }
                    }
                })
            do {
                try watcher.start()
                sessionsWatcher = watcher
            } catch {
                logger.warning("anarlog_sessions: FSEvents start failed", metadata: [
                    "error": .private(String(describing: error)),
                ])
            }
        }

        // phone_calls source: CallHistoryDB reader + push provider.
        // Feature-gated against the Pi's protocol_version via
        // HeartbeatStateProvider — the plugin self-disables when the
        // Pi reports protocol_version < phone_calls's required
        // minimum.
        let callHistoryDBPath = URL(fileURLWithPath: NSHomeDirectory())
            .appendingPathComponent(
                "Library/Application Support/CallHistoryDB/CallHistory.storedata")
        let phoneCallsPublisher = PhoneCallsPublisher(
            sender: { [piClient] auth, body in
                try await piClient.ingestEvents(auth: auth, body: body)
            },
            auth: auth,
            logger: logger)
        let heartbeatStateProvider = StateMutatorHeartbeatStateProvider(mutator: stateMutator)
        let phoneCallsPlugin = PhoneCallsSourcePlugin(
            tickInterval: 60,
            config: PhoneCallsSourceConfig(
                callHistoryDBPath: callHistoryDBPath,
                backfillFloor: PhoneCallsCursorWire.defaultBackfillFloor),
            piClient: piClient,
            auth: auth,
            mutator: stateMutator,
            publisher: phoneCallsPublisher,
            cache: knownIdentifiersCache,
            canonicalizer: { HandleNormalization.canonicalize($0) },
            heartbeatStateProvider: heartbeatStateProvider,
            healthRegistry: healthRegistry,
            logger: logger)


        let plugins: [SourcePlugin] = [
            messagesPlugin,
            phoneCallsPlugin,
            icloudPlugin,
            anarlogHumansPlugin,
            anarlogSessionsPlugin,
            reconcileLoopPlugin,
        ]
        let scheduler = DispatchSourceScheduleRunner(logger: logger)
        // preShutdown hook stops the FSEvents watcher before plugins
        // are cancelled — without this ordering a late FSEvents
        // callback can fire into a half-cancelled sessions actor.
        let runner = DaemonRunner(
            heartbeat: heartbeat,
            plugins: plugins,
            runner: scheduler,
            logger: logger,
            preShutdown: { [sessionsWatcher] in
                sessionsWatcher?.stop()
            })

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
