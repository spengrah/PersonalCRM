import XCTest
@testable import CRMMacPhoneCallsSource

final class PayloadShapingTests: XCTestCase {
    private let hostID = UUID(uuidString: "11111111-2222-3333-4444-555555555555")!

    func testInboundAnsweredCallShapes() throws {
        let row = CallHistoryRow(
            zPK: 1, uniqueID: "abc-123",
            address: "+1 (555) 123-4567",
            originated: false, answered: true,
            duration: 30,
            serviceProvider: "com.apple.Telephony", callType: 0,
            hasMessage: false,
            startedAt: Date(timeIntervalSince1970: 1_750_000_000))
        let result = CallPayloadShaping.shape(
            row: row, peerNormalized: "+15551234567", hostID: hostID)
        XCTAssertNotNil(result)
        let (kind, payload) = result!
        XCTAssertEqual(kind, .received)
        XCTAssertEqual(payload.direction, "inbound")
        XCTAssertEqual(payload.service, .voice)
        XCTAssertEqual(payload.callUniqueID, "abc-123")
        XCTAssertEqual(payload.peerHandle, "+1 (555) 123-4567")
        XCTAssertEqual(payload.peerNormalized, "+15551234567")
        XCTAssertEqual(payload.answered, true)
        XCTAssertEqual(payload.hasVoicemail, false)
        XCTAssertEqual(payload.durationSeconds, 30)
    }

    func testInboundVoicemailShapes() throws {
        let row = CallHistoryRow(
            zPK: 2, uniqueID: "vm-1",
            address: "alice@example.com",
            originated: false, answered: false,
            duration: 25,
            serviceProvider: "com.apple.FaceTime", callType: 8,
            hasMessage: true,
            startedAt: Date(timeIntervalSince1970: 1_750_000_100))
        let result = CallPayloadShaping.shape(
            row: row, peerNormalized: "alice@example.com", hostID: hostID)
        let (kind, payload) = result!
        XCTAssertEqual(kind, .received)
        XCTAssertEqual(payload.service, .facetimeAudio)
        XCTAssertEqual(payload.answered, false)
        XCTAssertEqual(payload.hasVoicemail, true)
    }

    func testOutboundForcesAnsweredNilAndHasVoicemailFalse() throws {
        // Even if source row has answered=true and hasMessage=true,
        // shape() forces NULL/false for outbound.
        let row = CallHistoryRow(
            zPK: 3, uniqueID: "out-1",
            address: "+15551234567",
            originated: true, answered: true,
            duration: 60,
            serviceProvider: "com.apple.Telephony", callType: 0,
            hasMessage: true,
            startedAt: Date(timeIntervalSince1970: 1_750_000_200))
        let result = CallPayloadShaping.shape(
            row: row, peerNormalized: "+15551234567", hostID: hostID)
        let (kind, payload) = result!
        XCTAssertEqual(kind, .sent)
        XCTAssertEqual(payload.direction, "outbound")
        XCTAssertNil(payload.answered)
        XCTAssertEqual(payload.hasVoicemail, false)
    }

    func testServiceUnknownReturnsNil() throws {
        let row = CallHistoryRow(
            zPK: 4, uniqueID: "unk-1",
            address: "+15551234567",
            originated: false, answered: true,
            duration: 30,
            serviceProvider: "com.apple.UnknownService", callType: 99,
            hasMessage: false,
            startedAt: Date())
        let result = CallPayloadShaping.shape(
            row: row, peerNormalized: "+15551234567", hostID: hostID)
        XCTAssertNil(result)
    }

    func testJSONEncodingMatchesWireShape() throws {
        let row = CallHistoryRow(
            zPK: 1, uniqueID: "abc-123",
            address: "+15551234567",
            originated: false, answered: true,
            duration: 30,
            serviceProvider: "com.apple.Telephony", callType: 0,
            hasMessage: false,
            startedAt: Date(timeIntervalSince1970: 1_750_000_000))
        let (_, payload) = CallPayloadShaping.shape(
            row: row, peerNormalized: "+15551234567", hostID: hostID)!

        let encoder = JSONEncoder()
        encoder.dateEncodingStrategy = .iso8601
        encoder.outputFormatting = [.sortedKeys, .withoutEscapingSlashes]
        let data = try encoder.encode(payload)
        let json = String(decoding: data, as: UTF8.self)

        // Check the wire keys are snake_case and the host_id is
        // lowercase (Go wire-shape parity).
        XCTAssertTrue(json.contains("\"call_unique_id\":\"abc-123\""), json)
        XCTAssertTrue(json.contains("\"peer_handle\":\"+15551234567\""), json)
        XCTAssertTrue(json.contains("\"peer_normalized\":\"+15551234567\""), json)
        XCTAssertTrue(json.contains("\"service\":\"voice\""), json)
        XCTAssertTrue(json.contains("\"direction\":\"inbound\""), json)
        XCTAssertTrue(json.contains("\"has_voicemail\":false"), json)
        XCTAssertTrue(json.contains("\"duration_seconds\":30"), json)
        XCTAssertTrue(json.contains("\"host_id\":\"11111111-2222-3333-4444-555555555555\""), json)
        XCTAssertTrue(json.contains("\"source\":\"phone_calls\""), json)
        XCTAssertTrue(json.contains("\"version\":1"), json)
    }

    func testOutboundJSONOmitsAnsweredKey() throws {
        // answered = nil for outbound; the encoder must omit the key
        // entirely so the Pi's `omitempty` JSON tag receives no value.
        let row = CallHistoryRow(
            zPK: 1, uniqueID: "out-1",
            address: "+15551234567",
            originated: true, answered: nil,
            duration: 0,
            serviceProvider: "com.apple.Telephony", callType: 0,
            hasMessage: false,
            startedAt: Date(timeIntervalSince1970: 1_750_000_000))
        let (_, payload) = CallPayloadShaping.shape(
            row: row, peerNormalized: "+15551234567", hostID: hostID)!
        let data = try JSONEncoder().encode(payload)
        let json = String(decoding: data, as: UTF8.self)
        XCTAssertFalse(json.contains("answered"), "outbound must omit answered key, got \(json)")
    }

    func testDirectionDiscriminatorRawValues() {
        // The IngestEvent.kind on the wire matches these raw values.
        XCTAssertEqual(CallDirection.received.rawValue, "call.received")
        XCTAssertEqual(CallDirection.sent.rawValue, "call.sent")
    }
}
