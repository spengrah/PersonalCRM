// ShutdownSignal is the awaitable barrier the daemon parks on while
// waiting for SIGTERM / SIGINT.
//
// Race-safe design: `signal()` and `wait()` may be invoked in any
// order. If signal() runs before wait() installs a continuation, the
// `signalled` flag set during deliver() is observed inside the
// withCheckedContinuation body and the continuation is resumed
// immediately — no permanent hang regardless of interleaving.
import Foundation

public actor ShutdownSignal {
    private var continuation: CheckedContinuation<Void, Never>?
    private var signalled = false

    public init() {}

    public nonisolated func signal() {
        Task { await self.deliver() }
    }

    private func deliver() {
        signalled = true
        continuation?.resume()
        continuation = nil
    }

    public nonisolated func wait() async {
        await waitInternal()
    }

    private func waitInternal() async {
        await withCheckedContinuation { (c: CheckedContinuation<Void, Never>) in
            // Install the continuation FIRST, then re-check the flag.
            // If signal() fired between the actor's previous quiescence
            // and now (or fires concurrently while we're suspended),
            // deliver() will see `continuation != nil` and resume us.
            // If signal already fired BEFORE we got here, we resume
            // ourselves inline so the continuation is not orphaned.
            if signalled {
                c.resume()
                return
            }
            continuation = c
        }
    }
}
