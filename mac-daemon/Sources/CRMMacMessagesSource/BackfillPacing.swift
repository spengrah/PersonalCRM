// BackfillPacing — (row-count, wall-clock) budget tracker.
//
// Each messages tick is bounded by ~500 rows and ~5 seconds.
// Backfill and live share a single budget that decrements as the tick
// processes batches: backfill runs first, then live consumes whatever
// remains.
//
// Pure value type — no system clock dependency. Caller supplies a
// startedAt timestamp and consults the budget after each batch.
import Foundation

public struct BackfillBudget: Sendable {
    public let maxRows: Int
    public let maxDuration: TimeInterval

    public private(set) var consumedRows: Int = 0
    private let startedAt: Date

    public init(maxRows: Int = 500,
                maxDuration: TimeInterval = 5.0,
                now: Date) {
        self.maxRows = maxRows
        self.maxDuration = maxDuration
        self.startedAt = now
    }

    public mutating func consume(rows: Int) {
        consumedRows += rows
    }

    /// Remaining rows the tick is allowed to process.  Zero means the
    /// row budget is exhausted.
    public var rowsRemaining: Int {
        max(0, maxRows - consumedRows)
    }

    /// Whether the wall-clock budget is exhausted, given the current
    /// time.  Caller passes `now` to avoid coupling to a global clock.
    public func wallclockExhausted(now: Date) -> Bool {
        now.timeIntervalSince(startedAt) >= maxDuration
    }

    /// True if either budget is exhausted (caller stops processing).
    public func exhausted(now: Date) -> Bool {
        rowsRemaining == 0 || wallclockExhausted(now: now)
    }
}
