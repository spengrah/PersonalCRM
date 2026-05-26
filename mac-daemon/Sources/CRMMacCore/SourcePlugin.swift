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

/// Wrapper holding the dependencies a plugin needs. Currently a tiny
/// surface (just a logger); source-specific dependencies (PiClient,
/// StateStore, contact-store accessors) land with each real reader.
public struct SourceContext: Sendable {
    public let logger: LoggerProtocol

    public init(logger: LoggerProtocol) {
        self.logger = logger
    }
}
