// ICloudContactsPublisher — batches `external_contact.*` IngestEvents
// and posts them to /api/v1/ingest/events. Mirrors MessagesPublisher
// minus the chat.db ROWID concept (icloud_contacts doesn't have a
// monotonically-increasing per-event watermark; the cursor is opaque
// CNChangeHistoryFetchRequest token bytes).
//
// Batching: cap at 100 events per batch (plan §11g locked decision;
// well under the Pi's hard 500-event limit). Body-size cap inherits
// the messages source's 1 MiB threshold for parity.
//
// Outcome semantics: the plugin uses this to decide whether to
// (a) advance the cursor, (b) commit pendingRemovals on the hash
// cache, and (c) set the recovery flag on hash-mismatch rejection.
import Foundation
import CRMMacCore
import CRMMacPiClient

/// One pending external_contact event ready to publish.
public struct ICloudContactsPublishItem: Sendable {
    /// `entity@hash` for upserts; `entity@deleted@hash` for deletes.
    public let sourceID: String
    /// Either `external_contact.upserted` or `external_contact.deleted`.
    public let kind: String
    /// The serialized payload bytes (raw JSON object).
    public let payloadBytes: Data

    public init(sourceID: String, kind: String, payloadBytes: Data) {
        self.sourceID = sourceID
        self.kind = kind
        self.payloadBytes = payloadBytes
    }
}

public struct ICloudContactsBatchOutcome: Equatable, Sendable {
    public let accepted: Int
    public let duplicate: Int
    public let rejected: [ICloudContactsRejection]
    /// Items submitted but neither accepted/duplicated nor rejected.
    /// Treat any nonzero value as "do not advance the cursor" —
    /// those rows didn't reach the Pi.
    public let unconfirmed: Int
    /// True if at least one batch succeeded — the plugin uses this
    /// to decide whether to bump SourceState.lastPushedAt.
    public let anyBatchSucceeded: Bool

    public init(
        accepted: Int,
        duplicate: Int,
        rejected: [ICloudContactsRejection],
        unconfirmed: Int,
        anyBatchSucceeded: Bool
    ) {
        self.accepted = accepted
        self.duplicate = duplicate
        self.rejected = rejected
        self.unconfirmed = unconfirmed
        self.anyBatchSucceeded = anyBatchSucceeded
    }
}

public struct ICloudContactsRejection: Equatable, Sendable {
    public let sourceID: String
    public let kind: String
    public let code: String
    public let message: String

    public init(sourceID: String, kind: String, code: String, message: String) {
        self.sourceID = sourceID
        self.kind = kind
        self.code = code
        self.message = message
    }
}

public typealias ICloudContactsIngestSender = @Sendable (PiAuth, IngestEventsBody) async throws -> IngestEventsData

public actor ICloudContactsPublisher {
    /// Cap on events per batch. Plan §11g locked decision: 100. Well
    /// under the Pi's hard 500-event limit so a malformed
    /// content_hash on one event doesn't poison an entire 500-batch.
    public static let maxEventsPerBatch: Int = 100
    /// Cap on body bytes per batch. Inherited from MessagesPublisher
    /// for consistency: 1 MiB.
    public static let maxBodyBytes: Int = 1 * 1024 * 1024

    private let sender: ICloudContactsIngestSender
    private let auth: PiAuth
    private let logger: LoggerProtocol
    private let clock: @Sendable () -> Date

    public init(
        sender: @escaping ICloudContactsIngestSender,
        auth: PiAuth,
        logger: LoggerProtocol,
        clock: @escaping @Sendable () -> Date = { Date() }
    ) {
        self.sender = sender
        self.auth = auth
        self.logger = logger
        self.clock = clock
    }

    /// Publish `items` in one or more batches sized per
    /// maxEventsPerBatch and maxBodyBytes. The caller is responsible
    /// for the cursor + cache commit decision based on the returned
    /// outcome — only advance when `rejected.isEmpty && unconfirmed
    /// == 0`.
    public func publish(items: [ICloudContactsPublishItem]) async -> ICloudContactsBatchOutcome {
        if items.isEmpty {
            return ICloudContactsBatchOutcome(
                accepted: 0, duplicate: 0, rejected: [],
                unconfirmed: 0, anyBatchSucceeded: false)
        }

        var totalAccepted = 0
        var totalDuplicate = 0
        var rejections: [ICloudContactsRejection] = []
        var unconfirmed = 0
        var anyBatchSucceeded = false

        // Split into batches up-front so the pre-emptive size check
        // doesn't have to recompute encoding twice.
        let batches = splitIntoBatches(items)
        var remainingItemsAfterBatch = items.count
        for (i, batch) in batches.enumerated() {
            let body = IngestEventsBody(events: batch.map { makeIngestEvent(from: $0) })
            do {
                let response = try await sender(auth, body)
                totalAccepted += response.accepted
                totalDuplicate += response.duplicate
                for err in response.errors {
                    guard err.index >= 0, err.index < batch.count else {
                        logger.warning("icloud publish: out-of-range rejection index", metadata: [
                            "index": .public(String(err.index)),
                            "batch_size": .public(String(batch.count)),
                        ])
                        continue
                    }
                    let item = batch[err.index]
                    rejections.append(ICloudContactsRejection(
                        sourceID: item.sourceID, kind: item.kind,
                        code: err.code, message: err.message))
                    logger.warning("icloud publish: per-event rejection", metadata: [
                        "source_id": .private(item.sourceID),
                        "kind": .public(item.kind),
                        "code": .public(err.code),
                        "message": .public(err.message),
                    ])
                }
                anyBatchSucceeded = true
                remainingItemsAfterBatch -= batch.count
            } catch {
                logger.warning("icloud publish: batch failed", metadata: [
                    "batch_index": .public(String(i)),
                    "batch_size": .public(String(batch.count)),
                    "error": .private(String(describing: error)),
                ])
                // Transport failure on this batch leaves all items
                // in this batch + everything after it unsubmitted.
                unconfirmed += remainingItemsAfterBatch
                break
            }
        }

        return ICloudContactsBatchOutcome(
            accepted: totalAccepted,
            duplicate: totalDuplicate,
            rejected: rejections,
            unconfirmed: unconfirmed,
            anyBatchSucceeded: anyBatchSucceeded)
    }

    // MARK: - private

    /// Split items into batches sized per maxEventsPerBatch and
    /// maxBodyBytes. Pre-emptive size check uses the encoded
    /// envelope length + a small per-event overhead for JSON
    /// commas/braces.
    private func splitIntoBatches(
        _ items: [ICloudContactsPublishItem]
    ) -> [[ICloudContactsPublishItem]] {
        var batches: [[ICloudContactsPublishItem]] = []
        var current: [ICloudContactsPublishItem] = []
        var currentBytes = 0
        let perEventOverhead = 16
        for item in items {
            // Approximate the encoded event envelope size as the
            // payload length + a small constant for the envelope
            // keys (source, source_id, kind, observed_at). Good
            // enough to keep the body under 1 MiB without forcing
            // a full JSONEncoder.encode per item.
            let approxEventBytes = item.payloadBytes.count + 256
            if !current.isEmpty,
               current.count >= Self.maxEventsPerBatch ||
                 currentBytes + approxEventBytes + perEventOverhead > Self.maxBodyBytes {
                batches.append(current)
                current = []
                currentBytes = 0
            }
            current.append(item)
            currentBytes += approxEventBytes + perEventOverhead
        }
        if !current.isEmpty {
            batches.append(current)
        }
        return batches
    }

    private func makeIngestEvent(from item: ICloudContactsPublishItem) -> IngestEvent {
        IngestEvent(
            source: "icloud_contacts",
            sourceID: item.sourceID,
            kind: item.kind,
            payload: RawJSON(item.payloadBytes),
            observedAt: clock())
    }
}
