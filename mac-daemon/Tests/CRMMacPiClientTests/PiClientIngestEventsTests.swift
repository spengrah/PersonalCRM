import XCTest
@testable import CRMMacPiClient

final class PiClientIngestEventsTests: XCTestCase {
    private let auth = PiAuth(
        hostID: UUID(uuidString: "11111111-2222-3333-4444-555555555555")!,
        apiKey: "k")

    private func client(_ script: MockTransportScript) -> PiClient {
        PiClient(
            baseURL: URL(string: "https://pi.example.test")!,
            transport: script.asTransport(),
            sleep: noopSleep)
    }

    private func sampleBody() -> IngestEventsBody {
        let payload = RawJSON(#"{"version":1,"source":"messages"}"#)
        let event = IngestEvent(
            source: "messages",
            sourceID: "guid-1",
            kind: "raw_message.received",
            payload: payload,
            observedAt: Date(timeIntervalSince1970: 1_700_000_000))
        return IngestEventsBody(events: [event])
    }

    // MARK: - happy path

    func testIngestEventsAllAccepted() async throws {
        let response = Data(#"{"accepted":1,"duplicate":0,"rejected":0,"errors":[]}"#.utf8)
        let script = MockTransportScript([.respond(status: 200, data: response)])
        let result = try await client(script).ingestEvents(auth: auth, body: sampleBody())
        XCTAssertEqual(result.accepted, 1)
        XCTAssertEqual(result.duplicate, 0)
        XCTAssertEqual(result.rejected, 0)
        XCTAssertTrue(result.errors.isEmpty)
    }

    func testIngestEventsMixedOutcome() async throws {
        let response = Data(#"""
            {"accepted":1,
             "duplicate":1,
             "rejected":1,
             "errors":[{"index":2,"code":"VALIDATION","message":"bad payload"}]}
            """#.utf8)
        let script = MockTransportScript([.respond(status: 200, data: response)])
        let result = try await client(script).ingestEvents(auth: auth, body: sampleBody())
        XCTAssertEqual(result.accepted, 1)
        XCTAssertEqual(result.duplicate, 1)
        XCTAssertEqual(result.rejected, 1)
        XCTAssertEqual(result.errors.count, 1)
        XCTAssertEqual(result.errors[0].index, 2)
        XCTAssertEqual(result.errors[0].code, "VALIDATION")
    }

    // MARK: - failure modes

    func testIngestEvents401() async {
        let response = Data(#"{"success":false,"error":{"code":"UNKNOWN_HOST","message":"revoked"}}"#.utf8)
        let script = MockTransportScript([.respond(status: 401, data: response)])
        do {
            _ = try await client(script).ingestEvents(auth: auth, body: sampleBody())
            XCTFail("expected throw")
        } catch PiClientError.authenticationRevoked {
            // OK
        } catch {
            XCTFail("got \(error)")
        }
    }

    func testIngestEvents412UpgradeRequired() async {
        let response = Data(#"{"success":false,"error":{"code":"UPGRADE_REQUIRED","message":"upgrade","min_version":2}}"#.utf8)
        let script = MockTransportScript([.respond(status: 412, data: response)])
        do {
            _ = try await client(script).ingestEvents(auth: auth, body: sampleBody())
            XCTFail("expected throw")
        } catch let PiClientError.upgradeRequired(minVersion, _) {
            XCTAssertEqual(minVersion, 2)
        } catch {
            XCTFail("got \(error)")
        }
    }

    func testIngestEvents400ClientError() async {
        let response = Data(#"{"success":false,"error":{"code":"VALIDATION","message":"empty batch"}}"#.utf8)
        let script = MockTransportScript([.respond(status: 400, data: response)])
        do {
            _ = try await client(script).ingestEvents(auth: auth, body: sampleBody())
            XCTFail("expected throw")
        } catch let PiClientError.clientError(status, code, _) {
            XCTAssertEqual(status, 400)
            XCTAssertEqual(code, "VALIDATION")
        } catch {
            XCTFail("got \(error)")
        }
    }

    // MARK: - wire shape

    func testIngestEventWireShape() async throws {
        let response = Data(#"{"accepted":0,"duplicate":0,"rejected":0,"errors":[]}"#.utf8)
        let script = MockTransportScript([.respond(status: 200, data: response)])
        _ = try await client(script).ingestEvents(auth: auth, body: sampleBody())

        XCTAssertEqual(script.invocations.count, 1)
        let req = script.invocations[0]

        // Method + path
        XCTAssertEqual(req.httpMethod, "POST")
        XCTAssertEqual(req.url?.path, "/api/v1/ingest/events")

        // Auth headers
        XCTAssertEqual(req.value(forHTTPHeaderField: "X-Mac-Host-ID"),
                       auth.hostID.uuidString.lowercased())
        XCTAssertEqual(req.value(forHTTPHeaderField: "Authorization"),
                       "Bearer \(auth.apiKey)")

        // Wire body keys
        guard let body = req.httpBody else {
            XCTFail("missing body")
            return
        }
        let bodyString = String(decoding: body, as: UTF8.self)
        XCTAssertTrue(bodyString.contains("\"source_id\""),
                      "wire uses source_id (snake_case)")
        XCTAssertTrue(bodyString.contains("\"observed_at\""),
                      "wire uses observed_at (snake_case)")

        // Payload is inline JSON object, NOT base64 string.
        // The sample payload JSON is `{"version":1,"source":"messages"}`
        // — appears verbatim in the body (modulo key ordering).
        XCTAssertTrue(bodyString.contains("\"payload\":{"),
                      "payload is inline JSON object (NOT base64)")
        XCTAssertTrue(bodyString.contains("\"version\":1"),
                      "payload contents preserved")
        // Defense: NOT base64 — a base64-encoded payload would look like
        // `"payload":"eyJ..."` and would never include a literal `{`.
        XCTAssertFalse(bodyString.contains("\"payload\":\""),
                       "payload must NOT be a base64 string")
    }

    // MARK: - needs_attention decoding

    func testIngestEventsNeedsAttentionAbsentDecodesAsEmpty() async throws {
        // Legacy Pi response (pre-meeting_note) — no needs_attention key.
        let response = Data(#"{"accepted":1,"duplicate":0,"rejected":0,"errors":[]}"#.utf8)
        let script = MockTransportScript([.respond(status: 200, data: response)])
        let result = try await client(script).ingestEvents(auth: auth, body: sampleBody())
        XCTAssertEqual(result.accepted, 1)
        XCTAssertTrue(result.needsAttention.isEmpty)
    }

    func testIngestEventsNeedsAttentionPresentDecodes() async throws {
        let response = Data(#"""
            {"accepted":1,
             "duplicate":0,
             "rejected":0,
             "errors":[],
             "needs_attention":[
                {"session_id":"deadbeef-1111-2222-3333-444455556666","reason":"orphan"},
                {"session_id":"cafebabe-aaaa-bbbb-cccc-ddddeeeeffff","reason":"conflict"}
             ]}
            """#.utf8)
        let script = MockTransportScript([.respond(status: 200, data: response)])
        let result = try await client(script).ingestEvents(auth: auth, body: sampleBody())
        XCTAssertEqual(result.needsAttention.count, 2)
        XCTAssertEqual(result.needsAttention[0].sessionID,
                       "deadbeef-1111-2222-3333-444455556666")
        XCTAssertEqual(result.needsAttention[0].reason, "orphan")
        XCTAssertEqual(result.needsAttention[1].sessionID,
                       "cafebabe-aaaa-bbbb-cccc-ddddeeeeffff")
        XCTAssertEqual(result.needsAttention[1].reason, "conflict")
    }

    func testIngestEventsNeedsAttentionToleratesUnknownFields() async throws {
        // Forward-compat: a future Pi adding fields to the items or to
        // the top-level response should still decode cleanly.
        let response = Data(#"""
            {"accepted":1,
             "duplicate":0,
             "rejected":0,
             "errors":[],
             "needs_attention":[
                {"session_id":"deadbeef-1111-2222-3333-444455556666",
                 "reason":"orphan",
                 "future_field":"ignored"}
             ],
             "future_top_level":"ignored"}
            """#.utf8)
        let script = MockTransportScript([.respond(status: 200, data: response)])
        let result = try await client(script).ingestEvents(auth: auth, body: sampleBody())
        XCTAssertEqual(result.needsAttention.count, 1)
        XCTAssertEqual(result.needsAttention[0].reason, "orphan")
    }

    func testEmptyPayloadRejectedAtEncode() {
        let bad = RawJSON(Data())
        let event = IngestEvent(source: "s", sourceID: "x", kind: "k",
                                  payload: bad,
                                  observedAt: Date(timeIntervalSince1970: 0))
        let body = IngestEventsBody(events: [event])
        let encoder = JSONEncoder()
        encoder.dateEncodingStrategy = .iso8601
        do {
            _ = try encoder.encode(body)
            XCTFail("expected encode failure for malformed RawJSON")
        } catch {
            // Expected.
        }
    }
}
