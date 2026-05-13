// SourcePlugin is the contract every per-source poller satisfies.
// PR6 ships the protocol + two no-op stubs (messages, icloud_contacts);
// PR7 and PR8 each replace one stub with a real reader.
//
// The contract is intentionally tiny — the scheduler is owned by
// CRMMacLifecycle, not by the plugins themselves, so plugins remain
// passive callees that get a SourceContext and do one tick of work.
import Foundation

/// Stable identifier for a source. Persisted in `state.json` under
/// `sources[<id>]` and used in heartbeat `source_health` payloads.
public struct SourceID: RawRepresentable, Hashable, Codable, ExpressibleByStringLiteral {
    public let rawValue: String

    public init(rawValue: String) {
        self.rawValue = rawValue
    }

    public init(stringLiteral value: String) {
        self.rawValue = value
    }

    public static let messages: SourceID = "messages"
    public static let icloudContacts: SourceID = "icloud_contacts"
}

/// A poller of one external source. PR6's stub implementations log a
/// no-op tick; PR7/PR8 replace with real readers.
public protocol SourcePlugin: AnyObject {
    /// Stable source identifier; used as the state-file key and
    /// heartbeat-payload key.
    var id: SourceID { get }

    /// Schedule cadence the registry should request from ScheduleRunner.
    /// PR6 stubs ask for 60s; real readers tune this themselves.
    var tickInterval: TimeInterval { get }

    /// Called by the scheduler on every tick. PR6 stubs log + return.
    /// Errors are caught and logged by the caller — plugins don't need
    /// to handle their own retry envelope.
    func tick() async throws
}

/// Wrapper holding the dependencies a plugin needs. PR6 ships a tiny
/// surface (just a logger); PR7 adds a PiClient + StateStore; PR8 adds
/// the contact-store + filesystem accessors.
public struct SourceContext {
    public let logger: LoggerProtocol

    public init(logger: LoggerProtocol) {
        self.logger = logger
    }
}
