import XCTest
@testable import CRMMacPiClient

final class PiClientPairTests: XCTestCase {
    private func client(script: MockTransportScript) -> PiClient {
        PiClient(
            baseURL: URL(string: "https://pi.example.test")!,
            transport: script.asTransport(),
            sleep: noopSleep)
    }

    func testPairSuccessDecodesData() async throws {
        let data = try loadFixture("pair_200")
        let script = MockTransportScript([.respond(status: 200, data: data)])
        let result = try await client(script: script).pair(
            token: "abc", hostname: "mac-1", daemonVersion: "0.1.0", protocolVersion: 1)
        XCTAssertEqual(result.cursorEpoch, 1)
        XCTAssertEqual(result.apiKey, "test-api-key-plaintext")
        XCTAssertEqual(script.invocations.count, 1)
        XCTAssertEqual(script.invocations[0].url?.path, "/api/v1/host")
        XCTAssertEqual(script.invocations[0].httpMethod, "POST")
        // Pair must NOT carry auth headers.
        XCTAssertNil(script.invocations[0].value(forHTTPHeaderField: "X-Mac-Host-ID"))
        XCTAssertNil(script.invocations[0].value(forHTTPHeaderField: "Authorization"))
    }

    func testPair410MapsToPairingTokenRejected() async throws {
        let data = try loadFixture("pair_410_invalid_pair")
        let script = MockTransportScript([.respond(status: 410, data: data)])
        await assertThrows(client(script: script).pair(
            token: "bad", hostname: "mac-1", daemonVersion: "0.1.0", protocolVersion: 1)) { error in
            guard case PiClientError.pairingTokenRejected = error else {
                XCTFail("expected pairingTokenRejected, got \(error)")
                return
            }
        }
    }

    func testPair409MapsToHostAlreadyPaired() async throws {
        let data = try loadFixture("pair_409_already_paired")
        let script = MockTransportScript([.respond(status: 409, data: data)])
        await assertThrows(client(script: script).pair(
            token: "abc", hostname: "mac-1", daemonVersion: "0.1.0", protocolVersion: 1)) { error in
            guard case PiClientError.hostAlreadyPaired = error else {
                XCTFail("expected hostAlreadyPaired, got \(error)")
                return
            }
        }
    }

    func testPair5xxDoesNotRetry() async throws {
        // Pair must surface 5xx immediately — maxRetries=0.
        let script = MockTransportScript([
            .respond(status: 502, data: Data("{}".utf8)),
        ])
        await assertThrows(client(script: script).pair(
            token: "abc", hostname: "mac-1", daemonVersion: "0.1.0", protocolVersion: 1)) { error in
            guard case PiClientError.serverError = error else {
                XCTFail("expected serverError, got \(error)")
                return
            }
        }
        XCTAssertEqual(script.invocations.count, 1, "pair must not retry on 5xx")
    }

    func testPairNetworkErrorSurfacesImmediately() async throws {
        let script = MockTransportScript([
            .fail(URLError(.timedOut)),
        ])
        await assertThrows(client(script: script).pair(
            token: "abc", hostname: "mac-1", daemonVersion: "0.1.0", protocolVersion: 1)) { error in
            guard case PiClientError.transport = error else {
                XCTFail("expected transport error, got \(error)")
                return
            }
        }
        XCTAssertEqual(script.invocations.count, 1, "pair must not retry on network error")
    }

    func testPairRequestBodyEncodesSnakeCase() async throws {
        let data = try loadFixture("pair_200")
        let script = MockTransportScript([.respond(status: 200, data: data)])
        _ = try await client(script: script).pair(
            token: "tk", hostname: "mac-1", daemonVersion: "0.1.0", protocolVersion: 3)
        let body = script.invocations[0].httpBody!
        let dict = try JSONSerialization.jsonObject(with: body) as! [String: Any]
        XCTAssertEqual(dict["pairing_token"] as? String, "tk")
        XCTAssertEqual(dict["hostname"] as? String, "mac-1")
        XCTAssertEqual(dict["daemon_version"] as? String, "0.1.0")
        XCTAssertEqual(dict["protocol_version"] as? Int, 3)
    }
}

// MARK: - async throws helper

func assertThrows<T>(
    _ expression: @autoclosure () async throws -> T,
    file: StaticString = #file,
    line: UInt = #line,
    _ handler: (Error) -> Void
) async {
    do {
        _ = try await expression()
        XCTFail("expected throw", file: file, line: line)
    } catch {
        handler(error)
    }
}
