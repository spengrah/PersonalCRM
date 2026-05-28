// Coverage for FirstSuccessLatch single-fire semantics.
import XCTest
@testable import CRMMacLifecycle

final class FirstSuccessLatchTests: XCTestCase {

    actor Counter {
        private(set) var value: Int = 0
        func bump() { value += 1 }
    }

    func testFiresOnceOnFirstCall() async {
        let c = Counter()
        let latch = FirstSuccessLatch { await c.bump() }
        await latch.fireOnce()
        let count = await c.value
        XCTAssertEqual(count, 1)
    }

    func testSubsequentCallsAreNoOps() async {
        let c = Counter()
        let latch = FirstSuccessLatch { await c.bump() }
        for _ in 0..<10 {
            await latch.fireOnce()
        }
        let count = await c.value
        XCTAssertEqual(count, 1)
    }

    func testHasFiredReflectsState() async {
        let c = Counter()
        let latch = FirstSuccessLatch { await c.bump() }
        var fired = await latch.hasFired()
        XCTAssertFalse(fired)
        await latch.fireOnce()
        fired = await latch.hasFired()
        XCTAssertTrue(fired)
    }

    func testConcurrentCallsFireOnceTotal() async {
        // Actor isolation guarantees the guard sees the latest
        // value across concurrent calls — even with 100 parallel
        // fireOnce() invocations, the callback runs exactly once.
        let c = Counter()
        let latch = FirstSuccessLatch { await c.bump() }
        await withTaskGroup(of: Void.self) { group in
            for _ in 0..<100 {
                group.addTask { await latch.fireOnce() }
            }
        }
        let count = await c.value
        XCTAssertEqual(count, 1)
    }
}
