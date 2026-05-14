// ClockAdapter exists so the installer's "installed_at" field and
// the heartbeat loop's last-tick recording can be exercised against a
// deterministic time source in tests. The fake (FixedClock) lives
// in CRMMacLifecycleTests/Fakes.
import Foundation

public protocol ClockAdapter: Sendable {
    func now() -> Date
}

public struct SystemClock: ClockAdapter {
    public init() {}
    public func now() -> Date { Date() }
}
