// Exponential-backoff helper. Used by CRMMacPiClient.RetryingTransport
// and by any future per-source retry callsite.
//
// No jitter — the daemon makes at most ~1 retry burst per minute
// under heartbeat, far from rate-limit pressure. Jitter can land in
// a follow-up if needed.
import Foundation

public struct BackoffPolicy: Equatable, Sendable {
    /// First sleep delay, in seconds.
    public let initialDelay: TimeInterval
    /// Multiplier per attempt. Default is 2 (exponential).
    public let multiplier: Double
    /// Max number of retries AFTER the initial attempt. The total
    /// number of attempts is `maxRetries + 1`.
    public let maxRetries: Int

    public init(
        initialDelay: TimeInterval = 1.0,
        multiplier: Double = 2.0,
        maxRetries: Int = 5
    ) {
        self.initialDelay = initialDelay
        self.multiplier = multiplier
        self.maxRetries = maxRetries
    }

    /// Sleep before retry attempt `attempt` (1-indexed; 1 == sleep
    /// before the first retry, 2 == before the second, etc.). Returns
    /// 0 for invalid indices.
    public func delay(forAttempt attempt: Int) -> TimeInterval {
        guard attempt >= 1, attempt <= maxRetries else { return 0 }
        return initialDelay * pow(multiplier, Double(attempt - 1))
    }
}
