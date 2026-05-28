// HeartbeatLoop drives the daemon's ~60s heartbeat tick. Wraps a
// SourcePlugin so the registry can schedule it via the same
// ScheduleRunner the source plugins use.
//
// Behavior:
//   - 60s tick by default; one in-flight at a time (plugin tick
//     awaits before scheduler considers the activity done).
//   - 401 → request exit 1 (auth revoked).
//   - 412 → request exit 2 (upgrade required).
//   - Other errors → log + continue (next tick at +60s).
//   - RetryingTransport handles 5xx retries WITHIN a single tick;
//     the 60s scheduler tick handles inter-request spacing.
import Foundation
import CRMMacCore
import CRMMacPiClient

/// Closure that the heartbeat loop invokes after each successful
/// heartbeat to refresh per-source caches with the latest
/// known-identifiers fetch.  Production wiring: the closure calls
/// piClient.knownIdentifiers + feeds the canonicalized set into the
/// messages plugin's KnownIdentifiersCache.replace(with:). Tests inject
/// no-op or recording closures.
///
/// Errors are caught + logged inside the heartbeat tick; refresher
/// failures must not propagate as heartbeat failures.
public typealias KnownIdentifiersRefresher = @Sendable () async throws -> Void

public final class HeartbeatLoop: SourcePlugin {
    public let id: SourceID = "_heartbeat"
    public let tickInterval: TimeInterval

    private let piClient: PiClient
    private let auth: PiAuth
    private let stateWriter: HeartbeatStateWriter
    private let exitHandler: ExitHandler
    private let logger: LoggerProtocol
    private let clock: ClockAdapter
    private let refresher: KnownIdentifiersRefresher?
    private let sourceHealthProvider: SourceHealthProvider?
    private let firstSuccessLatch: FirstSuccessLatch?

    public init(
        piClient: PiClient,
        auth: PiAuth,
        stateWriter: HeartbeatStateWriter,
        exitHandler: ExitHandler,
        logger: LoggerProtocol,
        clock: ClockAdapter,
        tickInterval: TimeInterval = 60,
        refresher: KnownIdentifiersRefresher? = nil,
        sourceHealthProvider: SourceHealthProvider? = nil,
        firstSuccessLatch: FirstSuccessLatch? = nil
    ) {
        self.piClient = piClient
        self.auth = auth
        self.stateWriter = stateWriter
        self.exitHandler = exitHandler
        self.logger = logger
        self.clock = clock
        self.tickInterval = tickInterval
        self.refresher = refresher
        self.sourceHealthProvider = sourceHealthProvider
        self.firstSuccessLatch = firstSuccessLatch
    }

    public func tick() async throws {
        let sourceHealthData = await buildSourceHealthData()
        let body = HeartbeatBody(
            daemonVersion: Daemon.version,
            protocolVersion: Daemon.protocolVersion,
            permissions: Data("{}".utf8),
            sourceHealth: sourceHealthData)
        do {
            let result = try await piClient.heartbeat(auth: auth, body: body)
            try await stateWriter.recordSuccessfulHeartbeat(
                at: clock.now(),
                cursorEpoch: result.cursorEpoch,
                protocolVersion: result.protocolVersion)
            logger.debug("heartbeat ok", metadata: [
                "cursor_epoch": .public(String(result.cursorEpoch)),
                "pi_protocol_version": .public(String(result.protocolVersion)),
            ])
            // Refresh known-identifiers after a successful heartbeat.
            // Errors here MUST NOT propagate as heartbeat failures —
            // log and continue.
            if let refresher {
                do {
                    try await refresher()
                } catch {
                    logger.warning("known-identifiers refresh failed", metadata: [
                        "error": .private(String(describing: error)),
                    ])
                }
            }
            // Fire the first-success latch (dedup internally). The
            // latch's callback runs exactly once across the daemon's
            // lifetime — the orphan-notification subsystem uses
            // this to trigger reconcile() at startup once the Pi
            // is proven reachable. Fire-and-forget via a Task so
            // a slow callback doesn't stall the heartbeat.
            if let latch = firstSuccessLatch {
                Task { await latch.fireOnce() }
            }
        } catch PiClientError.authenticationRevoked(let message) {
            logger.error("heartbeat: 401 — exiting", metadata: ["message": .public(message)])
            try exitHandler.requestExit(1)
        } catch let PiClientError.upgradeRequired(minVersion, message) {
            logger.error("heartbeat: 412 — exiting", metadata: [
                "min_version": .public(minVersion.map(String.init) ?? "unknown"),
                "message": .public(message),
            ])
            try exitHandler.requestExit(2)
        } catch {
            // 5xx after retries, transport, or other transient — log
            // and continue.
            logger.warning("heartbeat: transient failure", metadata: [
                "error": .private(String(describing: error)),
            ])
        }
    }
}

/// Hook the heartbeat loop calls after a successful tick. The
/// production composition root provides an impl that updates the
/// state.json `lastHeartbeatAt` field; tests inject a recording fake.
///
/// `async throws` because production impls funnel through the
/// `StateMutator` actor to serialize writes with other `state.json`
/// writers (source plugins).
///
/// `protocolVersion` is the Pi-reported `protocol_version` from the
/// heartbeat response. Production impls persist it into
/// `DaemonState.lastKnownPiProtocolVersion` so source plugins can
/// feature-gate themselves against older Pi instances (e.g. the
/// phone_calls source requires Pi protocol_version >= 2).
public protocol HeartbeatStateWriter: Sendable {
    func recordSuccessfulHeartbeat(
        at: Date,
        cursorEpoch: Int64,
        protocolVersion: Int32
    ) async throws
}

/// No-op writer for tests / smoke that don't care about state.
public final class DiscardingHeartbeatStateWriter: HeartbeatStateWriter {
    public init() {}
    public func recordSuccessfulHeartbeat(
        at: Date,
        cursorEpoch: Int64,
        protocolVersion: Int32
    ) async throws {}
}

/// Source-health JSON builder for the heartbeat body.  Production
/// wiring reads SourceHealthRegistry.all() and serializes the result;
/// tests inject a static / recording impl.
public protocol SourceHealthProvider: Sendable {
    /// Returns the JSON bytes to include in the heartbeat body's
    /// `source_health` field.  Must always return a valid JSON object
    /// (at minimum `{}`).
    func currentBody() async -> Data
}

/// Bridges CRMMacCore.SourceHealthRegistry into the heartbeat body
/// builder. Reads the latest snapshot for every registered source +
/// emits a JSON object keyed by source id.
public final class RegistryHealthProvider: SourceHealthProvider {
    private let registry: SourceHealthRegistry
    private let logger: LoggerProtocol

    public init(registry: SourceHealthRegistry, logger: LoggerProtocol) {
        self.registry = registry
        self.logger = logger
    }

    public func currentBody() async -> Data {
        let snapshots = await registry.all()
        var keyed: [String: SourceHealthSnapshot] = [:]
        for (id, snap) in snapshots {
            keyed[id.rawValue] = snap
        }
        let encoder = JSONEncoder()
        encoder.dateEncodingStrategy = .iso8601
        encoder.outputFormatting = [.sortedKeys]
        do {
            return try encoder.encode(keyed)
        } catch {
            logger.warning("source health body encode failed", metadata: [
                "error": .private(String(describing: error)),
            ])
            return Data("{}".utf8)
        }
    }
}

private extension HeartbeatLoop {
    func buildSourceHealthData() async -> Data {
        if let provider = sourceHealthProvider {
            return await provider.currentBody()
        }
        return Data("{}".utf8)
    }
}
