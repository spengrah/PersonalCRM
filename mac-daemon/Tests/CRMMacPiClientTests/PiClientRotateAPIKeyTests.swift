import XCTest
@testable import CRMMacPiClient

final class PiClientRotateAPIKeyTests: XCTestCase {
    private let hostID = UUID(uuidString: "11111111-2222-3333-4444-555555555555")!

    private func client(script: MockTransportScript) -> PiClient {
        PiClient(
            baseURL: URL(string: "https://pi.example.test")!,
            transport: script.asTransport(),
            sleep: noopSleep)
    }

    private func auth() -> PiAuth {
        PiAuth(hostID: hostID, apiKey: "current-key")
    }

    func testRotateAPIKeySendsBearerAndHostHeadersWithBody() async throws {
        let body = Data("""
        {"success": true, "data": {"api_key": "new-key", "api_key_rotated_at": "2026-05-28T12:00:00Z"}}
        """.utf8)
        let script = MockTransportScript([.respond(status: 200, data: body)])
        let result = try await client(script: script).rotateAPIKey(
            auth: auth(), newPairingToken: "fresh-token")

        XCTAssertEqual(result.apiKey, "new-key")
        XCTAssertEqual(result.apiKeyRotatedAt, "2026-05-28T12:00:00Z")
        XCTAssertEqual(script.invocations.count, 1)
        let req = script.invocations[0]
        XCTAssertEqual(req.url?.path, "/api/v1/host/\(hostID.uuidString.lowercased())/rotate-key")
        XCTAssertEqual(req.httpMethod, "POST")
        XCTAssertEqual(req.value(forHTTPHeaderField: "Authorization"), "Bearer current-key")
        XCTAssertEqual(req.value(forHTTPHeaderField: "X-Mac-Host-ID"), hostID.uuidString.lowercased())
        let dict = try JSONSerialization.jsonObject(with: req.httpBody!) as? [String: String]
        XCTAssertEqual(dict?["pairing_token"], "fresh-token")
    }

    func testRotateAPIKey400InvalidTokenMapsToClientError() async throws {
        let body = Data("""
        {"success": false, "error": {"code": "INVALID_PAIRING_TOKEN", "message": "invalid"}}
        """.utf8)
        let script = MockTransportScript([.respond(status: 400, data: body)])
        await assertThrows({ try await self.client(script: script).rotateAPIKey(
            auth: self.auth(), newPairingToken: "x") }) { error in
            guard case PiClientError.clientError(let status, let code, _) = error else {
                XCTFail("expected clientError, got \(error)")
                return
            }
            XCTAssertEqual(status, 400)
            XCTAssertEqual(code, "INVALID_PAIRING_TOKEN")
        }
    }

    func testRotateAPIKey400TokenAlreadyUsedMapsToClientError() async throws {
        let body = Data("""
        {"success": false, "error": {"code": "TOKEN_ALREADY_USED", "message": "consumed"}}
        """.utf8)
        let script = MockTransportScript([.respond(status: 400, data: body)])
        await assertThrows({ try await self.client(script: script).rotateAPIKey(
            auth: self.auth(), newPairingToken: "x") }) { error in
            guard case PiClientError.clientError(_, let code, _) = error else {
                XCTFail("expected clientError, got \(error)")
                return
            }
            XCTAssertEqual(code, "TOKEN_ALREADY_USED")
        }
    }

    func testRotateAPIKey400TokenExpiredMapsToClientError() async throws {
        let body = Data("""
        {"success": false, "error": {"code": "TOKEN_EXPIRED", "message": "expired"}}
        """.utf8)
        let script = MockTransportScript([.respond(status: 400, data: body)])
        await assertThrows({ try await self.client(script: script).rotateAPIKey(
            auth: self.auth(), newPairingToken: "x") }) { error in
            guard case PiClientError.clientError(_, let code, _) = error else {
                XCTFail("expected clientError, got \(error)")
                return
            }
            XCTAssertEqual(code, "TOKEN_EXPIRED")
        }
    }

    func testRotateAPIKey401MapsToAuthenticationRevoked() async throws {
        let body = Data("""
        {"success": false, "error": {"code": "INVALID_KEY", "message": "auth failed"}}
        """.utf8)
        let script = MockTransportScript([.respond(status: 401, data: body)])
        await assertThrows({ try await self.client(script: script).rotateAPIKey(
            auth: self.auth(), newPairingToken: "x") }) { error in
            guard case PiClientError.authenticationRevoked = error else {
                XCTFail("expected authenticationRevoked, got \(error)")
                return
            }
        }
    }

    func testRotateAPIKey401StaleAuthMapsToClientErrorWithCodePreserved() async throws {
        let body = Data("""
        {"success": false, "error": {"code": "STALE_AUTH", "message": "rotated by another request"}}
        """.utf8)
        let script = MockTransportScript([.respond(status: 401, data: body)])
        await assertThrows({ try await self.client(script: script).rotateAPIKey(
            auth: self.auth(), newPairingToken: "x") }) { error in
            guard case PiClientError.clientError(let status, let code, _) = error else {
                XCTFail("expected clientError(STALE_AUTH), got \(error)")
                return
            }
            XCTAssertEqual(status, 401)
            XCTAssertEqual(code, "STALE_AUTH",
                "STALE_AUTH must be preserved distinctly from generic auth-revoked so the CLI can branch")
        }
    }

    func testRotateAPIKey404WithEnvelopeMapsToHostNotFound() async throws {
        // gin's SendNotFound emits {success:false, error:{...}} —
        // standard envelope.
        let body = Data("""
        {"success": false, "error": {"code": "NOT_FOUND", "message": "Mac host not found"}}
        """.utf8)
        let script = MockTransportScript([.respond(status: 404, data: body)])
        await assertThrows({ try await self.client(script: script).rotateAPIKey(
            auth: self.auth(), newPairingToken: "x") }) { error in
            guard case PiClientError.clientError(let status, let code, _) = error else {
                XCTFail("expected clientError(NOT_FOUND), got \(error)")
                return
            }
            XCTAssertEqual(status, 404)
            XCTAssertEqual(code, "NOT_FOUND")
        }
    }

    func testRotateAPIKey404WithoutEnvelopeMapsToRouteNotFound() async throws {
        // gin's default for an unregistered route — what an OLDER Pi
        // without the rotate-key route would return. Empty body, no
        // standard envelope.
        let script = MockTransportScript([.respond(status: 404, data: Data())])
        await assertThrows({ try await self.client(script: script).rotateAPIKey(
            auth: self.auth(), newPairingToken: "x") }) { error in
            guard case PiClientError.clientError(let status, let code, _) = error else {
                XCTFail("expected clientError(ROUTE_NOT_FOUND), got \(error)")
                return
            }
            XCTAssertEqual(status, 404)
            XCTAssertEqual(code, "ROUTE_NOT_FOUND",
                "404 without envelope must surface as ROUTE_NOT_FOUND so the CLI can suggest a Pi upgrade")
        }
    }

    func testRotateAPIKeyTransportErrorBubblesUp() async throws {
        let script = MockTransportScript([.fail(URLError(.timedOut))])
        await assertThrows({ try await self.client(script: script).rotateAPIKey(
            auth: self.auth(), newPairingToken: "x") }) { error in
            guard case PiClientError.transport = error else {
                XCTFail("expected transport error, got \(error)")
                return
            }
        }
        XCTAssertEqual(script.invocations.count, 1, "rotate must not retry on transport error")
    }

    func testRotateAPIKeyDoesNotRetryOn5xx() async throws {
        let script = MockTransportScript([.respond(status: 500, data: Data())])
        await assertThrows({ try await self.client(script: script).rotateAPIKey(
            auth: self.auth(), newPairingToken: "x") }) { error in
            guard case PiClientError.serverError = error else {
                XCTFail("expected serverError, got \(error)")
                return
            }
        }
        XCTAssertEqual(script.invocations.count, 1,
            "rotate must not retry on 5xx (tx is non-idempotent — a retry could consume a second token if the first really succeeded)")
    }
}
