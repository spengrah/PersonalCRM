// Coverage for AnarlogSessionsPublisher. Distinct from the humans
// publisher tests because the recovery code set differs and the
// source string is different — both surfaces should be pinned.
import XCTest
import CRMMacCore
import CRMMacPiClient
@testable import CRMMacAnarlogSource

final class AnarlogSessionsPublisherTests: XCTestCase {
    private let auth = PiAuth(
        hostID: UUID(uuidString: "11111111-2222-3333-4444-555555555555")!,
        apiKey: "k")

    private func makeRecordedItem(index: Int) -> AnarlogSessionsPublishItem {
        let payload = #"{"version":1,"source_id":"id-\#(index)"}"#
        return AnarlogSessionsPublishItem(
            sourceID: "id-\(index)@deadbeef",
            kind: "meeting_note.recorded",
            payloadBytes: Data(payload.utf8))
    }

    private func makeDeletedItem(index: Int) -> AnarlogSessionsPublishItem {
        let payload = #"{"version":1,"source_id":"id-\#(index)"}"#
        return AnarlogSessionsPublishItem(
            sourceID: "id-\(index)@deleted@cafebabe",
            kind: "meeting_note.deleted",
            payloadBytes: Data(payload.utf8))
    }

    func testEmptyInputDoesNotCallSender() async {
        let publisher = AnarlogSessionsPublisher(
            sender: { _, _ in
                XCTFail("sender should not be called for empty input")
                return IngestEventsData(accepted: 0, duplicate: 0, rejected: 0, errors: [])
            },
            auth: auth, logger: NoopLogger())
        let outcome = await publisher.publish(items: [])
        XCTAssertFalse(outcome.anyBatchSucceeded)
    }

    func testSingleBatchAllAccepted() async {
        let publisher = AnarlogSessionsPublisher(
            sender: { _, body in
                IngestEventsData(
                    accepted: body.events.count,
                    duplicate: 0, rejected: 0, errors: [])
            },
            auth: auth, logger: NoopLogger())
        let outcome = await publisher.publish(items: (1...5).map(makeRecordedItem))
        XCTAssertEqual(outcome.accepted, 5)
        XCTAssertTrue(outcome.rejected.isEmpty)
    }

    func testUnknownKindRejectionHoldsCursor() async {
        // Pi currently rejects meeting_note.* with UNKNOWN_KIND
        // because the kinds aren't registered yet. UNKNOWN_KIND is
        // NOT in the recovery code set, so subsequent batches DO
        // continue; the cursor still holds via the rejected.isEmpty
        // gate.
        let publisher = AnarlogSessionsPublisher(
            sender: { _, body in
                IngestEventsData(
                    accepted: 0, duplicate: 0,
                    rejected: body.events.count,
                    errors: body.events.indices.map { idx in
                        IngestEventError(
                            index: idx, code: "UNKNOWN_KIND",
                            message: "unknown kind: meeting_note.recorded")
                    })
            },
            auth: auth, logger: NoopLogger())
        let outcome = await publisher.publish(items: (1...3).map(makeRecordedItem))
        XCTAssertEqual(outcome.accepted, 0)
        XCTAssertEqual(outcome.rejected.count, 3)
        XCTAssertEqual(outcome.unconfirmed, 0)
        XCTAssertTrue(outcome.anyBatchSucceeded)
        XCTAssertTrue(outcome.rejected.allSatisfy { $0.code == "UNKNOWN_KIND" })
    }

    func testMeetingNoteHashMismatchAbortsSubsequentBatches() async {
        actor BatchTracker {
            var seen: Int = 0
            func bump() { seen += 1 }
            func count() -> Int { seen }
        }
        let tracker = BatchTracker()
        let publisher = AnarlogSessionsPublisher(
            sender: { _, body in
                await tracker.bump()
                return IngestEventsData(
                    accepted: 0, duplicate: 0, rejected: 1,
                    errors: [IngestEventError(
                        index: 0, code: "MEETING_NOTE_HASH_MISMATCH",
                        message: "mismatch")])
            },
            auth: auth, logger: NoopLogger())
        let outcome = await publisher.publish(items: (1...250).map(makeRecordedItem))
        let batchesSent = await tracker.count()
        XCTAssertEqual(batchesSent, 1)
        XCTAssertEqual(outcome.unconfirmed, 150)
    }

    func testRecoveryCodesSetIsPinned() {
        XCTAssertEqual(AnarlogSessionsPublisher.recoveryCodes, [
            "MEETING_NOTE_HASH_MISMATCH",
            "MEETING_NOTE_DELETE_HASH_MISMATCH",
        ])
    }

    func testIngestEventCarriesAnarlogSessionsSource() async {
        actor SourceCapture {
            var source: String?
            func set(_ s: String) { source = s }
            func get() -> String? { source }
        }
        let capture = SourceCapture()
        let publisher = AnarlogSessionsPublisher(
            sender: { _, body in
                if let first = body.events.first {
                    await capture.set(first.source)
                }
                return IngestEventsData(
                    accepted: body.events.count,
                    duplicate: 0, rejected: 0, errors: [])
            },
            auth: auth, logger: NoopLogger())
        _ = await publisher.publish(items: [makeRecordedItem(index: 1)])
        let observed = await capture.get()
        XCTAssertEqual(observed, "anarlog_sessions")
    }

    func testRecordedAndDeletedCanMix() async {
        let publisher = AnarlogSessionsPublisher(
            sender: { _, body in
                IngestEventsData(
                    accepted: body.events.count,
                    duplicate: 0, rejected: 0, errors: [])
            },
            auth: auth, logger: NoopLogger())
        let items: [AnarlogSessionsPublishItem] = [
            makeRecordedItem(index: 1),
            makeDeletedItem(index: 2),
        ]
        let outcome = await publisher.publish(items: items)
        XCTAssertEqual(outcome.accepted, 2)
    }

    func testNeedsAttentionAggregatedAcrossBatches() async {
        // 250 items → ≥ 2 batches at maxEventsPerBatch=100. The
        // publisher must concatenate needs_attention across all
        // batches; the plugin downstream forwards them to the
        // notification center.
        let session1 = "deadbeef-1111-2222-3333-444455556661"
        let session2 = "deadbeef-1111-2222-3333-444455556662"
        let session3 = "deadbeef-1111-2222-3333-444455556663"
        actor BatchCounter {
            var i: Int = 0
            func next() -> Int { defer { i += 1 }; return i }
        }
        let counter = BatchCounter()
        let publisher = AnarlogSessionsPublisher(
            sender: { _, body in
                let idx = await counter.next()
                // Batch 0 surfaces session1+session2; batch 1
                // surfaces session3. The publisher's outcome
                // should carry all three.
                let na: [NeedsAttentionItem]
                if idx == 0 {
                    na = [
                        NeedsAttentionItem(sessionID: session1, reason: "orphan"),
                        NeedsAttentionItem(sessionID: session2, reason: "conflict"),
                    ]
                } else if idx == 1 {
                    na = [NeedsAttentionItem(sessionID: session3, reason: "orphan")]
                } else {
                    na = []
                }
                return IngestEventsData(
                    accepted: body.events.count,
                    duplicate: 0, rejected: 0, errors: [],
                    needsAttention: na)
            },
            auth: auth, logger: NoopLogger())
        let outcome = await publisher.publish(items: (1...250).map(makeRecordedItem))
        XCTAssertEqual(outcome.needsAttention.count, 3)
        XCTAssertTrue(outcome.needsAttention.contains {
            $0.sessionID == session1 && $0.reason == "orphan"
        })
        XCTAssertTrue(outcome.needsAttention.contains {
            $0.sessionID == session2 && $0.reason == "conflict"
        })
        XCTAssertTrue(outcome.needsAttention.contains {
            $0.sessionID == session3 && $0.reason == "orphan"
        })
    }

    func testNeedsAttentionEmptyByDefaultWhenSenderOmits() async {
        let publisher = AnarlogSessionsPublisher(
            sender: { _, body in
                IngestEventsData(
                    accepted: body.events.count,
                    duplicate: 0, rejected: 0, errors: [])
            },
            auth: auth, logger: NoopLogger())
        let outcome = await publisher.publish(items: [makeRecordedItem(index: 1)])
        XCTAssertTrue(outcome.needsAttention.isEmpty)
    }
}
