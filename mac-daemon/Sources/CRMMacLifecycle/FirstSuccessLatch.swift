// FirstSuccessLatch — a one-shot async dedup actor. The
// composition root passes a latch instance to HeartbeatLoop;
// the loop calls fireOnce() on every successful heartbeat. The
// latch's underlying closure runs exactly once (on the FIRST
// success), regardless of how many times fireOnce() is called.
//
// The orphan-notification subsystem uses this latch to trigger
// reconcile() after the daemon's first heartbeat succeeds —
// that's the earliest point where we know the Pi is reachable
// AND the host_id is still valid, so /needs-attention will not
// 401 due to a revoked key.
import Foundation

public actor FirstSuccessLatch {
    private var fired: Bool = false
    private let callback: @Sendable () async -> Void

    public init(callback: @escaping @Sendable () async -> Void) {
        self.callback = callback
    }

    /// Calls the registered callback the FIRST time this runs;
    /// subsequent calls are no-ops (and don't await the callback).
    /// Safe to call from any async context — actor isolation
    /// serializes the check + fire.
    public func fireOnce() async {
        guard !fired else { return }
        fired = true
        await callback()
    }

    /// Test accessor: returns true iff fireOnce() has been called
    /// at least once with the latch unfired (i.e. the callback was
    /// invoked).
    public func hasFired() -> Bool { fired }
}
