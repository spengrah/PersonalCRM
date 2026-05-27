// AnarlogSessionsPublisher — batches meeting_note.* IngestEvents for
// source=anarlog_sessions and posts them to /api/v1/ingest/events.
//
// Same shape as AnarlogHumansPublisher; per-source split exists so the
// recovery-code set can differ (meeting_note has its own anticipated
// PR 3 codes; humans uses external_contact_*).
//
// The two publishers are intentionally not merged into a single
// generic — the in-flight set of recovery codes is small + each
// source's mistakes should not abort the other.
import Foundation
import CRMMacCore
import CRMMacPiClient

public struct AnarlogSessionsPublishItem: Sendable {
    public let sourceID: String
    /// Either `meeting_note.recorded` or `meeting_note.deleted`.
    public let kind: String
    public let payloadBytes: Data

    public init(sourceID: String, kind: String, payloadBytes: Data) {
        self.sourceID = sourceID
        self.kind = kind
        self.payloadBytes = payloadBytes
    }
}

public struct AnarlogSessionsRejection: Equatable, Sendable {
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

public struct AnarlogSessionsBatchOutcome: Equatable, Sendable {
    public let accepted: Int
    public let duplicate: Int
    public let rejected: [AnarlogSessionsRejection]
    public let unconfirmed: Int
    public let anyBatchSucceeded: Bool

    public init(
        accepted: Int,
        duplicate: Int,
        rejected: [AnarlogSessionsRejection],
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

public typealias AnarlogSessionsIngestSender =
    @Sendable (PiAuth, IngestEventsBody) async throws -> IngestEventsData

public actor AnarlogSessionsPublisher {
    public static let maxEventsPerBatch: Int = 100
    public static let maxBodyBytes: Int = 1 * 1024 * 1024

    private let sender: AnarlogSessionsIngestSender
    private let auth: PiAuth
    private let logger: LoggerProtocol
    private let clock: @Sendable () -> Date

    public init(
        sender: @escaping AnarlogSessionsIngestSender,
        auth: PiAuth,
        logger: LoggerProtocol,
        clock: @escaping @Sendable () -> Date = { Date() }
    ) {
        self.sender = sender
        self.auth = auth
        self.logger = logger
        self.clock = clock
    }

    /// Anticipated PR 3 codes for meeting_note hash mismatch. Listed
    /// here ahead of Pi-side acceptance so the plugin already
    /// recognizes them on the day the acceptance ships. Pi will never
    /// emit these in PR 2 (all events get UNKNOWN_KIND rejected).
    public static let recoveryCodes: Set<String> = [
        "MEETING_NOTE_HASH_MISMATCH",
        "MEETING_NOTE_DELETE_HASH_MISMATCH",
    ]

    public func publish(items: [AnarlogSessionsPublishItem]) async -> AnarlogSessionsBatchOutcome {
        if items.isEmpty {
            return AnarlogSessionsBatchOutcome(
                accepted: 0, duplicate: 0, rejected: [],
                unconfirmed: 0, anyBatchSucceeded: false)
        }

        var totalAccepted = 0
        var totalDuplicate = 0
        var rejections: [AnarlogSessionsRejection] = []
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
                        rejections.append(AnarlogSessionsRejection(
                            sourceID: "<unattributed>", kind: "<unknown>",
                            code: err.code, message: err.message))
                        logger.warning("anarlog_sessions publish: out-of-range rejection index", metadata: [
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
                    rejections.append(AnarlogSessionsRejection(
                        sourceID: item.sourceID, kind: item.kind,
                        code: err.code, message: err.message))
                    logger.warning("anarlog_sessions publish: per-event rejection", metadata: [
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
                        rejections.append(AnarlogSessionsRejection(
                            sourceID: "<unattributed>", kind: "<unknown>",
                            code: "UNATTRIBUTED_REJECTION",
                            message: "Pi reported rejected=\(response.rejected) but errors only carries \(attributed) entries"))
                    }
                    logger.warning("anarlog_sessions publish: Pi rejected count exceeds errors[] length", metadata: [
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
                logger.warning("anarlog_sessions publish: batch failed", metadata: [
                    "batch_index": .public(String(i)),
                    "batch_size": .public(String(batch.count)),
                    "error": .private(String(describing: error)),
                ])
                unconfirmed += remainingItemsAfterBatch
                break
            }
        }

        if sawRecoveryCode {
            logger.warning("anarlog_sessions publish: hash-mismatch rejection aborted subsequent batches", metadata: [
                "remaining_unconfirmed": .public(String(unconfirmed)),
            ])
        }

        return AnarlogSessionsBatchOutcome(
            accepted: totalAccepted,
            duplicate: totalDuplicate,
            rejected: rejections,
            unconfirmed: unconfirmed,
            anyBatchSucceeded: anyBatchSucceeded)
    }

    // MARK: - private

    private func splitIntoBatches(
        _ items: [AnarlogSessionsPublishItem]
    ) -> [[AnarlogSessionsPublishItem]] {
        var batches: [[AnarlogSessionsPublishItem]] = []
        var current: [AnarlogSessionsPublishItem] = []
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

    private func makeIngestEvent(from item: AnarlogSessionsPublishItem) -> IngestEvent {
        IngestEvent(
            source: SourceID.anarlogSessions.rawValue,
            sourceID: item.sourceID,
            kind: item.kind,
            payload: RawJSON(item.payloadBytes),
            observedAt: clock())
    }
}
