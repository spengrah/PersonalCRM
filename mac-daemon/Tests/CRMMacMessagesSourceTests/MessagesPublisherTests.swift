import XCTest
import CRMMacCore
import CRMMacPiClient
@testable import CRMMacMessagesSource

final class MessagesPublisherTests: XCTestCase {
    private let auth = PiAuth(
        hostID: UUID(uuidString: "11111111-2222-3333-4444-555555555555")!,
        apiKey: "k")

    private func makeItem(rowID: Int64, guid: String = UUID().uuidString,
                          text: String = "hi") -> PublishItem {
        let payload = RawMessagePayload(
            hostID: auth.hostID,
            guid: guid, chatID: "chat",
            peerHandle: "+15551234567",
            text: text,
            messageType: .text,
            isGroup: false,
            sentAt: Date(timeIntervalSince1970: 1_700_000_000))
        return PublishItem(rowID: rowID, direction: .received, payload: payload)
    }

    // MARK: - happy path

    func testEmptyInputReturnsEmptyOutcome() async {
        let publisher = MessagesPublisher(
            sender: { _, _ in
                XCTFail("sender should not be called for empty input")
                return IngestEventsData(accepted: 0, duplicate: 0, rejected: 0, errors: [])
            },
            auth: auth, logger: NoopLogger())
        let outcome = await publisher.publish(items: [])
        XCTAssertEqual(outcome.accepted, 0)
        XCTAssertEqual(outcome.duplicate, 0)
        XCTAssertTrue(outcome.rejected.isEmpty)
        XCTAssertNil(outcome.advanceTo)
    }

    func testAllAcceptedAdvancesCursor() async {
        let publisher = MessagesPublisher(
            sender: { _, body in
                IngestEventsData(accepted: body.events.count,
                                 duplicate: 0, rejected: 0, errors: [])
            },
            auth: auth, logger: NoopLogger())
        let items = (1...3).map { makeItem(rowID: Int64($0)) }
        let outcome = await publisher.publish(items: items)
        XCTAssertEqual(outcome.accepted, 3)
        XCTAssertEqual(outcome.advanceTo, 3,
                       "cursor should advance to the highest ROWID in batch")
    }

    func testAllDuplicatesAdvancesCursor() async {
        // Plan §R5: dedup counts the same as accepted for cursor advance.
        let publisher = MessagesPublisher(
            sender: { _, body in
                IngestEventsData(accepted: 0,
                                 duplicate: body.events.count,
                                 rejected: 0, errors: [])
            },
            auth: auth, logger: NoopLogger())
        let items = (1...2).map { makeItem(rowID: Int64($0)) }
        let outcome = await publisher.publish(items: items)
        XCTAssertEqual(outcome.duplicate, 2)
        XCTAssertEqual(outcome.advanceTo, 2)
    }

    // MARK: - cursor advance gating

    func testCursorAdvanceOnlyOnCleanBatch() async {
        // Mixed accepted + rejected: cursor must NOT advance.
        let publisher = MessagesPublisher(
            sender: { _, body in
                IngestEventsData(
                    accepted: body.events.count - 1,
                    duplicate: 0,
                    rejected: 1,
                    errors: [IngestEventError(index: 1,
                                                code: "VALIDATION",
                                                message: "bad payload")])
            },
            auth: auth, logger: NoopLogger())
        let items = (1...3).map { makeItem(rowID: Int64($0)) }
        let outcome = await publisher.publish(items: items)
        XCTAssertEqual(outcome.accepted, 2)
        XCTAssertEqual(outcome.rejected.count, 1)
        XCTAssertNil(outcome.advanceTo,
                     "cursor must NOT advance with any per-event rejection")
    }

    func testAllRejectedNoAdvance() async {
        let publisher = MessagesPublisher(
            sender: { _, body in
                let errs = body.events.enumerated().map { i, _ in
                    IngestEventError(index: i, code: "VALIDATION",
                                       message: "bad payload")
                }
                return IngestEventsData(accepted: 0, duplicate: 0,
                                         rejected: body.events.count,
                                         errors: errs)
            },
            auth: auth, logger: NoopLogger())
        let items = (1...3).map { makeItem(rowID: Int64($0)) }
        let outcome = await publisher.publish(items: items)
        XCTAssertEqual(outcome.rejected.count, 3)
        XCTAssertNil(outcome.advanceTo)
    }

    // MARK: - batching

    func testEventCountSplitsBatch() async {
        nonisolated(unsafe) var callCount = 0
        let publisher = MessagesPublisher(
            sender: { _, body in
                callCount += 1
                return IngestEventsData(accepted: body.events.count,
                                         duplicate: 0, rejected: 0, errors: [])
            },
            auth: auth, logger: NoopLogger())
        // 250 items -> two batches (200 + 50)
        let items = (1...250).map { makeItem(rowID: Int64($0)) }
        _ = await publisher.publish(items: items)
        XCTAssertEqual(callCount, 2, "250 items should split into 2 batches")
    }

    func testTransportFailureHoldsCursor() async {
        struct NetError: Error {}
        let publisher = MessagesPublisher(
            sender: { _, _ in
                throw NetError()
            },
            auth: auth, logger: NoopLogger())
        let items = (1...3).map { makeItem(rowID: Int64($0)) }
        let outcome = await publisher.publish(items: items)
        XCTAssertEqual(outcome.accepted, 0)
        XCTAssertTrue(outcome.rejected.isEmpty,
                      "transport failure means no per-event rejections — only no advance")
        XCTAssertEqual(outcome.unconfirmed, 3,
                       "all items must be counted as unconfirmed on transport failure")
        XCTAssertNil(outcome.advanceTo)
    }

    /// Partial transport failure: batch 1 succeeds, batch 2 fails.
    /// advanceTo carries the highest ROWID from batch 1, but unconfirmed
    /// is non-zero so the caller MUST NOT advance the cursor.
    func testPartialSuccessReportsUnconfirmed() async {
        nonisolated(unsafe) var callCount = 0
        struct NetError: Error {}
        let publisher = MessagesPublisher(
            sender: { _, body in
                callCount += 1
                if callCount == 1 {
                    return IngestEventsData(accepted: body.events.count,
                                            duplicate: 0, rejected: 0,
                                            errors: [])
                }
                throw NetError()
            },
            auth: auth, logger: NoopLogger())
        // 250 items -> two batches (200 + 50). Batch 2 fails.
        let items = (1...250).map { makeItem(rowID: Int64($0)) }
        let outcome = await publisher.publish(items: items)
        XCTAssertEqual(callCount, 2)
        XCTAssertEqual(outcome.accepted, 200,
                       "first batch confirms")
        XCTAssertEqual(outcome.unconfirmed, 50,
                       "second batch's items are unconfirmed")
        XCTAssertEqual(outcome.advanceTo, 200,
                       "advanceTo is highest confirmed ROWID")
        // Caller MUST gate on (rejected.isEmpty AND unconfirmed == 0).
        XCTAssertFalse(outcome.rejected.isEmpty == false && outcome.unconfirmed > 0)
    }

    // MARK: - wire shape

    func testIngestEventWireShape() async throws {
        nonisolated(unsafe) var capturedBody: IngestEventsBody?
        let publisher = MessagesPublisher(
            sender: { _, body in
                capturedBody = body
                return IngestEventsData(accepted: body.events.count,
                                         duplicate: 0, rejected: 0, errors: [])
            },
            auth: auth, logger: NoopLogger())
        let item = makeItem(rowID: 1, guid: "g-1", text: "hello")
        _ = await publisher.publish(items: [item])

        guard let body = capturedBody else {
            XCTFail("sender not invoked")
            return
        }
        XCTAssertEqual(body.events.count, 1)
        XCTAssertEqual(body.events[0].source, "messages")
        XCTAssertEqual(body.events[0].sourceID, "g-1",
                       "sourceID must match payload.guid")
        XCTAssertEqual(body.events[0].kind, "raw_message.received")

        // Encode + spot-check key shape.
        let encoder = JSONEncoder()
        encoder.dateEncodingStrategy = .iso8601
        let encoded = try encoder.encode(body)
        let json = String(decoding: encoded, as: UTF8.self)
        XCTAssertTrue(json.contains("\"source_id\""))
        XCTAssertTrue(json.contains("\"observed_at\""))
        XCTAssertTrue(json.contains("\"payload\":{"),
                      "payload must be inline JSON object (NOT base64)")
    }

    func testOutboundDirection() async {
        nonisolated(unsafe) var captured: IngestEventsBody?
        let publisher = MessagesPublisher(
            sender: { _, body in
                captured = body
                return IngestEventsData(accepted: 1, duplicate: 0,
                                         rejected: 0, errors: [])
            },
            auth: auth, logger: NoopLogger())
        let payload = RawMessagePayload(
            hostID: auth.hostID,
            guid: "g", chatID: "c", peerHandle: "p", text: "out",
            messageType: .text, isGroup: false,
            sentAt: Date(timeIntervalSince1970: 0))
        let item = PublishItem(rowID: 1, direction: .sent, payload: payload)
        _ = await publisher.publish(items: [item])
        XCTAssertEqual(captured?.events[0].kind, "raw_message.sent")
    }
}
