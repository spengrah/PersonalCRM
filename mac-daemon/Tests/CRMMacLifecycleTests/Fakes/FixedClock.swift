import Foundation
@testable import CRMMacLifecycle

/// Deterministic time source for tests. The installer's
/// `installed_at` field and the heartbeat loop's last-tick recording
/// both go through ClockAdapter so test scenarios can pin time.
public final class FixedClock: ClockAdapter {
    private var current: Date
    public init(_ start: Date = Date(timeIntervalSince1970: 1_700_000_000)) {
        self.current = start
    }
    public func now() -> Date { current }
    public func advance(by seconds: TimeInterval) {
        current = current.addingTimeInterval(seconds)
    }
    public func setTo(_ date: Date) {
        current = date
    }
}
