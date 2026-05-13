import XCTest
@testable import CRMMacLifecycle

final class ShutdownSignalTests: XCTestCase {
    /// Common case: wait() is parked, signal() arrives later.
    func testSignalAfterWaitResumes() async {
        let shutdown = ShutdownSignal()
        let waitTask = Task<Bool, Never> {
            await shutdown.wait()
            return true
        }
        // Give the task a moment to park.
        try? await Task.sleep(nanoseconds: 10_000_000)
        shutdown.signal()
        let resumed = await waitTask.value
        XCTAssertTrue(resumed)
    }

    /// Edge case the actor reentrancy fix protects against: signal()
    /// fires BEFORE wait() is called. The wait() invocation must
    /// observe the latched flag and resume immediately rather than
    /// parking on a continuation no one will resume.
    func testSignalBeforeWaitDoesNotHang() async {
        let shutdown = ShutdownSignal()
        shutdown.signal()
        // Give the actor a moment to process the signal task.
        try? await Task.sleep(nanoseconds: 50_000_000)
        // wait() must return promptly. Wrap in a timeout-style task
        // so the test fails loudly rather than hanging if the bug
        // regresses.
        let returned = await withTaskGroup(of: Bool.self) { group in
            group.addTask {
                await shutdown.wait()
                return true
            }
            group.addTask {
                try? await Task.sleep(nanoseconds: 1_000_000_000)
                return false
            }
            let first = await group.next() ?? false
            group.cancelAll()
            return first
        }
        XCTAssertTrue(returned, "wait() did not return within 1s after pre-signal")
    }

    func testMultipleSignalsAreSafe() async {
        let shutdown = ShutdownSignal()
        shutdown.signal()
        shutdown.signal()
        shutdown.signal()
        try? await Task.sleep(nanoseconds: 20_000_000)
        await shutdown.wait()  // Must return.
    }
}
