import XCTest
@testable import CRMMacPiClient

final class PiClientCursorEndpointsTests: XCTestCase {
    private let auth = PiAuth(
        hostID: UUID(uuidString: "11111111-2222-3333-4444-555555555555")!,
        apiKey: "k")

    private func client(_ script: MockTransportScript) -> PiClient {
        PiClient(
            baseURL: URL(string: "https://pi.example.test")!,
            transport: script.asTransport(),
            sleep: noopSleep)
    }

    // MARK: - GET

    func testGetCursorHappyPath() async throws {
        let response = Data(#"""
            {"success":true,
             "data":{"cursor":"{\"backfill_cursor\":42}",
                     "cursor_epoch":7,
                     "backfill_complete":false}}
            """#.utf8)
        let script = MockTransportScript([.respond(status: 200, data: response)])
        let state = try await client(script).getCursor(auth: auth, source: "messages")
        XCTAssertEqual(state.cursor, "{\"backfill_cursor\":42}")
        XCTAssertEqual(state.cursorEpoch, 7)
        XCTAssertFalse(state.backfillComplete)
    }

    func testGetCursorFreshInstallEmptyCursor() async throws {
        let response = Data(#"""
            {"success":true,
             "data":{"cursor":"","cursor_epoch":0,"backfill_complete":false}}
            """#.utf8)
        let script = MockTransportScript([.respond(status: 200, data: response)])
        let state = try await client(script).getCursor(auth: auth, source: "messages")
        XCTAssertEqual(state.cursor, "")
        XCTAssertEqual(state.cursorEpoch, 0)
    }

    func testGetCursorPath() async throws {
        let response = Data(#"{"success":true,"data":{"cursor":"","cursor_epoch":1,"backfill_complete":false}}"#.utf8)
        let script = MockTransportScript([.respond(status: 200, data: response)])
        _ = try await client(script).getCursor(auth: auth, source: "messages")
        XCTAssertEqual(script.invocations[0].httpMethod, "GET")
        XCTAssertEqual(
            script.invocations[0].url?.path,
            "/api/v1/host/\(auth.hostID.uuidString.lowercased())/sync/messages/cursor")
    }

    func testGetCursor401() async {
        let response = Data(#"{"success":false,"error":{"code":"UNKNOWN_HOST","message":"revoked"}}"#.utf8)
        let script = MockTransportScript([.respond(status: 401, data: response)])
        do {
            _ = try await client(script).getCursor(auth: auth, source: "messages")
            XCTFail("expected throw")
        } catch PiClientError.authenticationRevoked {
            // OK
        } catch {
            XCTFail("got \(error)")
        }
    }

    // MARK: - POST

    func testCommitCursorHappyPath() async throws {
        let response = Data(#"{"success":true,"data":{"ok":true}}"#.utf8)
        let script = MockTransportScript([.respond(status: 200, data: response)])
        try await client(script).commitCursor(
            auth: auth,
            source: "messages",
            cursor: "next-cursor",
            baseCursor: "base-cursor",
            cursorEpoch: 7,
            backfillComplete: false)

        let req = script.invocations[0]
        XCTAssertEqual(req.httpMethod, "POST")
        XCTAssertEqual(req.url?.path,
                       "/api/v1/host/\(auth.hostID.uuidString.lowercased())/sync/messages/cursor")
        guard let body = req.httpBody else { XCTFail("missing body"); return }
        let s = String(decoding: body, as: UTF8.self)
        XCTAssertTrue(s.contains("\"cursor\":\"next-cursor\""))
        XCTAssertTrue(s.contains("\"base_cursor\":\"base-cursor\""))
        XCTAssertTrue(s.contains("\"cursor_epoch\":7"))
        XCTAssertTrue(s.contains("\"backfill_complete\":false"))
    }

    func testCommitCursorConflictEpochMismatch() async {
        let response = Data(#"""
            {"success":false,
             "error":{"code":"EPOCH_MISMATCH","message":"epoch mismatch"},
             "data":{"current_cursor":"abc","current_epoch":9}}
            """#.utf8)
        let script = MockTransportScript([.respond(status: 409, data: response)])
        do {
            try await client(script).commitCursor(
                auth: auth, source: "messages",
                cursor: "n", baseCursor: "b", cursorEpoch: 7, backfillComplete: false)
            XCTFail("expected throw")
        } catch let PiClientError.cursorConflict(code, current) {
            XCTAssertEqual(code, .epochMismatch)
            XCTAssertEqual(current.currentCursor, "abc")
            XCTAssertEqual(current.currentEpoch, 9)
        } catch {
            XCTFail("got \(error)")
        }
    }

    func testCommitCursorConflictBaseMismatch() async {
        let response = Data(#"""
            {"success":false,
             "error":{"code":"BASE_CURSOR_MISMATCH","message":"base mismatch"},
             "data":{"current_cursor":"xyz","current_epoch":7}}
            """#.utf8)
        let script = MockTransportScript([.respond(status: 409, data: response)])
        do {
            try await client(script).commitCursor(
                auth: auth, source: "messages",
                cursor: "n", baseCursor: "b", cursorEpoch: 7, backfillComplete: false)
            XCTFail("expected throw")
        } catch let PiClientError.cursorConflict(code, current) {
            XCTAssertEqual(code, .baseCursorMismatch)
            XCTAssertEqual(current.currentCursor, "xyz")
            XCTAssertEqual(current.currentEpoch, 7)
        } catch {
            XCTFail("got \(error)")
        }
    }

    func testCommitCursor401() async {
        let response = Data(#"{"success":false,"error":{"code":"UNKNOWN_HOST","message":"revoked"}}"#.utf8)
        let script = MockTransportScript([.respond(status: 401, data: response)])
        do {
            try await client(script).commitCursor(
                auth: auth, source: "messages",
                cursor: "n", baseCursor: "b", cursorEpoch: 7, backfillComplete: false)
            XCTFail("expected throw")
        } catch PiClientError.authenticationRevoked {
            // OK
        } catch {
            XCTFail("got \(error)")
        }
    }
}
