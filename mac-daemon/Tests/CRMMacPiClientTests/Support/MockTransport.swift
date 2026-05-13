import Foundation
import XCTest
@testable import CRMMacPiClient

/// Per-test transport function. Returns a (status, data) tuple for
/// each invocation, or throws to simulate a transport failure. Tests
/// pre-seed the script and consume in order; if the script runs out
/// we fail loudly.
final class MockTransportScript {
    enum Step {
        case respond(status: Int, data: Data)
        case fail(URLError)
    }

    private var steps: [Step]
    private(set) var invocations: [URLRequest] = []
    private let onMissingScript: (URLRequest) -> Void

    init(_ steps: [Step], onMissingScript: @escaping (URLRequest) -> Void = { _ in
        XCTFail("MockTransportScript ran out of scripted steps")
    }) {
        self.steps = steps
        self.onMissingScript = onMissingScript
    }

    func asTransport() -> TransportFunc {
        return { [weak self] request in
            guard let self else {
                throw URLError(.cancelled)
            }
            self.invocations.append(request)
            guard !self.steps.isEmpty else {
                self.onMissingScript(request)
                throw URLError(.unknown)
            }
            let step = self.steps.removeFirst()
            switch step {
            case .respond(let status, let data):
                let url = request.url ?? URL(string: "https://test.invalid")!
                let response = HTTPURLResponse(
                    url: url,
                    statusCode: status,
                    httpVersion: "HTTP/1.1",
                    headerFields: ["Content-Type": "application/json"])!
                return (data, response)
            case .fail(let urlError):
                throw urlError
            }
        }
    }
}

/// No-op sleep — keeps RetryingTransport tests fast.
func noopSleep(_ delay: TimeInterval) async throws {}

/// Loads a JSON fixture from this test target's bundle resources.
func loadFixture(_ name: String, file: StaticString = #file, line: UInt = #line) throws -> Data {
    guard let url = Bundle.module.url(forResource: name, withExtension: "json", subdirectory: "Fixtures") else {
        XCTFail("missing fixture \(name).json", file: file, line: line)
        throw URLError(.fileDoesNotExist)
    }
    return try Data(contentsOf: url)
}
