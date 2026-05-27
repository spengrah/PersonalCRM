import XCTest
import CRMMacCore
import CRMMacPiClient
@testable import CRMMacPhoneCallsSource

final class PhoneCallsPublisherTests: XCTestCase {
    private let auth = PiAuth(
        hostID: UUID(uuidString: "11111111-2222-3333-4444-555555555555")!,
        apiKey: "k")

    private func makeItem(zPK: Int64, zdate: Double = 1_750_000_000,
                          callUniqueID: String? = nil) -> PhoneCallPublishItem {
        let payload = CallPayload(
            hostID: auth.hostID,
            callUniqueID: callUniqueID ?? "c-\(zPK)",
            peerHandle: "+15551234567",
            peerNormalized: "+15551234567",
            service: .voice,
            direction: "inbound",
            answered: true,
            hasVoicemail: false,
            durationSeconds: 30,
            startedAt: Date(timeIntervalSince1970: 1_700_000_000))
        return PhoneCallPublishItem(
            cursorPoint: CallCursorPoint(zdate: zdate, zPK: zPK),
            direction: .received,
            payload: payload)
    }

    func testEmptyInputReturnsEmptyOutcome() async {
        let publisher = PhoneCallsPublisher(
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
        let publisher = PhoneCallsPublisher(
            sender: { _, body in
                IngestEventsData(accepted: body.events.count,
                                 duplicate: 0, rejected: 0, errors: [])
            },
            auth: auth, logger: NoopLogger())
        let items = (1...3).map { makeItem(zPK: Int64($0), zdate: 1_750_000_000 + Double($0)) }
        let outcome = await publisher.publish(items: items)
        XCTAssertEqual(outcome.accepted, 3)
        XCTAssertEqual(outcome.advanceTo?.zPK, 3)
        XCTAssertEqual(outcome.advanceTo?.zdate, 1_750_000_003)
    }

    func testAllDuplicatesAdvancesCursor() async {
        let publisher = PhoneCallsPublisher(
            sender: { _, body in
                IngestEventsData(accepted: 0,
                                 duplicate: body.events.count,
                                 rejected: 0, errors: [])
            },
            auth: auth, logger: NoopLogger())
        let items = (1...2).map { makeItem(zPK: Int64($0)) }
        let outcome = await publisher.publish(items: items)
        XCTAssertEqual(outcome.duplicate, 2)
        XCTAssertNotNil(outcome.advanceTo)
    }

    func testCursorAdvanceWithheldOnRejection() async {
        let publisher = PhoneCallsPublisher(
            sender: { _, body in
                IngestEventsData(
                    accepted: body.events.count - 1,
                    duplicate: 0,
                    rejected: 1,
                    errors: [IngestEventError(index: 1,
                                              code: "VALIDATION",
                                              message: "bad")])
            },
            auth: auth, logger: NoopLogger())
        let items = (1...3).map { makeItem(zPK: Int64($0)) }
        let outcome = await publisher.publish(items: items)
        XCTAssertEqual(outcome.rejected.count, 1)
        // Single-batch send: rejected count > 0 means
        // `advanced=false`, so advanceTo stays nil.
        XCTAssertNil(outcome.advanceTo)
    }

    func testTransportFailurePreservesCursor() async {
        struct StubError: Error {}
        let publisher = PhoneCallsPublisher(
            sender: { _, _ in
                throw StubError()
            },
            auth: auth, logger: NoopLogger())
        let items = (1...3).map { makeItem(zPK: Int64($0)) }
        let outcome = await publisher.publish(items: items)
        XCTAssertEqual(outcome.accepted, 0)
        XCTAssertNil(outcome.advanceTo)
        XCTAssertGreaterThan(outcome.unconfirmed, 0)
    }

    func testBatchSplitWhenOver200Events() async {
        // Sender records how many batches arrived.
        actor BatchCounter {
            var count = 0
            func incr() { count += 1 }
        }
        let counter = BatchCounter()
        let publisher = PhoneCallsPublisher(
            sender: { _, body in
                await counter.incr()
                return IngestEventsData(accepted: body.events.count,
                                        duplicate: 0, rejected: 0, errors: [])
            },
            auth: auth, logger: NoopLogger())
        // 250 items -> 200 + 50.
        let items = (1...250).map { makeItem(zPK: Int64($0)) }
        let outcome = await publisher.publish(items: items)
        XCTAssertEqual(outcome.accepted, 250)
        let observed = await counter.count
        XCTAssertEqual(observed, 2)
    }

    func testPerEventRejectionRecordsCallUniqueID() async {
        let publisher = PhoneCallsPublisher(
            sender: { _, _ in
                IngestEventsData(
                    accepted: 1, duplicate: 0, rejected: 1,
                    errors: [IngestEventError(index: 0,
                                              code: "PAYLOAD_INVARIANT",
                                              message: "bad")])
            },
            auth: auth, logger: NoopLogger())
        let items = [
            makeItem(zPK: 1, callUniqueID: "uniq-A"),
            makeItem(zPK: 2, callUniqueID: "uniq-B"),
        ]
        let outcome = await publisher.publish(items: items)
        XCTAssertEqual(outcome.rejected.count, 1)
        XCTAssertEqual(outcome.rejected[0].callUniqueID, "uniq-A")
        XCTAssertEqual(outcome.rejected[0].zPK, 1)
        XCTAssertEqual(outcome.rejected[0].code, "PAYLOAD_INVARIANT")
    }
}
