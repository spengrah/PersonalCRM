// StateMutator serializes load -> mutate -> save against StateStore.
//
// Atomic-rename in StateStore.save() prevents torn writes (a partial
// JSON file) but does NOT prevent stale-overwrite when two writers
// race on load -> mutate -> save. The actor's `mutate(_:)` closure
// runs the full read-modify-write under actor isolation so concurrent
// callers serialize.
//
// Writers expected to share a single instance per process:
//   - HeartbeatLoop (via OnDiskHeartbeatStateWriter)
//   - MessagesSourcePlugin (the messages source)
//   - Future source plugins (icloud_contacts in future iCloud Contacts source, etc.)
//
// Cross-process concurrency (daemon vs CLI ops subcommands) is the
// PidfileLock's job — actor isolation is intra-process only.
import Foundation

public actor StateMutator {
    private let store: StateStore

    public init(store: StateStore) {
        self.store = store
    }

    /// Load -> mutate via the closure -> save, all under actor
    /// isolation. The closure may throw, in which case state is NOT
    /// saved and the underlying state.json is unchanged.
    public func mutate(
        _ change: @Sendable (inout DaemonState) throws -> Void
    ) async throws {
        var state = try store.load()
        try change(&state)
        try store.save(state)
    }

    /// Variant of `mutate(_:)` that lets the closure surface a
    /// derived value from inside the serialized read-modify-write —
    /// useful when the caller needs to know, e.g., the sequence
    /// number assigned during the mutation. The persisted state is
    /// saved before the value is returned. The return value must be
    /// Sendable so it can cross the actor boundary safely.
    public func mutateReturning<T: Sendable>(
        _ change: @Sendable (inout DaemonState) throws -> T
    ) async throws -> T {
        var state = try store.load()
        let value = try change(&state)
        try store.save(state)
        return value
    }

    /// Load the current state without modifying it. Convenience for
    /// callers that need a consistent read alongside a separate mutate.
    public func read() async throws -> DaemonState {
        try store.load()
    }
}
