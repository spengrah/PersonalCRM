// ClockAdapter exists so the installer's "installed_at" field and
// the heartbeat loop's last-tick recording can be exercised against a
// deterministic time source in tests.
import Foundation

public protocol ClockAdapter {
    func now() -> Date
}

public struct SystemClock: ClockAdapter {
    public init() {}
    public func now() -> Date { Date() }
}

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
