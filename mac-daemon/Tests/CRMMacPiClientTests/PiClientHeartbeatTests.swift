import XCTest
@testable import CRMMacPiClient

final class PiClientHeartbeatTests: XCTestCase {
    private let auth = PiAuth(
        hostID: UUID(uuidString: "11111111-2222-3333-4444-555555555555")!,
        apiKey: "k")
    private let emptyBody = HeartbeatBody(
        daemonVersion: "0.1.0",
        protocolVersion: 1,
        permissions: Data("{}".utf8),
        sourceHealth: Data("{}".utf8))

    private func client(_ script: MockTransportScript) -> PiClient {
        PiClient(
            baseURL: URL(string: "https://pi.example.test")!,
            transport: script.asTransport(),
            sleep: noopSleep)
    }

    func testHeartbeat200Decodes() async throws {
        let data = try loadFixture("heartbeat_200")
        let script = MockTransportScript([.respond(status: 200, data: data)])
        let result = try await client(script).heartbeat(auth: auth, body: emptyBody)
        XCTAssertTrue(result.ok)
        XCTAssertEqual(result.cursorEpoch, 1)
        XCTAssertEqual(result.protocolVersion, 1)
        XCTAssertEqual(result.minProtocolVersion, 1)
    }

    func testHeartbeat401MapsToAuthRevoked() async throws {
        let data = try loadFixture("heartbeat_401")
        let script = MockTransportScript([.respond(status: 401, data: data)])
        await assertThrows({ try await client(script).heartbeat(auth: auth, body: emptyBody) }) { error in
            guard case PiClientError.authenticationRevoked = error else {
                XCTFail("got \(error)")
                return
            }
        }
    }

    func testHeartbeat412MapsToUpgradeRequiredWithMinVersion() async throws {
        let data = try loadFixture("heartbeat_412")
        let script = MockTransportScript([.respond(status: 412, data: data)])
        await assertThrows({ try await client(script).heartbeat(auth: auth, body: emptyBody) }) { error in
            guard case let PiClientError.upgradeRequired(minVersion, _) = error else {
                XCTFail("got \(error)")
                return
            }
            XCTAssertEqual(minVersion, 2)
        }
    }

    func testHeartbeat5xxRetriesThenSurfaces() async throws {
        // Five retries means six total attempts before surfacing.
        let script = MockTransportScript([
            .respond(status: 500, data: Data("{}".utf8)),
            .respond(status: 502, data: Data("{}".utf8)),
            .respond(status: 503, data: Data("{}".utf8)),
            .respond(status: 504, data: Data("{}".utf8)),
            .respond(status: 500, data: Data("{}".utf8)),
            .respond(status: 502, data: Data("{}".utf8)),
        ])
        await assertThrows({ try await client(script).heartbeat(auth: auth, body: emptyBody) }) { error in
            guard case PiClientError.serverError = error else {
                XCTFail("got \(error)")
                return
            }
        }
        XCTAssertEqual(script.invocations.count, 6, "should attempt 6 times (1 + 5 retries)")
    }

    func testHeartbeat5xxThenSuccessRecovers() async throws {
        let okData = try loadFixture("heartbeat_200")
        let script = MockTransportScript([
            .respond(status: 503, data: Data("{}".utf8)),
            .respond(status: 200, data: okData),
        ])
        let result = try await client(script).heartbeat(auth: auth, body: emptyBody)
        XCTAssertTrue(result.ok)
        XCTAssertEqual(script.invocations.count, 2)
    }

    func testHeartbeatSetsAuthHeaders() async throws {
        let data = try loadFixture("heartbeat_200")
        let script = MockTransportScript([.respond(status: 200, data: data)])
        _ = try await client(script).heartbeat(auth: auth, body: emptyBody)
        XCTAssertEqual(script.invocations[0].value(forHTTPHeaderField: "X-Mac-Host-ID")?.lowercased(), auth.hostID.uuidString.lowercased())
        XCTAssertEqual(script.invocations[0].value(forHTTPHeaderField: "Authorization"), "Bearer k")
        XCTAssertEqual(
            script.invocations[0].url?.path,
            "/api/v1/host/\(auth.hostID.uuidString.lowercased())/heartbeat")
    }

    func testHeartbeat429NotRetried() async throws {
        // 429 is a 4xx — RetryingTransport must surface it without
        // retrying. The Pi pairing limiter returns 429 without
        // Retry-After; we don't amplify that pressure.
        let script = MockTransportScript([
            .respond(status: 429, data: Data("{\"success\":false,\"error\":{\"code\":\"RATE_LIMITED\",\"message\":\"too many\"}}".utf8)),
        ])
        await assertThrows({ try await client(script).heartbeat(auth: auth, body: emptyBody) }) { error in
            guard case let PiClientError.clientError(status, _, _) = error else {
                XCTFail("got \(error)")
                return
            }
            XCTAssertEqual(status, 429)
        }
        XCTAssertEqual(script.invocations.count, 1, "4xx must not retry")
    }
}
