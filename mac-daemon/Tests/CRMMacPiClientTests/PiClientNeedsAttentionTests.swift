// Coverage for PiClient.needsAttention(auth:hostID:) — the daemon-side
// HTTP client for GET /api/v1/meeting-notes/needs-attention.
//
// Pins the wire shape against the Pi-side handler at
// backend/internal/api/handlers/meeting_note.go: enveloped via
// api.SendSuccess, NeedsAttentionItemResponse fields. Tolerates
// extra fields the daemon ignores (summary_excerpt, candidates,
// mac_host_id, etc.) so future Pi-side additions don't break the
// daemon decoder.
import XCTest
@testable import CRMMacPiClient

final class PiClientNeedsAttentionTests: XCTestCase {
    private let auth = PiAuth(
        hostID: UUID(uuidString: "11111111-2222-3333-4444-555555555555")!,
        apiKey: "k")

    private func client(_ script: MockTransportScript) -> PiClient {
        PiClient(
            baseURL: URL(string: "https://pi.example.test")!,
            transport: script.asTransport(),
            sleep: noopSleep)
    }

    // MARK: - wire shape

    func testRequestPathAndQuery() async throws {
        let response = Data(#"{"success":true,"data":[]}"#.utf8)
        let script = MockTransportScript([.respond(status: 200, data: response)])
        _ = try await client(script).needsAttention(auth: auth, hostID: auth.hostID)
        XCTAssertEqual(script.invocations.count, 1)
        let req = script.invocations[0]
        XCTAssertEqual(req.httpMethod, "GET")
        XCTAssertNil(req.httpBody)
        XCTAssertEqual(req.url?.path, "/api/v1/meeting-notes/needs-attention")
        let comps = URLComponents(url: req.url!, resolvingAgainstBaseURL: false)
        let q = comps?.queryItems ?? []
        XCTAssertEqual(q.count, 1)
        XCTAssertEqual(q[0].name, "host_id")
        XCTAssertEqual(q[0].value, auth.hostID.uuidString.lowercased())
    }

    func testRequestSendsHostAuthHeadersNotApiKey() async throws {
        // Regression test pinning the host-auth header shape. The
        // daemon must NEVER send X-API-Key for this endpoint — it
        // doesn't possess the global API key. A future refactor
        // adding X-API-Key would silently 401 in production.
        let response = Data(#"{"success":true,"data":[]}"#.utf8)
        let script = MockTransportScript([.respond(status: 200, data: response)])
        _ = try await client(script).needsAttention(auth: auth, hostID: auth.hostID)
        let req = script.invocations[0]
        XCTAssertEqual(req.value(forHTTPHeaderField: "X-Mac-Host-ID"),
                       auth.hostID.uuidString.lowercased())
        XCTAssertEqual(req.value(forHTTPHeaderField: "Authorization"),
                       "Bearer \(auth.apiKey)")
        XCTAssertNil(req.value(forHTTPHeaderField: "X-API-Key"))
    }

    // MARK: - happy-path decoding

    func testDecodesEmptyArray() async throws {
        let response = Data(#"{"success":true,"data":[]}"#.utf8)
        let script = MockTransportScript([.respond(status: 200, data: response)])
        let result = try await client(script).needsAttention(auth: auth, hostID: auth.hostID)
        XCTAssertTrue(result.isEmpty)
    }

    func testDecodesMixedConflictAndOrphanItems() async throws {
        // Both linkage_state values present in one response. Extra
        // fields (summary_excerpt, candidates, mac_host_id) must
        // decode without error.
        let response = Data(#"""
            {"success":true,
             "data":[
                {"id":"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
                 "anarlog_session_id":"11111111-1111-1111-1111-111111111111",
                 "mac_host_id":"22222222-2222-2222-2222-222222222222",
                 "title":"Synthetic Session A",
                 "summary_excerpt":"A short excerpt that the daemon ignores.",
                 "meeting_at":"2026-05-27T14:00:00Z",
                 "linkage_state":"conflict_pending",
                 "candidates":[
                    {"kind":"event",
                     "id":"33333333-3333-3333-3333-333333333333",
                     "occurred_at":"2026-05-27T14:00:00Z",
                     "overlap_count":1,
                     "target_missing":false,
                     "preview":{"title":"Calendar event A"}}
                 ]},
                {"id":"bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",
                 "anarlog_session_id":"44444444-4444-4444-4444-444444444444",
                 "mac_host_id":null,
                 "title":null,
                 "summary_excerpt":null,
                 "meeting_at":"2026-05-26T19:30:00Z",
                 "linkage_state":"orphan_needs_review",
                 "candidates":null}
             ]}
            """#.utf8)
        let script = MockTransportScript([.respond(status: 200, data: response)])
        let result = try await client(script).needsAttention(auth: auth, hostID: auth.hostID)
        XCTAssertEqual(result.count, 2)
        XCTAssertEqual(result[0].linkageState, "conflict_pending")
        XCTAssertEqual(result[0].title, "Synthetic Session A")
        XCTAssertEqual(result[0].meetingAt, "2026-05-27T14:00:00Z")
        XCTAssertEqual(result[0].anarlogSessionID,
                       UUID(uuidString: "11111111-1111-1111-1111-111111111111"))
        XCTAssertEqual(result[1].linkageState, "orphan_needs_review")
        XCTAssertNil(result[1].title)
        XCTAssertEqual(result[1].anarlogSessionID,
                       UUID(uuidString: "44444444-4444-4444-4444-444444444444"))
    }

    // MARK: - failure modes

    func testMaps401ToAuthenticationRevoked() async {
        let response = Data(#"{"success":false,"error":{"code":"UNAUTHORIZED","message":"revoked"}}"#.utf8)
        let script = MockTransportScript([.respond(status: 401, data: response)])
        await assertThrows({
            _ = try await client(script).needsAttention(auth: auth, hostID: auth.hostID)
        }) { error in
            guard case PiClientError.authenticationRevoked = error else {
                XCTFail("got \(error)")
                return
            }
        }
    }

    func testMaps5xxToServerError() async {
        let response = Data(#"{"success":false,"error":{"code":"INTERNAL_ERROR","message":"boom"}}"#.utf8)
        // RetryingTransport retries 5xx 5 times (so 6 attempts total)
        // before surfacing the error. Script enough responses to
        // exhaust the retry budget.
        let script = MockTransportScript(Array(repeating: .respond(status: 500, data: response), count: 6))
        await assertThrows({
            _ = try await client(script).needsAttention(auth: auth, hostID: auth.hostID)
        }) { error in
            guard case let PiClientError.serverError(status, _) = error else {
                XCTFail("got \(error)")
                return
            }
            XCTAssertEqual(status, 500)
        }
    }

    func testMaps400ToClientError() async {
        // E.g. invalid host_id query param.
        let response = Data(#"{"success":false,"error":{"code":"VALIDATION_ERROR","message":"bad host_id"}}"#.utf8)
        let script = MockTransportScript([.respond(status: 400, data: response)])
        await assertThrows({
            _ = try await client(script).needsAttention(auth: auth, hostID: auth.hostID)
        }) { error in
            guard case let PiClientError.clientError(status, code, _) = error else {
                XCTFail("got \(error)")
                return
            }
            XCTAssertEqual(status, 400)
            XCTAssertEqual(code, "VALIDATION_ERROR")
        }
    }
}
