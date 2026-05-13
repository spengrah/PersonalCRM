import XCTest
@testable import CRMMacPiClient
@testable import CRMMacCore

final class RetryingTransportTests: XCTestCase {
    private func makeTransport(script: MockTransportScript, policy: BackoffPolicy = BackoffPolicy(maxRetries: 5)) -> RetryingTransport {
        RetryingTransport(transport: script.asTransport(), policy: policy, sleep: noopSleep)
    }

    private func request() -> URLRequest {
        URLRequest(url: URL(string: "https://pi.example.test/api/v1/host")!)
    }

    func test4xxNotRetried() async throws {
        let script = MockTransportScript([
            .respond(status: 400, data: Data("{}".utf8)),
        ])
        let transport = makeTransport(script: script)
        let (_, http) = try await transport.send(request())
        XCTAssertEqual(http.statusCode, 400)
        XCTAssertEqual(script.invocations.count, 1)
    }

    func test429NotRetried() async throws {
        let script = MockTransportScript([
            .respond(status: 429, data: Data("{}".utf8)),
        ])
        let transport = makeTransport(script: script)
        let (_, http) = try await transport.send(request())
        XCTAssertEqual(http.statusCode, 429)
        XCTAssertEqual(script.invocations.count, 1)
    }

    func test5xxRetriedUpToLimit() async throws {
        // 5 retries + initial = 6 attempts
        let script = MockTransportScript(Array(repeating: .respond(status: 503, data: Data("{}".utf8)), count: 6))
        let transport = makeTransport(script: script)
        await assertThrows(transport.send(request())) { error in
            guard case PiClientError.serverError = error else {
                XCTFail("got \(error)")
                return
            }
        }
        XCTAssertEqual(script.invocations.count, 6)
    }

    func testZeroMaxRetriesSurfacesImmediately() async throws {
        let script = MockTransportScript([
            .respond(status: 502, data: Data("{}".utf8)),
        ])
        let transport = makeTransport(script: script)
        await assertThrows(transport.send(request(), maxRetries: 0)) { error in
            guard case PiClientError.serverError = error else {
                XCTFail("got \(error)")
                return
            }
        }
        XCTAssertEqual(script.invocations.count, 1)
    }

    func testNetworkErrorRetried() async throws {
        let script = MockTransportScript([
            .fail(URLError(.timedOut)),
            .respond(status: 200, data: Data("{\"success\":true}".utf8)),
        ])
        let transport = makeTransport(script: script)
        let (_, http) = try await transport.send(request())
        XCTAssertEqual(http.statusCode, 200)
        XCTAssertEqual(script.invocations.count, 2)
    }

    func testServerErrorMessageExtractedFromEnvelope() async throws {
        let body = Data("""
        {"success": false, "error": {"code": "INTERNAL_ERROR", "message": "kaboom"}}
        """.utf8)
        let script = MockTransportScript(Array(repeating: .respond(status: 500, data: body), count: 6))
        let transport = makeTransport(script: script)
        await assertThrows(transport.send(request())) { error in
            guard case let PiClientError.serverError(_, message) = error else {
                XCTFail("got \(error)")
                return
            }
            XCTAssertEqual(message, "kaboom")
        }
    }
}
