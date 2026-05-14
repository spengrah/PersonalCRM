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

public final class HeartbeatLoop: SourcePlugin {
    public let id: SourceID = "_heartbeat"
    public let tickInterval: TimeInterval

    private let piClient: PiClient
    private let auth: PiAuth
    private let stateWriter: HeartbeatStateWriter
    private let exitHandler: ExitHandler
    private let logger: LoggerProtocol
    private let clock: ClockAdapter

    public init(
        piClient: PiClient,
        auth: PiAuth,
        stateWriter: HeartbeatStateWriter,
        exitHandler: ExitHandler,
        logger: LoggerProtocol,
        clock: ClockAdapter,
        tickInterval: TimeInterval = 60
    ) {
        self.piClient = piClient
        self.auth = auth
        self.stateWriter = stateWriter
        self.exitHandler = exitHandler
        self.logger = logger
        self.clock = clock
        self.tickInterval = tickInterval
    }

    public func tick() async throws {
        let body = HeartbeatBody(
            daemonVersion: Daemon.version,
            protocolVersion: Daemon.protocolVersion,
            permissions: Data("{}".utf8),
            sourceHealth: Data("{}".utf8))
        do {
            let result = try await piClient.heartbeat(auth: auth, body: body)
            try await stateWriter.recordSuccessfulHeartbeat(at: clock.now(), cursorEpoch: result.cursorEpoch)
            logger.debug("heartbeat ok", metadata: [
                "cursor_epoch": .public(String(result.cursorEpoch)),
            ])
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
/// Note: `async throws` because production impls funnel through the
/// `StateMutator` actor (introduced in PR7) to serialize writes with
/// other `state.json` writers (source plugins). Pre-PR7 the protocol
/// was synchronous and the actor did not exist.
public protocol HeartbeatStateWriter: Sendable {
    func recordSuccessfulHeartbeat(at: Date, cursorEpoch: Int64) async throws
}

/// No-op writer for tests / smoke that don't care about state.
public final class DiscardingHeartbeatStateWriter: HeartbeatStateWriter {
    public init() {}
    public func recordSuccessfulHeartbeat(at: Date, cursorEpoch: Int64) async throws {}
}
