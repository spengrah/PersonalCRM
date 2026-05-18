// ICloudContactsPublisher — batches `external_contact.*` IngestEvents
// and posts them to /api/v1/ingest/events. Mirrors MessagesPublisher
// minus the chat.db ROWID concept (icloud_contacts doesn't have a
// monotonically-increasing per-event watermark; the cursor is opaque
// CNChangeHistoryFetchRequest token bytes).
//
// Batching: cap at 100 events per batch (well under the Pi's hard
// 500-event limit so a single malformed content_hash on one event
// doesn't poison an entire 500-batch). Body-size cap inherits the
// messages source's 1 MiB threshold for parity.
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

    /// Codes that signal the daemon must abort further batches in
    /// this tick AND request a recovery cycle on the next tick. The
    /// plan's locked semantics: a single hash-mismatch indicates the
    /// daemon's cache vs Pi's stored hash have diverged; sending more
    /// events from the same tick risks compounding the divergence.
    public static let recoveryCodes: Set<String> = [
        "EXTERNAL_CONTACT_HASH_MISMATCH",
        "EXTERNAL_CONTACT_DELETE_HASH_MISMATCH",
    ]

    /// Publish `items` in one or more batches sized per
    /// maxEventsPerBatch and maxBodyBytes. The caller advances the
    /// cursor + commits cache removals ONLY when
    /// `rejected.isEmpty && unconfirmed == 0`. Any per-event
    /// rejection whose code is in `recoveryCodes` aborts subsequent
    /// batches; the remaining items are reported as `unconfirmed`.
    /// Pi-reported `rejected` count vs `errors` count mismatch is
    /// surfaced as an `unaccountedRejections` term that the caller
    /// treats the same as `rejected.count > 0` (cursor held).
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
        var sawRecoveryCode = false

        let batches = splitIntoBatches(items)
        // Track items in batches not yet attempted so a mid-stream
        // failure can attribute the right count to `unconfirmed`.
        var remainingItemsAfterBatch = items.count
        for (i, batch) in batches.enumerated() {
            let body = IngestEventsBody(events: batch.map { makeIngestEvent(from: $0) })
            do {
                let response = try await sender(auth, body)
                totalAccepted += response.accepted
                totalDuplicate += response.duplicate
                var batchRecoveryHit = false
                for err in response.errors {
                    guard err.index >= 0, err.index < batch.count else {
                        // Pi returned a rejection but the index didn't
                        // map back to a known item. Treat this batch
                        // as containing one extra rejection we
                        // couldn't attribute — the plugin's commit
                        // gate uses the aggregate count, so the
                        // cursor is correctly held.
                        rejections.append(ICloudContactsRejection(
                            sourceID: "<unattributed>", kind: "<unknown>",
                            code: err.code, message: err.message))
                        logger.warning("icloud publish: out-of-range rejection index", metadata: [
                            "index": .public(String(err.index)),
                            "batch_size": .public(String(batch.count)),
                            "code": .public(err.code),
                        ])
                        if Self.recoveryCodes.contains(err.code) {
                            batchRecoveryHit = true
                        }
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
                    if Self.recoveryCodes.contains(err.code) {
                        batchRecoveryHit = true
                    }
                }
                // Pi-reported `rejected` count that exceeds the
                // `errors` array indicates lost rejection details —
                // synthesize placeholder entries so the caller's
                // commit gate (`rejected.isEmpty`) trips correctly.
                let attributed = response.errors.count
                if response.rejected > attributed {
                    let missing = response.rejected - attributed
                    for _ in 0..<missing {
                        rejections.append(ICloudContactsRejection(
                            sourceID: "<unattributed>", kind: "<unknown>",
                            code: "UNATTRIBUTED_REJECTION",
                            message: "Pi reported rejected=\(response.rejected) but errors only carries \(attributed) entries"))
                    }
                    logger.warning("icloud publish: Pi rejected count exceeds errors[] length", metadata: [
                        "rejected_count": .public(String(response.rejected)),
                        "errors_count": .public(String(attributed)),
                    ])
                }
                anyBatchSucceeded = true
                remainingItemsAfterBatch -= batch.count
                if batchRecoveryHit {
                    // Abort: don't send subsequent batches. The
                    // unsent items count as `unconfirmed` so the
                    // plugin's gate (`unconfirmed == 0`) holds the
                    // cursor.
                    sawRecoveryCode = true
                    unconfirmed += remainingItemsAfterBatch
                    break
                }
            } catch {
                logger.warning("icloud publish: batch failed", metadata: [
                    "batch_index": .public(String(i)),
                    "batch_size": .public(String(batch.count)),
                    "error": .private(String(describing: error)),
                ])
                unconfirmed += remainingItemsAfterBatch
                break
            }
        }

        if sawRecoveryCode {
            logger.warning("icloud publish: hash-mismatch rejection aborted subsequent batches", metadata: [
                "remaining_unconfirmed": .public(String(unconfirmed)),
            ])
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
