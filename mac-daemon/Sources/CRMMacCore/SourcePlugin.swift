// SourcePlugin is the contract every per-source poller satisfies.
// Real implementations live in their own targets
// (CRMMacMessagesSource.MessagesSourcePlugin,
// CRMMacIcloudContactsSource.ICloudContactsSourcePlugin).
//
// The contract is intentionally tiny — the scheduler is owned by
// CRMMacLifecycle, not by the plugins themselves, so plugins remain
// passive callees that get a SourceContext and do one tick of work.
import Foundation

/// Stable identifier for a source. Persisted in `state.json` under
/// `sources[<id>]` and used in heartbeat `source_health` payloads.
public struct SourceID: RawRepresentable, Hashable, Codable, ExpressibleByStringLiteral, Sendable {
    public let rawValue: String

    public init(rawValue: String) {
        self.rawValue = rawValue
    }

    public init(stringLiteral value: String) {
        self.rawValue = value
    }

    public static let messages: SourceID = "messages"
    public static let icloudContacts: SourceID = "icloud_contacts"
    public static let anarlogHumans: SourceID = "anarlog_humans"
    public static let anarlogSessions: SourceID = "anarlog_sessions"
    public static let phoneCalls: SourceID = "phone_calls"
    /// Periodic poll of /meeting-notes/needs-attention that
    /// reconciles the daemon's local pending-notification table
    /// against the Pi's authoritative set.
    public static let notificationReconcile: SourceID = "notification_reconcile"
}

/// A poller of one external source. The stub implementations log a
/// no-op tick; real readers replace them.
///
/// `Sendable` constraint: `PluginRegistry` reads `id`/`tickInterval` from
/// arbitrary contexts and the scheduler runners spawn a `Task` to invoke
/// `tick()`. Existing class-based conformers (`HeartbeatLoop`) must be
/// safe to share across actor boundaries — they are: stored state is
/// either `let` plus injected protocol-typed collaborators (themselves
/// `Sendable`) or guarded via the `StateMutator` actor introduced when
/// source plugins began writing state. Actor-based conformers like
/// `MessagesSourcePlugin` and `ICloudContactsSourcePlugin` are
/// automatically `Sendable`.
public protocol SourcePlugin: AnyObject, Sendable {
    /// Stable source identifier; used as the state-file key and
    /// heartbeat-payload key.
    var id: SourceID { get }

    /// Schedule cadence the registry should request from ScheduleRunner.
    /// Stubs ask for 60s; real readers tune this themselves.
    var tickInterval: TimeInterval { get }

    /// Called by the scheduler on every tick. Stubs log + return.
    /// Errors are caught and logged by the caller — plugins don't need
    /// to handle their own retry envelope.
    func tick() async throws
}

/// A SourcePlugin that tails an external data source and must persist a
/// per-tick liveness heartbeat to `state.sources[id].lastScheduledAt`.
///
/// The protocol extension below provides a `tick()` default so
/// individual data plugins CANNOT forget the state.json heartbeat: it
/// bumps `lastScheduledAt` BEFORE delegating to `performTick()`,
/// mirroring what each plugin used to do by hand. The bump runs before
/// any early-return gate inside `performTick()` (e.g. the phone_calls
/// protocol-version gate), preserving the prior contract that even a
/// gated/aborted tick records a fresh scheduled-at.
///
/// IMPORTANT — dispatch: a conformer MUST NOT declare its own `tick()`.
/// Swift resolves a protocol-requirement witness to the most-specific
/// concrete declaration; a plugin-defined `tick()` would shadow this
/// extension default when the plugin is used through `SourcePlugin`,
/// silently disabling the heartbeat. Conformers implement `performTick()`
/// ONLY. This is enforced two ways: (a) a source grep-test forbidding
/// `func tick(` in any DataSourcePlugin conformer module, and (b) a
/// conformance test calling `tick()` through a `SourcePlugin`-typed
/// reference and asserting the bump occurred.
///
/// The extension ALSO does not own the in-memory SourceHealthRegistry
/// update that feeds the Pi /heartbeat payload's `last_scheduled_at`
/// (per-plugin snapshot shape); that stays in `performTick()` and is
/// covered by the conformance test's registry assertion.
///
/// Operational-loop conformers (HeartbeatLoop, NotificationReconcileLoop)
/// stay on bare `SourcePlugin` — they have no `mutator`/`clock` and must
/// not write `lastScheduledAt`.
public protocol DataSourcePlugin: SourcePlugin {
    /// Serialized state writer shared process-wide.
    var mutator: StateMutator { get }
    /// Injected clock (tests pass a fixed clock).
    var clock: @Sendable () -> Date { get }
    /// Structured logger (heartbeat-write failures are logged + swallowed).
    var logger: LoggerProtocol { get }

    /// The plugin's real per-tick work. Replaces the old `tick()` body.
    func performTick() async throws
}

public extension DataSourcePlugin {
    /// Provided `tick()`: persist the liveness heartbeat, then run the
    /// plugin's work. A state-write hiccup is logged + swallowed — it
    /// must never abort the tick.
    func tick() async throws {
        await persistScheduledHeartbeat(at: clock())
        try await performTick()
    }

    /// Bump `state.sources[id].lastScheduledAt`. Single canonical copy
    /// replacing the five hand-written `updateScheduled(at:)` methods.
    func persistScheduledHeartbeat(at date: Date) async {
        do {
            try await mutator.mutate { state in
                var src = state.sources[id.rawValue] ?? SourceState()
                src.lastScheduledAt = date
                state.sources[id.rawValue] = src
            }
        } catch {
            logger.warning("\(id.rawValue) tick: lastScheduledAt mutate failed", metadata: [
                "error": .private(String(describing: error)),
            ])
        }
    }
}

/// Wrapper holding the dependencies a plugin needs. Currently a tiny
/// surface (just a logger); source-specific dependencies (PiClient,
/// StateStore, contact-store accessors) land with each real reader.
public struct SourceContext: Sendable {
    public let logger: LoggerProtocol

    public init(logger: LoggerProtocol) {
        self.logger = logger
    }
}
