// HeartbeatStateProvider — read-only seam exposing the most recent
// Pi-reported `protocol_version` from `DaemonState`. Used by source
// plugins to feature-gate themselves against older Pi instances
// without taking a full `StateMutator` dependency.
//
// Production wiring: the executable composition root provides an impl
// backed by `StateMutator` (read-only path). Tests inject a recording
// fake.
//
// nil = no successful heartbeat has been recorded yet (treat as
// "wait — don't activate yet" in the gate).
import Foundation

/// Read-only view onto `DaemonState.lastKnownPiProtocolVersion`.
public protocol HeartbeatStateProvider: Sendable {
    /// The Pi's `protocol_version` from the most recent successful
    /// heartbeat. Nil means no successful heartbeat is on record (yet).
    var lastKnownPiProtocolVersion: Int32? { get async }
}

/// In-memory test impl. Reads/writes a single Int32? slot via an
/// actor — async-safe and concurrency-friendly.
public actor InMemoryHeartbeatStateProvider: HeartbeatStateProvider {
    private var value: Int32?

    public init(initial: Int32? = nil) {
        self.value = initial
    }

    public var lastKnownPiProtocolVersion: Int32? {
        get async { value }
    }

    /// Test-only setter.
    public func set(_ v: Int32?) {
        value = v
    }
}
