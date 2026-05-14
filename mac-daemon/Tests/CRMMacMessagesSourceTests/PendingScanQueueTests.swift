import XCTest
@testable import CRMMacMessagesSource

final class PendingScanQueueTests: XCTestCase {
    private func makeScan(_ handle: String, _ unixSeconds: TimeInterval = 1_700_000_000) -> PendingScan {
        PendingScan(normalizedHandle: handle, since: Date(timeIntervalSince1970: unixSeconds))
    }

    func testEmptyOnInit() {
        let q = PendingScanQueue()
        XCTAssertTrue(q.isEmpty)
        XCTAssertEqual(q.count, 0)
    }

    func testEnqueueAndDequeueFIFO() {
        var q = PendingScanQueue()
        q.enqueue(makeScan("+15551234567"))
        q.enqueue(makeScan("foo@example.com"))
        XCTAssertEqual(q.count, 2)

        let first = q.dequeue()
        XCTAssertEqual(first?.normalizedHandle, "+15551234567")

        let second = q.dequeue()
        XCTAssertEqual(second?.normalizedHandle, "foo@example.com")

        XCTAssertNil(q.dequeue())
    }

    func testCapDropsOldest() {
        var q = PendingScanQueue()
        for i in 0..<MessagesCursor.pendingScansCap {
            q.enqueue(makeScan("+1555\(i)"))
        }
        XCTAssertEqual(q.count, MessagesCursor.pendingScansCap)

        // Next enqueue must drop the oldest (entry at index 0).
        let dropped = q.enqueue(makeScan("+15559999999"))
        XCTAssertTrue(dropped)
        XCTAssertEqual(q.count, MessagesCursor.pendingScansCap)

        // The first dequeue is now the SECOND original entry.
        let next = q.dequeue()
        XCTAssertEqual(next?.normalizedHandle, "+15551") // was index 1
    }

    func testInitEnforcesCap() {
        // Caller hands in 300 entries; queue caps at pendingScansCap
        // and keeps the LAST pendingScansCap entries.
        let oversized = (0..<300).map { makeScan("+1555\($0)") }
        let q = PendingScanQueue(oversized)
        XCTAssertEqual(q.count, MessagesCursor.pendingScansCap)
        // First entry should be index (300 - 256) = 44.
        XCTAssertEqual(q.entries.first?.normalizedHandle, "+155544")
    }

    func testReplace() {
        var q = PendingScanQueue([makeScan("a"), makeScan("b")])
        q.replace(with: [makeScan("c")])
        XCTAssertEqual(q.count, 1)
        XCTAssertEqual(q.entries.first?.normalizedHandle, "c")
    }
}
