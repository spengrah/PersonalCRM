import Foundation
import CRMMacCore
import CRMMacOrphanNotifications
import CRMMacPiClient

/// The one way to build Anarlog source plugins outside this module: both share
/// one health notifier and read Anarlog through the production CLI client.
public struct AnarlogSourcePlugins: Sendable {
    public let humans: AnarlogHumansSourcePlugin
    public let sessions: AnarlogSessionsSourcePlugin

    public init(
        piClient: PiClient,
        auth: PiAuth,
        mutator: StateMutator,
        humansPublisher: AnarlogHumansPublisher,
        sessionsPublisher: AnarlogSessionsPublisher,
        configSource: AnarlogConfigSource,
        healthRegistry: SourceHealthRegistry,
        orphanNotificationCenter: OrphanNotificationCenter?,
        presenter: UserNotificationPresenter,
        logger: LoggerProtocol,
        clock: @escaping @Sendable () -> Date
    ) {
        let healthNotifier = AnarlogHealthNotifier(
            presenter: presenter, logger: logger, clock: clock)
        self.humans = AnarlogHumansSourcePlugin(
            piClient: piClient,
            auth: auth,
            mutator: mutator,
            publisher: humansPublisher,
            configSource: configSource,
            healthSink: healthNotifier,
            healthRegistry: healthRegistry,
            logger: logger)
        self.sessions = AnarlogSessionsSourcePlugin(
            piClient: piClient,
            auth: auth,
            mutator: mutator,
            publisher: sessionsPublisher,
            configSource: configSource,
            healthSink: healthNotifier,
            healthRegistry: healthRegistry,
            orphanNotificationCenter: orphanNotificationCenter,
            logger: logger)
    }
}
