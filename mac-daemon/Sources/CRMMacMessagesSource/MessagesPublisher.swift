// MessagesPublisher — batches shaped payloads + POSTs them to the
// Pi's /api/v1/ingest/events endpoint.
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
// MessagesPublisherTests can drive batching + outcome aggregation
// without spinning up a URLSession.
import Foundation
import CRMMacCore
import CRMMacPiClient

/// Result of publishing one or more events.
public struct MessagesBatchOutcome: Equatable, Sendable {
    public let accepted: Int
    public let duplicate: Int
    public let rejected: [PerEventRejection]
    /// Items submitted but neither accepted/duplicated nor rejected
    /// by the Pi — i.e. lost in transit (transport / 5xx after
    /// retries). Caller MUST treat any nonzero value as "do not
    /// advance the cursor": those rows didn't reach the Pi.
    public let unconfirmed: Int
    /// Highest ROWID confirmed by the Pi.  Nil if the transport
    /// failed entirely (no batches completed).
    ///
    /// IMPORTANT: presence of advanceTo does NOT mean all items
    /// reached the Pi — partial success during multi-batch publish
    /// can produce advanceTo != nil AND unconfirmed > 0. Caller MUST
    /// gate cursor advance on `rejected.isEmpty && unconfirmed == 0`.
    public let advanceTo: Int64?

    public init(accepted: Int, duplicate: Int,
                rejected: [PerEventRejection],
                unconfirmed: Int,
                advanceTo: Int64?) {
        self.accepted = accepted
        self.duplicate = duplicate
        self.rejected = rejected
        self.unconfirmed = unconfirmed
        self.advanceTo = advanceTo
    }
}

public struct PerEventRejection: Equatable, Sendable {
    public let rowID: Int64
    public let guid: String
    public let code: String
    public let message: String

    public init(rowID: Int64, guid: String, code: String, message: String) {
        self.rowID = rowID
        self.guid = guid
        self.code = code
        self.message = message
    }
}

/// One row queued for publish: the shaped payload + the chat.db ROWID
/// so the caller can track cursor advance.
public struct PublishItem: Sendable {
    public let rowID: Int64
    public let direction: MessageDirection
    public let payload: RawMessagePayload

    public init(rowID: Int64, direction: MessageDirection,
                payload: RawMessagePayload) {
        self.rowID = rowID
        self.direction = direction
        self.payload = payload
    }
}

/// Closure shape for injecting the Pi-side POST.  Real wiring passes
/// piClient.ingestEvents; tests pass a stub.
public typealias IngestEventsSender = @Sendable (PiAuth, IngestEventsBody) async throws -> IngestEventsData

public actor MessagesPublisher {
    /// Cap on events per batch (well below Pi's hard 500).
    public static let maxEventsPerBatch: Int = 200
    /// Cap on body bytes per batch (well below Pi's 8 MiB).
    public static let maxBodyBytes: Int = 1 * 1024 * 1024

    private let sender: IngestEventsSender
    private let auth: PiAuth
    private let logger: LoggerProtocol
    private let clock: () -> Date

    public init(
        sender: @escaping IngestEventsSender,
        auth: PiAuth,
        logger: LoggerProtocol,
        clock: @escaping () -> Date = { Date() }
    ) {
        self.sender = sender
        self.auth = auth
        self.logger = logger
        self.clock = clock
    }

    /// Publish `items` in one or more batches sized per maxEventsPerBatch
    /// and maxBodyBytes.  Returns an aggregated outcome.
    ///
    /// Caller is responsible for the cursor-advance decision based on
    /// the returned outcome: advance ONLY when
    /// `rejected.isEmpty && unconfirmed == 0`.
    public func publish(items: [PublishItem]) async -> MessagesBatchOutcome {
        if items.isEmpty {
            return MessagesBatchOutcome(
                accepted: 0, duplicate: 0,
                rejected: [], unconfirmed: 0, advanceTo: nil)
        }

        var totalAccepted = 0
        var totalDuplicate = 0
        var unconfirmed = 0
        var rejections: [PerEventRejection] = []
        var lastSuccessfulBatchHighestROWID: Int64? = nil

        let encoder = JSONEncoder()
        encoder.dateEncodingStrategy = .iso8601
        encoder.outputFormatting = [.withoutEscapingSlashes]

        var currentBatch: [PublishItem] = []
        var currentBatchBytes = 0

        for item in items {
            // Pre-emptively size the encoded event.
            let envelope = self.makeIngestEvent(from: item, encoder: encoder)
            let eventBytes = (try? encoder.encode(envelope))?.count ?? 0
            // Reserve a few bytes per event for JSON commas/braces overhead.
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
                    lastSuccessfulBatchHighestROWID = outcome.highestROWID
                } else if outcome.transportFailed {
                    // Transport failure on this batch + everything
                    // after this point in the input is unsubmitted.
                    let stillUnflushed = items.count -
                        (totalAccepted + totalDuplicate + rejections.count
                          + currentBatch.count)
                    unconfirmed += currentBatch.count + max(0, stillUnflushed)
                    return MessagesBatchOutcome(
                        accepted: totalAccepted,
                        duplicate: totalDuplicate,
                        rejected: rejections,
                        unconfirmed: unconfirmed,
                        advanceTo: lastSuccessfulBatchHighestROWID)
                }
                currentBatch = []
                currentBatchBytes = 0
            }
            currentBatch.append(item)
            currentBatchBytes += eventBytes + perEventOverhead
        }

        // Final flush.
        if !currentBatch.isEmpty {
            let outcome = await flushBatch(
                items: currentBatch,
                encoder: encoder,
                rejections: &rejections)
            totalAccepted += outcome.accepted
            totalDuplicate += outcome.duplicate
            if outcome.advanced {
                lastSuccessfulBatchHighestROWID = outcome.highestROWID
            } else if outcome.transportFailed {
                unconfirmed += currentBatch.count
            }
        }

        return MessagesBatchOutcome(
            accepted: totalAccepted,
            duplicate: totalDuplicate,
            rejected: rejections,
            unconfirmed: unconfirmed,
            advanceTo: lastSuccessfulBatchHighestROWID)
    }

    private struct BatchOutcome {
        let accepted: Int
        let duplicate: Int
        let highestROWID: Int64?
        let advanced: Bool
        let transportFailed: Bool
    }

    /// Send a single batch.  Returns the per-batch outcome.
    private func flushBatch(
        items: [PublishItem],
        encoder: JSONEncoder,
        rejections: inout [PerEventRejection]
    ) async -> BatchOutcome {
        let envelopes = items.map { makeIngestEvent(from: $0, encoder: encoder) }
        let body = IngestEventsBody(events: envelopes)

        let highestROWID = items.map(\.rowID).max()
        let response: IngestEventsData
        do {
            response = try await sender(auth, body)
        } catch {
            logger.warning("messages publish: batch failed", metadata: [
                "error": .private(String(describing: error)),
                "batch_size": .public(String(items.count)),
            ])
            return BatchOutcome(
                accepted: 0, duplicate: 0,
                highestROWID: nil,
                advanced: false,
                transportFailed: true)
        }

        // Aggregate per-event rejections by mapping the response's
        // errors[].index back to the original item.
        for err in response.errors {
            guard err.index >= 0, err.index < items.count else {
                // Server returned an out-of-bounds index — log and skip.
                logger.warning("messages publish: rejection index out of range", metadata: [
                    "index": .public(String(err.index)),
                    "batch_size": .public(String(items.count)),
                ])
                continue
            }
            let item = items[err.index]
            rejections.append(PerEventRejection(
                rowID: item.rowID,
                guid: item.payload.guid,
                code: err.code,
                message: err.message))
            logger.warning("messages publish: per-event rejection", metadata: [
                "guid": .private(item.payload.guid),
                "code": .public(err.code),
                "message": .public(err.message),
            ])
        }

        // Per the spec: cursor advances ONLY when rejected == 0.
        // BatchOutcome.advanced reflects "this batch had no rejections";
        // the caller's caller decides cursor advance across all batches.
        let advanced = response.rejected == 0
        return BatchOutcome(
            accepted: response.accepted,
            duplicate: response.duplicate,
            highestROWID: highestROWID,
            advanced: advanced,
            transportFailed: false)
    }

    /// Build the IngestEvent envelope for a PublishItem. Encodes the
    /// inner payload to RawJSON so the encoder inlines it as a nested
    /// JSON object (NOT base64).
    private func makeIngestEvent(from item: PublishItem,
                                  encoder: JSONEncoder) -> IngestEvent {
        let payloadBytes: Data
        do {
            payloadBytes = try encoder.encode(item.payload)
        } catch {
            // Shouldn't happen — RawMessagePayload is total. Fall back to
            // an empty payload + logged warning.
            logger.warning("messages publish: payload encode failed", metadata: [
                "guid": .private(item.payload.guid),
                "error": .public(String(describing: error)),
            ])
            payloadBytes = Data("{}".utf8)
        }
        return IngestEvent(
            source: "messages",
            sourceID: item.payload.guid,
            kind: item.direction.rawValue,
            payload: RawJSON(payloadBytes),
            observedAt: clock())
    }
}
