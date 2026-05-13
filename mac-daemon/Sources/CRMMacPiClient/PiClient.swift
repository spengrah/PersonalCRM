// PiClient is the typed HTTP client every CRMMacLifecycle workflow
// uses to talk to the Pi. All requests go through a RetryingTransport
// per plan D16; the pair request bypasses retry entirely.
//
// Tests inject a per-test URLSession + MockHTTPProtocol handler so no
// global URLProtocol registration is needed and parallel tests cannot
// race on shared state.
import Foundation
import CRMMacCore

public final class PiClient {
    private let builder: RequestBuilder
    private let transport: RetryingTransport
    private let decoder: JSONDecoder
    private let logger: LoggerProtocol

    public init(
        baseURL: URL,
        session: URLSession = .shared,
        policy: BackoffPolicy = BackoffPolicy(),
        logger: LoggerProtocol = NoopLogger()
    ) {
        self.builder = RequestBuilder(baseURL: baseURL)
        self.transport = RetryingTransport(
            transport: { request in
                try await session.data(for: request)
            },
            policy: policy,
            logger: logger)
        self.decoder = JSONDecoder()
        self.logger = logger
    }

    /// Test-friendly initializer that takes the transport function
    /// directly. Used by the URLProtocol-mocked unit tests.
    public init(
        baseURL: URL,
        transport: @escaping TransportFunc,
        policy: BackoffPolicy = BackoffPolicy(),
        sleep: @escaping SleepFunction = { try await Task.sleep(nanoseconds: UInt64($0 * 1_000_000_000)) },
        logger: LoggerProtocol = NoopLogger()
    ) {
        self.builder = RequestBuilder(baseURL: baseURL)
        self.transport = RetryingTransport(
            transport: transport,
            policy: policy,
            sleep: sleep,
            logger: logger)
        self.decoder = JSONDecoder()
        self.logger = logger
    }

    public func pair(
        token: String,
        hostname: String,
        daemonVersion: String,
        protocolVersion: Int32
    ) async throws -> PairData {
        let request = try builder.pair(
            token: token,
            hostname: hostname,
            daemonVersion: daemonVersion,
            protocolVersion: protocolVersion)
        let (data, http) = try await transport.send(request, maxRetries: 0)
        return try decodePair(data: data, http: http)
    }

    public func heartbeat(auth: PiAuth, body: HeartbeatBody) async throws -> HeartbeatData {
        let request = try builder.heartbeat(auth: auth, body: body)
        let (data, http) = try await transport.send(request)
        return try decodeHeartbeat(data: data, http: http)
    }

    public func knownIdentifiers(auth: PiAuth) async throws -> KnownIdentifiersData {
        let request = try builder.knownIdentifiers(auth: auth)
        let (data, http) = try await transport.send(request)
        return try decodeKnownIdentifiers(data: data, http: http)
    }

    // MARK: - decoding helpers

    private func decodePair(data: Data, http: HTTPURLResponse) throws -> PairData {
        switch http.statusCode {
        case 200, 201:
            return try decodeSuccess(data: data, type: PairData.self)
        case 409:
            throw PiClientError.hostAlreadyPaired(message: errorMessage(data) ?? "host already paired")
        case 410:
            throw PiClientError.pairingTokenRejected(message: errorMessage(data) ?? "pairing token invalid")
        case 400...499:
            throw mapClient4xx(status: http.statusCode, data: data)
        case 500...599:
            // RetryingTransport surfaces 5xx as PiClientError.serverError;
            // any 5xx reaching here is anomalous and worth surfacing.
            throw PiClientError.serverError(
                status: http.statusCode,
                message: errorMessage(data) ?? "server error \(http.statusCode)")
        default:
            throw PiClientError.transport(
                underlying: "unexpected status \(http.statusCode)")
        }
    }

    private func decodeHeartbeat(data: Data, http: HTTPURLResponse) throws -> HeartbeatData {
        switch http.statusCode {
        case 200:
            return try decodeSuccess(data: data, type: HeartbeatData.self)
        case 401:
            throw PiClientError.authenticationRevoked(
                message: errorMessage(data) ?? "authentication revoked")
        case 412:
            let minVersion = try? extractMinVersion(data: data)
            throw PiClientError.upgradeRequired(
                minVersion: minVersion,
                message: errorMessage(data) ?? "upgrade required")
        case 400...499:
            throw mapClient4xx(status: http.statusCode, data: data)
        default:
            throw PiClientError.transport(
                underlying: "unexpected status \(http.statusCode)")
        }
    }

    private func decodeKnownIdentifiers(data: Data, http: HTTPURLResponse) throws -> KnownIdentifiersData {
        switch http.statusCode {
        case 200:
            return try decodeSuccess(data: data, type: KnownIdentifiersData.self)
        case 401:
            throw PiClientError.authenticationRevoked(
                message: errorMessage(data) ?? "authentication revoked")
        case 400...499:
            throw mapClient4xx(status: http.statusCode, data: data)
        default:
            throw PiClientError.transport(
                underlying: "unexpected status \(http.statusCode)")
        }
    }

    private func decodeSuccess<T: Decodable>(data: Data, type: T.Type) throws -> T {
        let envelope: APIEnvelope<T>
        do {
            envelope = try decoder.decode(APIEnvelope<T>.self, from: data)
        } catch {
            throw PiClientError.decode(reason: String(describing: error))
        }
        guard envelope.success else {
            if let err = envelope.error {
                throw PiClientError.envelopeError(code: err.code, message: err.message)
            }
            throw PiClientError.envelopeError(code: "UNKNOWN", message: "success=false with no error payload")
        }
        guard let payload = envelope.data else {
            throw PiClientError.decode(reason: "success envelope missing data")
        }
        return payload
    }

    private func errorMessage(_ data: Data) -> String? {
        struct Probe: Decodable { let error: APIError? }
        return (try? decoder.decode(Probe.self, from: data))?.error?.message
    }

    private func mapClient4xx(status: Int, data: Data) -> PiClientError {
        struct Probe: Decodable { let error: APIError? }
        let probe = (try? decoder.decode(Probe.self, from: data))?.error
        return PiClientError.clientError(
            status: status,
            code: probe?.code ?? "CLIENT_ERROR",
            message: probe?.message ?? "client error \(status)")
    }

    /// Extracts the `min_version` field from the upgrade-required 412
    /// payload. Best-effort; nil if absent.
    private func extractMinVersion(data: Data) throws -> Int32? {
        // The 412 body is shaped as `{success, error: {code, message, min_version, upgrade_required}}`
        // — the Pi puts min_version inside the error object, not the
        // top-level data. We decode lazily to a permissive shape.
        struct ErrorWithVersion: Decodable {
            let minVersion: Int32?
            private enum CodingKeys: String, CodingKey {
                case minVersion = "min_version"
            }
        }
        struct Probe: Decodable {
            let error: ErrorWithVersion?
        }
        return (try? decoder.decode(Probe.self, from: data))?.error?.minVersion
    }
}
