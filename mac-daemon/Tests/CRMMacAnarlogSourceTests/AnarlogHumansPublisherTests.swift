// Coverage for AnarlogHumansPublisher's batching + outcome semantics.
// Mirrors ICloudContactsPublisherTests; the two publishers share the
// same internal shape so this is the regression guard for the
// anarlog-specific instance (recovery code set, source string).
import XCTest
import CRMMacCore
import CRMMacPiClient
@testable import CRMMacAnarlogSource

final class AnarlogHumansPublisherTests: XCTestCase {
    private let auth = PiAuth(
        hostID: UUID(uuidString: "11111111-2222-3333-4444-555555555555")!,
        apiKey: "k")

    private func makeUpsertItem(index: Int) -> AnarlogHumansPublishItem {
        let payload = #"{"version":1,"entity_id":"id-\#(index)"}"#
        return AnarlogHumansPublishItem(
            sourceID: "id-\(index)@deadbeef",
            kind: "external_contact.upserted",
            payloadBytes: Data(payload.utf8))
    }

    private func makeDeleteItem(index: Int) -> AnarlogHumansPublishItem {
        let payload = #"{"version":1,"entity_id":"id-\#(index)"}"#
        return AnarlogHumansPublishItem(
            sourceID: "id-\(index)@deleted@cafebabe",
            kind: "external_contact.deleted",
            payloadBytes: Data(payload.utf8))
    }

    func testEmptyInputDoesNotCallSender() async {
        let publisher = AnarlogHumansPublisher(
            sender: { _, _ in
                XCTFail("sender should not be called for empty input")
                return IngestEventsData(accepted: 0, duplicate: 0, rejected: 0, errors: [])
            },
            auth: auth, logger: NoopLogger())
        let outcome = await publisher.publish(items: [])
        XCTAssertEqual(outcome.accepted, 0)
        XCTAssertEqual(outcome.duplicate, 0)
        XCTAssertTrue(outcome.rejected.isEmpty)
        XCTAssertEqual(outcome.unconfirmed, 0)
        XCTAssertFalse(outcome.anyBatchSucceeded)
    }

    func testSingleBatchAllAccepted() async {
        let publisher = AnarlogHumansPublisher(
            sender: { _, body in
                IngestEventsData(
                    accepted: body.events.count,
                    duplicate: 0, rejected: 0, errors: [])
            },
            auth: auth, logger: NoopLogger())
        let outcome = await publisher.publish(items: (1...5).map(makeUpsertItem))
        XCTAssertEqual(outcome.accepted, 5)
        XCTAssertTrue(outcome.rejected.isEmpty)
        XCTAssertEqual(outcome.unconfirmed, 0)
        XCTAssertTrue(outcome.anyBatchSucceeded)
    }

    func testRejectionsHoldCursor() async {
        // Simulate 3 events; one rejected with PAYLOAD_INVARIANT (PR 2
        // expectation pre PR 3 acceptance).
        let publisher = AnarlogHumansPublisher(
            sender: { _, body in
                IngestEventsData(
                    accepted: body.events.count - 1,
                    duplicate: 0, rejected: 1,
                    errors: [IngestEventError(
                        index: 1, code: "PAYLOAD_INVARIANT",
                        message: "unknown source")])
            },
            auth: auth, logger: NoopLogger())
        let outcome = await publisher.publish(items: (1...3).map(makeUpsertItem))
        XCTAssertEqual(outcome.accepted, 2)
        XCTAssertEqual(outcome.rejected.count, 1)
        XCTAssertEqual(outcome.rejected[0].code, "PAYLOAD_INVARIANT")
        XCTAssertEqual(outcome.unconfirmed, 0)
        XCTAssertTrue(outcome.anyBatchSucceeded)
    }

    func testRecoveryCodeAbortsSubsequentBatches() async {
        // 250 items @ 100/batch = 3 batches. First batch returns
        // EXTERNAL_CONTACT_HASH_MISMATCH on index 5; expect 200
        // unconfirmed (batches 2 + 3 never sent).
        actor BatchTracker {
            var seen: Int = 0
            func bump() { seen += 1 }
            func count() -> Int { seen }
        }
        let tracker = BatchTracker()
        let publisher = AnarlogHumansPublisher(
            sender: { _, body in
                await tracker.bump()
                return IngestEventsData(
                    accepted: 0, duplicate: 0, rejected: 1,
                    errors: [IngestEventError(
                        index: 5, code: "EXTERNAL_CONTACT_HASH_MISMATCH",
                        message: "mismatch")])
            },
            auth: auth, logger: NoopLogger())
        let outcome = await publisher.publish(items: (1...250).map(makeUpsertItem))
        let batchesSent = await tracker.count()
        XCTAssertEqual(batchesSent, 1)
        XCTAssertEqual(outcome.unconfirmed, 150)
        XCTAssertEqual(outcome.rejected.count, 1)
    }

    func testRecoveryCodesSetIsPinned() {
        XCTAssertEqual(AnarlogHumansPublisher.recoveryCodes, [
            "EXTERNAL_CONTACT_HASH_MISMATCH",
            "EXTERNAL_CONTACT_DELETE_HASH_MISMATCH",
        ])
    }

    func testSenderThrowMarksRemainingUnconfirmed() async {
        struct Boom: Error {}
        let publisher = AnarlogHumansPublisher(
            sender: { _, _ in throw Boom() },
            auth: auth, logger: NoopLogger())
        let outcome = await publisher.publish(items: (1...3).map(makeUpsertItem))
        XCTAssertEqual(outcome.accepted, 0)
        XCTAssertEqual(outcome.unconfirmed, 3)
        XCTAssertFalse(outcome.anyBatchSucceeded)
    }

    func testIngestEventCarriesAnarlogHumansSource() async {
        actor SourceCapture {
            var source: String?
            func set(_ s: String) { source = s }
            func get() -> String? { source }
        }
        let capture = SourceCapture()
        let publisher = AnarlogHumansPublisher(
            sender: { _, body in
                if let first = body.events.first {
                    await capture.set(first.source)
                }
                return IngestEventsData(
                    accepted: body.events.count,
                    duplicate: 0, rejected: 0, errors: [])
            },
            auth: auth, logger: NoopLogger())
        _ = await publisher.publish(items: [makeUpsertItem(index: 1)])
        let observed = await capture.get()
        XCTAssertEqual(observed, "anarlog_humans")
    }

    func testDeleteAndUpsertCanMix() async {
        let publisher = AnarlogHumansPublisher(
            sender: { _, body in
                IngestEventsData(
                    accepted: body.events.count,
                    duplicate: 0, rejected: 0, errors: [])
            },
            auth: auth, logger: NoopLogger())
        let items: [AnarlogHumansPublishItem] = [
            makeUpsertItem(index: 1),
            makeDeleteItem(index: 2),
            makeUpsertItem(index: 3),
        ]
        let outcome = await publisher.publish(items: items)
        XCTAssertEqual(outcome.accepted, 3)
    }
}
