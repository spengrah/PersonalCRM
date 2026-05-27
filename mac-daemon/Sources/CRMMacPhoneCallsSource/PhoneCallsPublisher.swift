// PhoneCallsPublisher — batches shaped CallPayloads + POSTs them to
// the Pi's /api/v1/ingest/events endpoint.
//
// Batching rules:
//   - Cap at 200 events per batch (well below Pi's hard 500 limit).
//   - Cap at 1 MB body bytes per batch (well below Pi's 8 MiB limit).
//   - Compute the body size pre-emptively: if appending the next event
//     would push the batch over either threshold, flush first.
//
// Cursor advance rules:
//   - Cursor advances ONLY when rejected == 0.
//   - On per-event rejection, log warning + DO NOT advance the cursor.
//   - On transport failure, cursor stays put for the next tick.
//
// Test-driven design: the network call is injected as a closure so
// PhoneCallsPublisherTests can drive batching + outcome aggregation
// without spinning up a URLSession.
import Foundation
import CRMMacCore
import CRMMacPiClient

/// Result of publishing one or more events. Mirrors
/// MessagesBatchOutcome — different cursor primitive (CallCursorPoint
/// pair vs Int64 ROWID) but same shape.
public struct PhoneCallsBatchOutcome: Equatable, Sendable {
    public let accepted: Int
    public let duplicate: Int
    public let rejected: [PhoneCallPerEventRejection]
    /// Items submitted but neither accepted/duplicated nor rejected by
    /// the Pi — lost in transit (transport / 5xx after retries). Caller
    /// MUST treat any nonzero value as "do not advance the cursor".
    public let unconfirmed: Int
    /// Highest (ZDATE, Z_PK) confirmed by the Pi. Nil if no batch
    /// completed cleanly.
    public let advanceTo: CallCursorPoint?

    public init(
        accepted: Int,
        duplicate: Int,
        rejected: [PhoneCallPerEventRejection],
        unconfirmed: Int,
        advanceTo: CallCursorPoint?
    ) {
        self.accepted = accepted
        self.duplicate = duplicate
        self.rejected = rejected
        self.unconfirmed = unconfirmed
        self.advanceTo = advanceTo
    }
}

public struct PhoneCallPerEventRejection: Equatable, Sendable {
    public let zPK: Int64
    public let callUniqueID: String
    public let code: String
    public let message: String

    public init(zPK: Int64, callUniqueID: String, code: String, message: String) {
        self.zPK = zPK
        self.callUniqueID = callUniqueID
        self.code = code
        self.message = message
    }
}

/// One row queued for publish: the shaped payload + the source row's
/// (ZDATE, Z_PK) coordinate so the caller can track cursor advance.
public struct PhoneCallPublishItem: Sendable {
    public let cursorPoint: CallCursorPoint
    public let direction: CallDirection
    public let payload: CallPayload

    public init(cursorPoint: CallCursorPoint, direction: CallDirection, payload: CallPayload) {
        self.cursorPoint = cursorPoint
        self.direction = direction
        self.payload = payload
    }
}

public actor PhoneCallsPublisher {
    /// Cap on events per batch (well below Pi's hard 500).
    public static let maxEventsPerBatch: Int = 200
    /// Cap on body bytes per batch (well below Pi's 8 MiB).
    public static let maxBodyBytes: Int = 1 * 1024 * 1024

    /// Closure-injected for testability. Production passes
    /// PiClient.ingestEvents; tests pass a stub.
    public typealias Sender = @Sendable (PiAuth, IngestEventsBody) async throws -> IngestEventsData

    private let sender: Sender
    private let auth: PiAuth
    private let logger: LoggerProtocol
    private let clock: () -> Date

    public init(
        sender: @escaping Sender,
        auth: PiAuth,
        logger: LoggerProtocol,
        clock: @escaping () -> Date = { Date() }
    ) {
        self.sender = sender
        self.auth = auth
        self.logger = logger
        self.clock = clock
    }

    /// Publish `items` in one or more batches sized per
    /// `maxEventsPerBatch` and `maxBodyBytes`. Returns an aggregated
    /// outcome.
    ///
    /// Caller is responsible for the cursor-advance decision: advance
    /// ONLY when `rejected.isEmpty && unconfirmed == 0`.
    public func publish(items: [PhoneCallPublishItem]) async -> PhoneCallsBatchOutcome {
        if items.isEmpty {
            return PhoneCallsBatchOutcome(
                accepted: 0, duplicate: 0,
                rejected: [], unconfirmed: 0, advanceTo: nil)
        }

        var totalAccepted = 0
        var totalDuplicate = 0
        var unconfirmed = 0
        var rejections: [PhoneCallPerEventRejection] = []
        var lastSuccessfulBatchHighestPoint: CallCursorPoint?

        let encoder = JSONEncoder()
        encoder.dateEncodingStrategy = .iso8601
        encoder.outputFormatting = [.withoutEscapingSlashes]

        var currentBatch: [PhoneCallPublishItem] = []
        var currentBatchBytes = 0

        for item in items {
            let envelope = makeIngestEvent(from: item, encoder: encoder)
            let eventBytes = (try? encoder.encode(envelope))?.count ?? 0
            let perEventOverhead = 16

            if !currentBatch.isEmpty,
               currentBatch.count >= Self.maxEventsPerBatch ||
                 currentBatchBytes + eventBytes + perEventOverhead > Self.maxBodyBytes {
                let outcome = await flushBatch(
                    items: currentBatch,
                    encoder: encoder,
                    rejections: &rejections)
                totalAccepted += outcome.accepted
                totalDuplicate += outcome.duplicate
                if outcome.advanced {
                    lastSuccessfulBatchHighestPoint = outcome.highestPoint
                } else if outcome.transportFailed {
                    let stillUnflushed = items.count -
                        (totalAccepted + totalDuplicate + rejections.count
                          + currentBatch.count)
                    unconfirmed += currentBatch.count + max(0, stillUnflushed)
                    return PhoneCallsBatchOutcome(
                        accepted: totalAccepted,
                        duplicate: totalDuplicate,
                        rejected: rejections,
                        unconfirmed: unconfirmed,
                        advanceTo: lastSuccessfulBatchHighestPoint)
                }
                currentBatch = []
                currentBatchBytes = 0
            }
            currentBatch.append(item)
            currentBatchBytes += eventBytes + perEventOverhead
        }

        if !currentBatch.isEmpty {
            let outcome = await flushBatch(
                items: currentBatch,
                encoder: encoder,
                rejections: &rejections)
            totalAccepted += outcome.accepted
            totalDuplicate += outcome.duplicate
            if outcome.advanced {
                lastSuccessfulBatchHighestPoint = outcome.highestPoint
            } else if outcome.transportFailed {
                unconfirmed += currentBatch.count
            }
        }

        return PhoneCallsBatchOutcome(
            accepted: totalAccepted,
            duplicate: totalDuplicate,
            rejected: rejections,
            unconfirmed: unconfirmed,
            advanceTo: lastSuccessfulBatchHighestPoint)
    }

    private struct BatchOutcome {
        let accepted: Int
        let duplicate: Int
        let highestPoint: CallCursorPoint?
        let advanced: Bool
        let transportFailed: Bool
    }

    private func flushBatch(
        items: [PhoneCallPublishItem],
        encoder: JSONEncoder,
        rejections: inout [PhoneCallPerEventRejection]
    ) async -> BatchOutcome {
        let envelopes = items.map { makeIngestEvent(from: $0, encoder: encoder) }
        let body = IngestEventsBody(events: envelopes)

        // Highest (lexicographic max) of the batch's cursor points so
        // the caller can advance past the batch's high-water mark.
        var highest: CallCursorPoint?
        for item in items {
            if let h = highest {
                if (item.cursorPoint.zdate, item.cursorPoint.zPK)
                    > (h.zdate, h.zPK) {
                    highest = item.cursorPoint
                }
            } else {
                highest = item.cursorPoint
            }
        }

        let response: IngestEventsData
        do {
            response = try await sender(auth, body)
        } catch {
            logger.warning("phone_calls publish: batch failed", metadata: [
                "error": .private(String(describing: error)),
                "batch_size": .public(String(items.count)),
            ])
            return BatchOutcome(
                accepted: 0, duplicate: 0,
                highestPoint: nil,
                advanced: false,
                transportFailed: true)
        }

        for err in response.errors {
            guard err.index >= 0, err.index < items.count else {
                logger.warning("phone_calls publish: rejection index out of range", metadata: [
                    "index": .public(String(err.index)),
                    "batch_size": .public(String(items.count)),
                ])
                continue
            }
            let item = items[err.index]
            rejections.append(PhoneCallPerEventRejection(
                zPK: item.cursorPoint.zPK,
                callUniqueID: item.payload.callUniqueID,
                code: err.code,
                message: err.message))
            logger.warning("phone_calls publish: per-event rejection", metadata: [
                "call_unique_id": .private(item.payload.callUniqueID),
                "code": .public(err.code),
                "message": .public(err.message),
            ])
        }

        let advanced = response.rejected == 0
        return BatchOutcome(
            accepted: response.accepted,
            duplicate: response.duplicate,
            highestPoint: highest,
            advanced: advanced,
            transportFailed: false)
    }

    private func makeIngestEvent(
        from item: PhoneCallPublishItem,
        encoder: JSONEncoder
    ) -> IngestEvent {
        let payloadBytes: Data
        do {
            payloadBytes = try encoder.encode(item.payload)
        } catch {
            // Shouldn't happen — CallPayload is total. Fall back to an
            // empty payload + logged warning.
            logger.warning("phone_calls publish: payload encode failed", metadata: [
                "call_unique_id": .private(item.payload.callUniqueID),
                "error": .public(String(describing: error)),
            ])
            payloadBytes = Data("{}".utf8)
        }
        return IngestEvent(
            source: "phone_calls",
            sourceID: item.payload.callUniqueID,
            kind: item.direction.rawValue,
            payload: RawJSON(payloadBytes),
            observedAt: clock())
    }
}
