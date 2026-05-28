// PiClient is the typed HTTP client every CRMMacLifecycle workflow
// uses to talk to the Pi. All requests go through a RetryingTransport;
// the pair request bypasses retry entirely because the pair tx is
// non-idempotent on the daemon side (ambiguous response is operator-
// recovery territory).
//
// Tests inject a per-test URLSession + MockHTTPProtocol handler so no
// global URLProtocol registration is needed and parallel tests cannot
// race on shared state.
import Foundation
import CRMMacCore

/// `Sendable` constraint: `PiClient` is shared by every workflow
/// (installer, heartbeat loop, source plugins). The stored builder /
/// transport / decoder / logger are themselves `Sendable` or wrap
/// thread-safe Apple types. `JSONDecoder` is documented thread-safe
/// after configuration; we configure once in `init` and never mutate
/// it. Marked `@unchecked Sendable` because `JSONDecoder` lacks a
/// formal `Sendable` conformance from Foundation but is safe to share
/// across actors in practice.
public final class PiClient: @unchecked Sendable {
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

    /// POST /api/v1/host/:id/rotate-key. Returns the new plaintext
    /// api-key + the timestamp the server recorded for the rotation.
    /// `maxRetries: 0` because the rotate tx is non-idempotent: a
    /// retry against a tx that really did succeed would consume a
    /// second token via the next attempt.
    public func rotateAPIKey(
        auth: PiAuth,
        newPairingToken: String
    ) async throws -> RotateAPIKeyData {
        let request = try builder.rotateAPIKey(
            auth: auth, pairingToken: newPairingToken)
        let (data, http) = try await transport.send(request, maxRetries: 0)
        return try decodeRotateAPIKey(data: data, http: http)
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

    /// GET /api/v1/host/:id/sync/:source/known-ids. Returns the
    /// per-(host, source) set of live external_contact source_ids
    /// the Pi has on file, paired with each row's last observed
    /// content hash. Used by the icloud_contacts source plugin's
    /// recovery flow to drive tombstone reconciliation against the
    /// live CNContactStore scan.
    public func knownIDs(auth: PiAuth, source: String) async throws -> KnownIDsData {
        let request = try builder.knownIDs(auth: auth, source: source)
        let (data, http) = try await transport.send(request)
        return try decodeKnownIDs(data: data, http: http)
    }

    /// GET /api/v1/host/:id/sync/:source/cursor.  Returns the
    /// unwrapped cursorResponse data (cursor string + epoch + flag).
    public func getCursor(auth: PiAuth, source: String) async throws -> SourceCursorState {
        let request = try builder.getCursor(auth: auth, source: source)
        let (data, http) = try await transport.send(request)
        return try decodeGetCursor(data: data, http: http)
    }

    /// POST /api/v1/host/:id/sync/:source/cursor.  Success returns void
    /// (the Pi's `data` is `{"ok": true}`).  On 409: throws
    /// PiClientError.cursorConflict(code:, current:).
    public func commitCursor(
        auth: PiAuth,
        source: String,
        cursor: String,
        baseCursor: String,
        cursorEpoch: Int64,
        backfillComplete: Bool
    ) async throws {
        let request = try builder.commitCursor(
            auth: auth,
            source: source,
            cursor: cursor,
            baseCursor: baseCursor,
            cursorEpoch: cursorEpoch,
            backfillComplete: backfillComplete)
        let (data, http) = try await transport.send(request)
        try decodeCommitCursor(data: data, http: http)
    }

    /// POST /api/v1/ingest/events.  The Pi's response is deliberately
    /// NOT enveloped — we decode the raw top-level shape.
    public func ingestEvents(
        auth: PiAuth,
        body: IngestEventsBody
    ) async throws -> IngestEventsData {
        let request = try builder.ingestEvents(auth: auth, body: body)
        let (data, http) = try await transport.send(request)
        return try decodeIngestEvents(data: data, http: http)
    }

    /// GET /api/v1/meeting-notes/needs-attention?host_id=<uuid>.
    /// Returns the current set of meeting_note rows in linkage_state
    /// conflict_pending or orphan_needs_review for the given host.
    /// Response is enveloped per api.SendSuccess; decoded via the
    /// existing decodeSuccess helper.
    public func needsAttention(
        auth: PiAuth,
        hostID: UUID
    ) async throws -> [NeedsAttentionListItem] {
        let request = try builder.needsAttention(auth: auth, hostID: hostID)
        let (data, http) = try await transport.send(request)
        return try decodeNeedsAttention(data: data, http: http)
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

    private func decodeRotateAPIKey(data: Data, http: HTTPURLResponse) throws -> RotateAPIKeyData {
        switch http.statusCode {
        case 200:
            return try decodeSuccess(data: data, type: RotateAPIKeyData.self)
        case 400:
            // 400s carry distinct codes (INVALID_PAIRING_TOKEN,
            // TOKEN_ALREADY_USED, TOKEN_EXPIRED). mapClient4xx returns
            // PiClientError.clientError with the code preserved so the
            // CLI wrapper can branch on it.
            throw mapClient4xx(status: http.statusCode, data: data)
        case 401:
            // Two distinct 401 codes:
            //   INVALID_KEY → map to authenticationRevoked for shared
            //     CLI handling (matches every other endpoint's 401).
            //   STALE_AUTH → distinct operator remediation
            //     ("re-pair against the new live key"). Preserved as
            //     clientError(401, "STALE_AUTH", _) so the CLI can
            //     branch.
            struct Auth401Probe: Decodable {
                let error: APIError?
            }
            if let probe = try? decoder.decode(Auth401Probe.self, from: data),
               probe.error?.code == "STALE_AUTH" {
                throw PiClientError.clientError(
                    status: 401,
                    code: "STALE_AUTH",
                    message: probe.error?.message ?? "api-key stale; another rotation committed")
            }
            throw PiClientError.authenticationRevoked(
                message: errorMessage(data) ?? "authentication revoked")
        case 404:
            // 404 could mean either:
            //   (a) the Pi recognized the route but the host id was
            //       revoked between auth and tx-internal lookup
            //       (handler returns SendNotFound which emits the
            //       standard {success:false, error:{...}} envelope).
            //   (b) the Pi is an older version that doesn't have the
            //       rotate-key route at all (gin returns an empty 404
            //       body with no `success` field).
            // Distinguish via body shape so the CLI wrapper can print
            // distinct remediation ("host gone" vs "Pi needs upgrade").
            struct EnvelopeProbe: Decodable {
                let success: Bool
                let error: APIError?
            }
            if let probe = try? decoder.decode(EnvelopeProbe.self, from: data) {
                throw PiClientError.clientError(
                    status: 404,
                    code: probe.error?.code ?? "NOT_FOUND",
                    message: probe.error?.message ?? "host not found")
            }
            throw PiClientError.clientError(
                status: 404,
                code: "ROUTE_NOT_FOUND",
                message: "Pi did not recognize POST /api/v1/host/:id/rotate-key — Pi may need an upgrade")
        case 400...499:
            throw mapClient4xx(status: http.statusCode, data: data)
        case 500...599:
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

    private func decodeKnownIDs(data: Data, http: HTTPURLResponse) throws -> KnownIDsData {
        switch http.statusCode {
        case 200:
            return try decodeSuccess(data: data, type: KnownIDsData.self)
        case 401:
            throw PiClientError.authenticationRevoked(
                message: errorMessage(data) ?? "authentication revoked")
        case 400...499:
            throw mapClient4xx(status: http.statusCode, data: data)
        case 500...599:
            throw PiClientError.serverError(
                status: http.statusCode,
                message: errorMessage(data) ?? "server error \(http.statusCode)")
        default:
            throw PiClientError.transport(
                underlying: "unexpected status \(http.statusCode)")
        }
    }

    private func decodeGetCursor(data: Data, http: HTTPURLResponse) throws -> SourceCursorState {
        switch http.statusCode {
        case 200:
            return try decodeSuccess(data: data, type: SourceCursorState.self)
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

    private func decodeCommitCursor(data: Data, http: HTTPURLResponse) throws {
        switch http.statusCode {
        case 200:
            // Success — body is `{"ok": true}`.  We do not need to read
            // it; the status code is the discriminator.
            return
        case 401:
            throw PiClientError.authenticationRevoked(
                message: errorMessage(data) ?? "authentication revoked")
        case 409:
            throw try decodeCursorConflict(data: data)
        case 400...499:
            throw mapClient4xx(status: http.statusCode, data: data)
        default:
            throw PiClientError.transport(
                underlying: "unexpected status \(http.statusCode)")
        }
    }

    /// 409 conflict body shape is
    ///   {success:false,
    ///    error:{code,message},
    ///    data:{current_cursor?, current_epoch?}}
    /// We decode error.code -> ConflictCode and data -> CursorConflict.
    private func decodeCursorConflict(data: Data) throws -> PiClientError {
        struct Envelope: Decodable {
            let error: APIError?
            let data: CursorConflict?
        }
        let env: Envelope
        do {
            env = try decoder.decode(Envelope.self, from: data)
        } catch {
            return PiClientError.decode(reason: "decode cursor conflict body: \(error)")
        }
        guard let err = env.error,
              let code = ConflictCode(rawValue: err.code) else {
            // Recognized 409 status but body is unexpected — surface
            // as a generic 4xx for visibility.
            return PiClientError.clientError(
                status: 409,
                code: env.error?.code ?? "UNKNOWN_CONFLICT",
                message: env.error?.message ?? "unknown cursor conflict shape")
        }
        let conflict = env.data ?? CursorConflict(currentCursor: nil, currentEpoch: nil)
        return PiClientError.cursorConflict(code: code, current: conflict)
    }

    /// IngestResponse is NOT enveloped — decode the raw top-level shape.
    private func decodeIngestEvents(data: Data, http: HTTPURLResponse) throws -> IngestEventsData {
        switch http.statusCode {
        case 200:
            do {
                return try decoder.decode(IngestEventsData.self, from: data)
            } catch {
                throw PiClientError.decode(reason: String(describing: error))
            }
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
        case 500...599:
            throw PiClientError.serverError(
                status: http.statusCode,
                message: errorMessage(data) ?? "server error \(http.statusCode)")
        default:
            throw PiClientError.transport(
                underlying: "unexpected status \(http.statusCode)")
        }
    }

    private func decodeNeedsAttention(data: Data, http: HTTPURLResponse) throws -> [NeedsAttentionListItem] {
        switch http.statusCode {
        case 200:
            return try decodeSuccess(data: data, type: [NeedsAttentionListItem].self)
        case 401:
            throw PiClientError.authenticationRevoked(
                message: errorMessage(data) ?? "authentication revoked")
        case 400...499:
            throw mapClient4xx(status: http.statusCode, data: data)
        case 500...599:
            throw PiClientError.serverError(
                status: http.statusCode,
                message: errorMessage(data) ?? "server error \(http.statusCode)")
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
