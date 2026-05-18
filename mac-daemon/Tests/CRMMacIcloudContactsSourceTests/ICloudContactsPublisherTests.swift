// Tests for ICloudContactsPublisher's batching + outcome semantics.
//
// The publisher splits items into ≤100-event batches, posts each via
// the injected sender, aggregates accepted/duplicate/rejected counts
// across batches, and signals to the plugin (via the outcome shape)
// whether it's safe to advance the cursor + commit the staged cache
// removals.
import XCTest
import CRMMacCore
import CRMMacPiClient
@testable import CRMMacIcloudContactsSource

final class ICloudContactsPublisherTests: XCTestCase {
    private let auth = PiAuth(
        hostID: UUID(uuidString: "11111111-2222-3333-4444-555555555555")!,
        apiKey: "k")

    private func makeUpsertItem(index: Int) -> ICloudContactsPublishItem {
        // Minimal valid JSON payload so the IngestEvent's RawJSON
        // encoder doesn't trip the parse guard.
        let payload = #"{"version":1,"entity_id":"id-\#(index)"}"#
        return ICloudContactsPublishItem(
            sourceID: "id-\(index)@deadbeef",
            kind: "external_contact.upserted",
            payloadBytes: Data(payload.utf8))
    }

    private func makeDeleteItem(index: Int) -> ICloudContactsPublishItem {
        let payload = #"{"version":1,"entity_id":"id-\#(index)"}"#
        return ICloudContactsPublishItem(
            sourceID: "id-\(index)@deleted@cafebabe",
            kind: "external_contact.deleted",
            payloadBytes: Data(payload.utf8))
    }

    // MARK: - empty input

    func testEmptyInputDoesNotCallSender() async {
        let publisher = ICloudContactsPublisher(
            sender: { _, _ in
                XCTFail("sender should not be called for empty input")
                return IngestEventsData(
                    accepted: 0, duplicate: 0, rejected: 0, errors: [])
            },
            auth: auth, logger: NoopLogger())
        let outcome = await publisher.publish(items: [])
        XCTAssertEqual(outcome.accepted, 0)
        XCTAssertEqual(outcome.duplicate, 0)
        XCTAssertTrue(outcome.rejected.isEmpty)
        XCTAssertEqual(outcome.unconfirmed, 0)
        XCTAssertFalse(outcome.anyBatchSucceeded)
    }

    // MARK: - happy paths

    func testSingleBatchAllAccepted() async {
        let publisher = ICloudContactsPublisher(
            sender: { _, body in
                IngestEventsData(
                    accepted: body.events.count,
                    duplicate: 0, rejected: 0, errors: [])
            },
            auth: auth, logger: NoopLogger())
        let items = (1...5).map(makeUpsertItem)
        let outcome = await publisher.publish(items: items)
        XCTAssertEqual(outcome.accepted, 5)
        XCTAssertEqual(outcome.duplicate, 0)
        XCTAssertTrue(outcome.rejected.isEmpty)
        XCTAssertEqual(outcome.unconfirmed, 0)
        XCTAssertTrue(outcome.anyBatchSucceeded)
    }

    func testAllDuplicatesAggregates() async {
        let publisher = ICloudContactsPublisher(
            sender: { _, body in
                IngestEventsData(
                    accepted: 0,
                    duplicate: body.events.count,
                    rejected: 0, errors: [])
            },
            auth: auth, logger: NoopLogger())
        let items = (1...3).map(makeUpsertItem)
        let outcome = await publisher.publish(items: items)
        XCTAssertEqual(outcome.duplicate, 3)
        XCTAssertEqual(outcome.unconfirmed, 0)
        XCTAssertTrue(outcome.rejected.isEmpty)
    }

    // MARK: - batching

    func testBatchSplitAtMaxEventsPerBatch() async {
        // 250 items @ 100/batch → 3 batches (100 + 100 + 50)
        actor BatchCounter {
            var batchSizes: [Int] = []
            func record(_ n: Int) { batchSizes.append(n) }
            func snapshot() -> [Int] { batchSizes }
        }
        let counter = BatchCounter()
        let publisher = ICloudContactsPublisher(
            sender: { _, body in
                await counter.record(body.events.count)
                return IngestEventsData(
                    accepted: body.events.count,
                    duplicate: 0, rejected: 0, errors: [])
            },
            auth: auth, logger: NoopLogger())
        let items = (1...250).map(makeUpsertItem)
        let outcome = await publisher.publish(items: items)
        XCTAssertEqual(outcome.accepted, 250)
        XCTAssertEqual(outcome.unconfirmed, 0)
        let sizes = await counter.snapshot()
        XCTAssertEqual(sizes, [100, 100, 50])
    }

    func testBatchHonorsMaxEventsForExactly100() async {
        actor BatchCounter {
            var count: Int = 0
            func bump() { count += 1 }
            func snapshot() -> Int { count }
        }
        let counter = BatchCounter()
        let publisher = ICloudContactsPublisher(
            sender: { _, _ in
                await counter.bump()
                return IngestEventsData(
                    accepted: 0, duplicate: 0, rejected: 0, errors: [])
            },
            auth: auth, logger: NoopLogger())
        let items = (1...100).map(makeUpsertItem)
        _ = await publisher.publish(items: items)
        let n = await counter.snapshot()
        XCTAssertEqual(n, 1, "exactly 100 items should fit in one batch")
    }

    // MARK: - per-event rejections

    func testPerEventRejectionReportedWithKindAndSourceID() async {
        let publisher = ICloudContactsPublisher(
            sender: { _, body in
                // Reject the second event with a hash-mismatch code.
                IngestEventsData(
                    accepted: body.events.count - 1,
                    duplicate: 0, rejected: 1,
                    errors: [IngestEventError(
                        index: 1, code: "EXTERNAL_CONTACT_HASH_MISMATCH",
                        message: "hash mismatch on payload")])
            },
            auth: auth, logger: NoopLogger())
        let items = [
            makeUpsertItem(index: 1),
            makeUpsertItem(index: 2),
            makeUpsertItem(index: 3),
        ]
        let outcome = await publisher.publish(items: items)
        XCTAssertEqual(outcome.accepted, 2)
        XCTAssertEqual(outcome.rejected.count, 1)
        XCTAssertEqual(outcome.rejected.first?.code,
                       "EXTERNAL_CONTACT_HASH_MISMATCH")
        XCTAssertEqual(outcome.rejected.first?.sourceID, "id-2@deadbeef")
        XCTAssertEqual(outcome.rejected.first?.kind,
                       "external_contact.upserted")
        XCTAssertEqual(outcome.unconfirmed, 0,
                       "all events were submitted; rejection ≠ unconfirmed")
    }

    func testDeleteHashMismatchSurfacedToCaller() async {
        let publisher = ICloudContactsPublisher(
            sender: { _, _ in
                IngestEventsData(
                    accepted: 0, duplicate: 0, rejected: 1,
                    errors: [IngestEventError(
                        index: 0,
                        code: "EXTERNAL_CONTACT_DELETE_HASH_MISMATCH",
                        message: "delete prior hash mismatch")])
            },
            auth: auth, logger: NoopLogger())
        let outcome = await publisher.publish(items: [makeDeleteItem(index: 1)])
        XCTAssertEqual(outcome.rejected.count, 1)
        XCTAssertEqual(outcome.rejected.first?.code,
                       "EXTERNAL_CONTACT_DELETE_HASH_MISMATCH")
        XCTAssertEqual(outcome.rejected.first?.kind,
                       "external_contact.deleted")
    }

    func testRejectionIndexOutOfRangeIsSurfacedAsUnattributedRejection() async {
        // Pi returning an out-of-range index is anomalous; we
        // surface the rejection as an unattributed entry so the
        // caller's commit gate (`rejected.isEmpty`) holds the
        // cursor rather than silently advancing.
        let publisher = ICloudContactsPublisher(
            sender: { _, body in
                IngestEventsData(
                    accepted: body.events.count,
                    duplicate: 0, rejected: 0,
                    errors: [IngestEventError(
                        index: 99, code: "VALIDATION", message: "bogus")])
            },
            auth: auth, logger: NoopLogger())
        let items = (1...2).map(makeUpsertItem)
        let outcome = await publisher.publish(items: items)
        XCTAssertEqual(outcome.accepted, 2)
        XCTAssertEqual(outcome.rejected.count, 1,
                       "out-of-range rejection index must still surface so cursor advance is blocked")
        XCTAssertEqual(outcome.rejected.first?.sourceID, "<unattributed>")
    }

    func testPiReportedRejectedCountExceedingErrorsListSynthesizesPlaceholders() async {
        // The Pi's response carries both `rejected` (count) and
        // `errors` (array). If they diverge — `rejected > errors.count`
        // — the publisher must synthesize placeholder rejections so
        // the caller's commit gate (`rejected.isEmpty`) holds the
        // cursor rather than silently advancing.
        let publisher = ICloudContactsPublisher(
            sender: { _, _ in
                IngestEventsData(
                    accepted: 0, duplicate: 0, rejected: 3,
                    errors: [])
            },
            auth: auth, logger: NoopLogger())
        let outcome = await publisher.publish(items: [makeUpsertItem(index: 1)])
        XCTAssertEqual(outcome.rejected.count, 3,
                       "missing error details get synthesized as unattributed entries")
        XCTAssertTrue(outcome.rejected.allSatisfy {
            $0.code == "UNATTRIBUTED_REJECTION"
        })
    }

    func testHashMismatchAbortsRemainingBatches() async {
        // The publisher must abort subsequent batches after the
        // first hash-mismatch rejection so the recovery flow can
        // run on the next tick without compounding divergence.
        actor BatchTracker {
            var batches: Int = 0
            func bump() -> Int { batches += 1; return batches }
        }
        let tracker = BatchTracker()
        let publisher = ICloudContactsPublisher(
            sender: { _, body in
                let n = await tracker.bump()
                if n == 1 {
                    return IngestEventsData(
                        accepted: body.events.count - 1,
                        duplicate: 0, rejected: 1,
                        errors: [IngestEventError(
                            index: 0,
                            code: "EXTERNAL_CONTACT_HASH_MISMATCH",
                            message: "diverge")])
                }
                return IngestEventsData(
                    accepted: body.events.count,
                    duplicate: 0, rejected: 0, errors: [])
            },
            auth: auth, logger: NoopLogger())
        let items = (1...250).map(makeUpsertItem)
        let outcome = await publisher.publish(items: items)
        let batchCount = await tracker.batches
        XCTAssertEqual(batchCount, 1,
                       "subsequent batches must not be attempted after hash mismatch")
        XCTAssertEqual(outcome.unconfirmed, 150,
                       "batches 2 + 3 (100 + 50) are reported as unconfirmed so cursor holds")
        XCTAssertEqual(outcome.rejected.count, 1)
        XCTAssertEqual(outcome.rejected.first?.code,
                       "EXTERNAL_CONTACT_HASH_MISMATCH")
    }

    // MARK: - transport failure mid-stream

    func testTransportFailureMidStreamMarksRemainderUnconfirmed() async {
        // 250 items split into 3 batches; batch 2 throws → remaining
        // batches NOT attempted. unconfirmed counts both batch 2 and
        // batch 3 items (= 150).
        actor Counter {
            var batchesSeen: Int = 0
            func bump() -> Int { batchesSeen += 1; return batchesSeen }
        }
        let c = Counter()
        let publisher = ICloudContactsPublisher(
            sender: { _, body in
                let n = await c.bump()
                if n == 2 {
                    throw URLError(.networkConnectionLost)
                }
                return IngestEventsData(
                    accepted: body.events.count,
                    duplicate: 0, rejected: 0, errors: [])
            },
            auth: auth, logger: NoopLogger())
        let items = (1...250).map(makeUpsertItem)
        let outcome = await publisher.publish(items: items)
        XCTAssertEqual(outcome.accepted, 100,
                       "only batch 1 succeeded; batch 2 threw, batch 3 not attempted")
        XCTAssertEqual(outcome.unconfirmed, 150,
                       "batch 2 (100) + batch 3 (50) never reached Pi")
        XCTAssertTrue(outcome.anyBatchSucceeded,
                      "batch 1 succeeded — caller can bump lastPushedAt")
    }

    func testTransportFailureOnFirstBatchLeavesEverythingUnconfirmed() async {
        let publisher = ICloudContactsPublisher(
            sender: { _, _ in throw URLError(.cannotConnectToHost) },
            auth: auth, logger: NoopLogger())
        let items = (1...50).map(makeUpsertItem)
        let outcome = await publisher.publish(items: items)
        XCTAssertEqual(outcome.accepted, 0)
        XCTAssertEqual(outcome.unconfirmed, 50)
        XCTAssertFalse(outcome.anyBatchSucceeded)
    }

    // MARK: - event envelope contents

    func testEnvelopeCarriesSourceAndKind() async {
        actor Capture {
            var bodies: [IngestEventsBody] = []
            func record(_ b: IngestEventsBody) { bodies.append(b) }
            func snapshot() -> [IngestEventsBody] { bodies }
        }
        let cap = Capture()
        let publisher = ICloudContactsPublisher(
            sender: { _, body in
                await cap.record(body)
                return IngestEventsData(
                    accepted: body.events.count,
                    duplicate: 0, rejected: 0, errors: [])
            },
            auth: auth, logger: NoopLogger())
        let items = [
            makeUpsertItem(index: 1),
            makeDeleteItem(index: 2),
        ]
        _ = await publisher.publish(items: items)
        let bodies = await cap.snapshot()
        XCTAssertEqual(bodies.count, 1)
        XCTAssertEqual(bodies.first?.events.count, 2)
        XCTAssertEqual(bodies.first?.events[0].source, "icloud_contacts")
        XCTAssertEqual(bodies.first?.events[0].kind,
                       "external_contact.upserted")
        XCTAssertEqual(bodies.first?.events[0].sourceID, "id-1@deadbeef")
        XCTAssertEqual(bodies.first?.events[1].kind,
                       "external_contact.deleted")
        XCTAssertEqual(bodies.first?.events[1].sourceID,
                       "id-2@deleted@cafebabe")
    }
}
