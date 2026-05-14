import Foundation
@testable import CRMMacPiClient

/// Lightweight scriptable transport reused across lifecycle tests.
/// Mirrors the helper in CRMMacPiClientTests but lives in this test
/// target — SwiftPM doesn't share test code across targets.
final class LifecycleMockTransport: @unchecked Sendable {
    enum Step: Sendable {
        case respond(status: Int, data: Data)
        case fail(URLError)
    }

    private var steps: [Step]
    private(set) var invocations: [URLRequest] = []

    init(_ steps: [Step]) {
        self.steps = steps
    }

    func asTransport() -> TransportFunc {
        // Strong capture: tests create the transport inline
        // (`LifecycleMockTransport([...]).asTransport()`) and rely on
        // the closure keeping the script alive. A weak capture would
        // let the script deallocate before the PiClient invokes the
        // transport, making every request fail with URLError(.cancelled).
        return { request in
            self.invocations.append(request)
            guard !self.steps.isEmpty else {
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
            case .fail(let err):
                throw err
            }
        }
    }
}

@Sendable func noopSleep(_ delay: TimeInterval) async throws {}

let pair200JSON: Data = Data("""
{"success": true, "data": {"host_id":"11111111-2222-3333-4444-555555555555","api_key":"k","cursor_epoch":1}}
""".utf8)

let pair410JSON: Data = Data("""
{"success": false, "error": {"code": "PAIRING_TOKEN_INVALID", "message": "invalid"}}
""".utf8)

let pair409JSON: Data = Data("""
{"success": false, "error": {"code": "HOST_ALREADY_PAIRED", "message": "already paired"}}
""".utf8)

let heartbeat200JSON: Data = Data("""
{"success": true, "data": {"ok": true, "cursor_epoch": 1, "protocol_version": 1, "min_protocol_version": 1}}
""".utf8)

let heartbeat401JSON: Data = Data("""
{"success": false, "error": {"code": "UNKNOWN_HOST", "message": "revoked"}}
""".utf8)

let heartbeat412JSON: Data = Data("""
{"success": false, "error": {"code": "UPGRADE_REQUIRED", "message": "upgrade required", "min_version": 2, "upgrade_required": true}}
""".utf8)

let known200JSON: Data = Data("""
{"success": true, "data": {"phones": ["+15555550100"], "emails": ["bob@test"]}}
""".utf8)
