// AnarlogHumansPublisher — batches external_contact.* IngestEvents
// for source=anarlog_humans and posts them to /api/v1/ingest/events.
//
// Mirrors ICloudContactsPublisher's semantics:
//   - 100 events per batch (well under the Pi's 500 hard cap)
//   - 1 MiB max body per batch (parity with messages source)
//   - per-event recovery codes abort subsequent batches in this tick
//     AND request a recovery cycle on the next tick
//   - per-event rejections are returned to the plugin; cursor stays
//     held until rejections clear
import Foundation
import CRMMacCore
import CRMMacPiClient

/// One pending external_contact event ready to publish.
public struct AnarlogHumansPublishItem: Sendable {
    public let sourceID: String
    /// Either `external_contact.upserted` or `external_contact.deleted`.
    public let kind: String
    public let payloadBytes: Data

    public init(sourceID: String, kind: String, payloadBytes: Data) {
        self.sourceID = sourceID
        self.kind = kind
        self.payloadBytes = payloadBytes
    }
}

public struct AnarlogHumansRejection: Equatable, Sendable {
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

public struct AnarlogHumansBatchOutcome: Equatable, Sendable {
    public let accepted: Int
    public let duplicate: Int
    public let rejected: [AnarlogHumansRejection]
    /// Items submitted but neither accepted/duplicated nor rejected.
    /// Any nonzero value means "do not advance the cursor."
    public let unconfirmed: Int
    public let anyBatchSucceeded: Bool

    public init(
        accepted: Int,
        duplicate: Int,
        rejected: [AnarlogHumansRejection],
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

public typealias AnarlogHumansIngestSender =
    @Sendable (PiAuth, IngestEventsBody) async throws -> IngestEventsData

public actor AnarlogHumansPublisher {
    public static let maxEventsPerBatch: Int = 100
    public static let maxBodyBytes: Int = 1 * 1024 * 1024

    private let sender: AnarlogHumansIngestSender
    private let auth: PiAuth
    private let logger: LoggerProtocol
    private let clock: @Sendable () -> Date

    public init(
        sender: @escaping AnarlogHumansIngestSender,
        auth: PiAuth,
        logger: LoggerProtocol,
        clock: @escaping @Sendable () -> Date = { Date() }
    ) {
        self.sender = sender
        self.auth = auth
        self.logger = logger
        self.clock = clock
    }

    /// Per-event rejection codes that signal a daemon-Pi divergence.
    /// Active once the Pi-side handler ships acceptance for
    /// source=anarlog_humans; harmless if the Pi never returns them
    /// (the plugin's commit gate uses rejected.isEmpty so any
    /// rejection holds the cursor).
    public static let recoveryCodes: Set<String> = [
        "EXTERNAL_CONTACT_HASH_MISMATCH",
        "EXTERNAL_CONTACT_DELETE_HASH_MISMATCH",
    ]

    public func publish(items: [AnarlogHumansPublishItem]) async -> AnarlogHumansBatchOutcome {
        if items.isEmpty {
            return AnarlogHumansBatchOutcome(
                accepted: 0, duplicate: 0, rejected: [],
                unconfirmed: 0, anyBatchSucceeded: false)
        }

        var totalAccepted = 0
        var totalDuplicate = 0
        var rejections: [AnarlogHumansRejection] = []
        var unconfirmed = 0
        var anyBatchSucceeded = false
        var sawRecoveryCode = false

        let batches = splitIntoBatches(items)
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
                        rejections.append(AnarlogHumansRejection(
                            sourceID: "<unattributed>", kind: "<unknown>",
                            code: err.code, message: err.message))
                        logger.warning("anarlog_humans publish: out-of-range rejection index", metadata: [
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
                    rejections.append(AnarlogHumansRejection(
                        sourceID: item.sourceID, kind: item.kind,
                        code: err.code, message: err.message))
                    logger.warning("anarlog_humans publish: per-event rejection", metadata: [
                        "source_id": .private(item.sourceID),
                        "kind": .public(item.kind),
                        "code": .public(err.code),
                        "message": .public(err.message),
                    ])
                    if Self.recoveryCodes.contains(err.code) {
                        batchRecoveryHit = true
                    }
                }
                let attributed = response.errors.count
                if response.rejected > attributed {
                    let missing = response.rejected - attributed
                    for _ in 0..<missing {
                        rejections.append(AnarlogHumansRejection(
                            sourceID: "<unattributed>", kind: "<unknown>",
                            code: "UNATTRIBUTED_REJECTION",
                            message: "Pi reported rejected=\(response.rejected) but errors only carries \(attributed) entries"))
                    }
                    logger.warning("anarlog_humans publish: Pi rejected count exceeds errors[] length", metadata: [
                        "rejected_count": .public(String(response.rejected)),
                        "errors_count": .public(String(attributed)),
                    ])
                }
                anyBatchSucceeded = true
                remainingItemsAfterBatch -= batch.count
                if batchRecoveryHit {
                    sawRecoveryCode = true
                    unconfirmed += remainingItemsAfterBatch
                    break
                }
            } catch {
                logger.warning("anarlog_humans publish: batch failed", metadata: [
                    "batch_index": .public(String(i)),
                    "batch_size": .public(String(batch.count)),
                    "error": .private(String(describing: error)),
                ])
                unconfirmed += remainingItemsAfterBatch
                break
            }
        }

        if sawRecoveryCode {
            logger.warning("anarlog_humans publish: hash-mismatch rejection aborted subsequent batches", metadata: [
                "remaining_unconfirmed": .public(String(unconfirmed)),
            ])
        }

        return AnarlogHumansBatchOutcome(
            accepted: totalAccepted,
            duplicate: totalDuplicate,
            rejected: rejections,
            unconfirmed: unconfirmed,
            anyBatchSucceeded: anyBatchSucceeded)
    }

    // MARK: - private

    private func splitIntoBatches(
        _ items: [AnarlogHumansPublishItem]
    ) -> [[AnarlogHumansPublishItem]] {
        var batches: [[AnarlogHumansPublishItem]] = []
        var current: [AnarlogHumansPublishItem] = []
        var currentBytes = 0
        let perEventOverhead = 16
        for item in items {
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

    private func makeIngestEvent(from item: AnarlogHumansPublishItem) -> IngestEvent {
        IngestEvent(
            source: SourceID.anarlogHumans.rawValue,
            sourceID: item.sourceID,
            kind: item.kind,
            payload: RawJSON(item.payloadBytes),
            observedAt: clock())
    }
}
