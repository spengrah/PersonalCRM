// Coverage for AnarlogCursorReset — the configure --reset-cursor
// handshake. The 409-retry contract is the critical asserter;
// success path is exercised too. Daemon-running rejection is
// enforced at the CLI entry point (requireDaemonNotRunning) BEFORE
// calling resetOne, so it's a CLI-layer concern that doesn't reach
// this helper.
import XCTest
@testable import CRMMacLifecycle
@testable import CRMMacPiClient

final class AnarlogCursorResetTests: XCTestCase {

    private let testAuth = PiAuth(
        hostID: UUID(uuidString: "11111111-2222-3333-4444-555555555555")!,
        apiKey: "k")

    // GET response with the current cursor + epoch.
    private func cursorGet(cursor: String, epoch: Int64) -> Data {
        Data("""
        {"success": true, "data": {"cursor": "\(cursor)", "cursor_epoch": \(epoch), "backfill_complete": false}}
        """.utf8)
    }

    // POST 200 success.
    private let commitOK: Data = Data(#"{"success":true,"data":{"ok":true}}"#.utf8)

    // POST 409 conflict.
    private func cursorConflict(currentCursor: String, currentEpoch: Int64) -> Data {
        Data("""
        {"success": false,
         "error": {"code": "BASE_CURSOR_MISMATCH", "message": "stale"},
         "data": {"current_cursor": "\(currentCursor)", "current_epoch": \(currentEpoch)}}
        """.utf8)
    }

    func testSuccessOnFirstTry() async throws {
        // Sequence: GET cursor → POST commit (200).
        let transport = LifecycleMockTransport([
            .respond(status: 200, data: cursorGet(cursor: "old-cursor", epoch: 7)),
            .respond(status: 200, data: commitOK),
        ])
        let client = PiClient(
            baseURL: URL(string: "https://test.invalid")!,
            transport: transport.asTransport(),
            sleep: noopSleep)
        try await AnarlogCursorReset.resetOne(
            client: client, auth: testAuth, source: "anarlog_humans")
        XCTAssertEqual(transport.invocations.count, 2)
        // First invocation is GET; second is POST with cursor="".
        let postBody = try XCTUnwrap(transport.invocations[1].httpBody)
        let parsed = try JSONSerialization.jsonObject(with: postBody) as! [String: Any]
        XCTAssertEqual(parsed["cursor"] as? String, "")
        XCTAssertEqual(parsed["base_cursor"] as? String, "old-cursor")
        XCTAssertEqual(parsed["cursor_epoch"] as? Int, 7)
    }

    func testConflictRefetchesAndRetriesOnce() async throws {
        // Sequence: GET (old) → POST commit (409) → GET (refresh) →
        // POST commit (200). The second commit's base_cursor must use
        // the REFETCHED value, NOT the conflict-body value (that's
        // the regression guard: the prior impl trusted the body).
        let transport = LifecycleMockTransport([
            .respond(status: 200, data: cursorGet(cursor: "stale-cursor", epoch: 5)),
            .respond(status: 409, data: cursorConflict(currentCursor: "body-cursor", currentEpoch: 9)),
            .respond(status: 200, data: cursorGet(cursor: "refetched-cursor", epoch: 11)),
            .respond(status: 200, data: commitOK),
        ])
        let client = PiClient(
            baseURL: URL(string: "https://test.invalid")!,
            transport: transport.asTransport(),
            sleep: noopSleep)
        try await AnarlogCursorReset.resetOne(
            client: client, auth: testAuth, source: "anarlog_sessions")
        XCTAssertEqual(transport.invocations.count, 4)
        // Final POST uses refetched cursor.
        let finalPost = try XCTUnwrap(transport.invocations[3].httpBody)
        let parsed = try JSONSerialization.jsonObject(with: finalPost) as! [String: Any]
        XCTAssertEqual(parsed["base_cursor"] as? String, "refetched-cursor")
        XCTAssertEqual(parsed["cursor_epoch"] as? Int, 11)
    }

    func testSecondConflictPropagates() async throws {
        // A second 409 propagates — the bound is exactly one retry.
        let transport = LifecycleMockTransport([
            .respond(status: 200, data: cursorGet(cursor: "old", epoch: 1)),
            .respond(status: 409, data: cursorConflict(currentCursor: "new", currentEpoch: 2)),
            .respond(status: 200, data: cursorGet(cursor: "refetched", epoch: 3)),
            .respond(status: 409, data: cursorConflict(currentCursor: "newer", currentEpoch: 4)),
        ])
        let client = PiClient(
            baseURL: URL(string: "https://test.invalid")!,
            transport: transport.asTransport(),
            sleep: noopSleep)
        do {
            try await AnarlogCursorReset.resetOne(
                client: client, auth: testAuth, source: "anarlog_humans")
            XCTFail("expected second 409 to propagate")
        } catch PiClientError.cursorConflict {
            // expected
        }
        // Bound is exactly one retry: 4 calls total (GET, POST 409,
        // GET, POST 409).
        XCTAssertEqual(transport.invocations.count, 4)
    }

    func testCommitUsesCorrectSourceInURL() async throws {
        let transport = LifecycleMockTransport([
            .respond(status: 200, data: cursorGet(cursor: "x", epoch: 0)),
            .respond(status: 200, data: commitOK),
        ])
        let client = PiClient(
            baseURL: URL(string: "https://test.invalid")!,
            transport: transport.asTransport(),
            sleep: noopSleep)
        try await AnarlogCursorReset.resetOne(
            client: client, auth: testAuth, source: "anarlog_sessions")
        XCTAssertTrue(transport.invocations.allSatisfy {
            ($0.url?.path ?? "").contains("/sync/anarlog_sessions/cursor")
        })
    }
}
