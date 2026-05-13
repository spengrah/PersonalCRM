// RetryingTransport wraps URLSession with policy-aware retries.
//
// Retry policy:
//   - 5xx + network errors are retryable (5 retries; 1s, 2s, 4s, 8s, 16s).
//   - 4xx (including 429) are NEVER retried — Pi's pairing limiter
//     returns 429 without Retry-After and a client retry would amplify
//     pressure on a rate-limited endpoint.
//   - Pair requests are non-idempotent on the daemon side (ambiguous
//     response is operator-recovery territory) and bypass retry
//     entirely by passing maxRetries=0.
import Foundation
import CRMMacCore

/// Function-type sleeper so tests can inject a no-sleep variant.
public typealias SleepFunction = (TimeInterval) async throws -> Void

/// Closure-based mock URLSession contract. Production passes a real
/// URLSession.data(for:) closure; tests pass a closure that consults
/// a per-test MockHTTPProtocol handler.
public typealias TransportFunc = (URLRequest) async throws -> (Data, URLResponse)

public struct RetryingTransport {
    public let transport: TransportFunc
    public let policy: BackoffPolicy
    public let sleep: SleepFunction
    public let logger: LoggerProtocol

    public init(
        transport: @escaping TransportFunc,
        policy: BackoffPolicy = BackoffPolicy(),
        sleep: @escaping SleepFunction = { try await Task.sleep(nanoseconds: UInt64($0 * 1_000_000_000)) },
        logger: LoggerProtocol = NoopLogger()
    ) {
        self.transport = transport
        self.policy = policy
        self.sleep = sleep
        self.logger = logger
    }

    /// Send `request`. Retries on 5xx + network errors per policy.
    /// `maxRetries=0` disables retry entirely; used by the pair request.
    public func send(_ request: URLRequest, maxRetries: Int? = nil) async throws -> (Data, HTTPURLResponse) {
        let effectiveMax = min(maxRetries ?? policy.maxRetries, policy.maxRetries)
        var lastError: Error?
        var lastStatus: Int?
        var lastData: Data?

        // Attempts: 1 initial + `effectiveMax` retries.
        for attempt in 0...effectiveMax {
            if attempt > 0 {
                let delay = policy.delay(forAttempt: attempt)
                logger.warning("pi client retry", metadata: [
                    "attempt": .public(String(attempt)),
                    "max_retries": .public(String(effectiveMax)),
                    "delay_seconds": .public(String(delay)),
                ])
                try await sleep(delay)
            }
            do {
                let (data, response) = try await transport(request)
                guard let http = response as? HTTPURLResponse else {
                    throw PiClientError.transport(underlying: "non-HTTP response")
                }
                lastStatus = http.statusCode
                lastData = data
                if http.statusCode >= 500 && http.statusCode <= 599 {
                    // Retryable server error — loop.
                    lastError = PiClientError.serverError(
                        status: http.statusCode,
                        message: "server error \(http.statusCode)")
                    continue
                }
                return (data, http)
            } catch let pi as PiClientError {
                throw pi
            } catch let urlError as URLError {
                lastError = PiClientError.transport(underlying: String(describing: urlError.code))
                // Retryable network failure — loop.
                continue
            } catch {
                lastError = PiClientError.transport(underlying: String(describing: error))
                continue
            }
        }

        // Exhausted retries.
        if let status = lastStatus, let data = lastData, (500...599).contains(status) {
            let message = Self.extractServerMessage(from: data) ?? "server error \(status)"
            throw PiClientError.serverError(status: status, message: message)
        }
        if let lastError {
            throw lastError
        }
        // Should be unreachable — but surface a typed error rather than
        // an implicit nil.
        throw PiClientError.transport(underlying: "no response after \(effectiveMax + 1) attempts")
    }

    /// Best-effort extraction of `error.message` from a Pi envelope.
    /// Returns nil if the body doesn't look like one.
    private static func extractServerMessage(from data: Data) -> String? {
        struct Probe: Decodable {
            let error: APIError?
        }
        return (try? JSONDecoder().decode(Probe.self, from: data))?.error?.message
    }
}
