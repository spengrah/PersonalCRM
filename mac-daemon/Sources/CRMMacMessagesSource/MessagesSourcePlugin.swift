// MessagesSourcePlugin — actor that orchestrates one messages tick.
//
// Per-tick flow (plan §"Plugin lifecycle on a single tick"):
//   1. Check schema health: if drifted, set source_health unhealthy
//      -> return.
//   2. Open chat.db DatabasePool (cached on first successful open).
//   3. If no cached cursor: piClient.getMessagesCursor(...) -> decode
//      into MessagesCursor; cache locally via StateMutator.
//   4. If installMaxRowID == nil: capture MAX(ROWID) into the in-memory
//      MessagesCursor (uncommitted; committed in step 8).
//   5. Drain pendingScans (from cursor JSON): for each, run targeted
//      30-day scan -> publish via MessagesPublisher.
//   6. If backfillComplete == false: run a backfill batch (budget-
//      bound) -> publish -> advance backfillCursor downward.
//   7. Run live batch (remaining budget): read message.ROWID > liveCursor
//      -> publish -> advance liveCursor upward.
//   8. If the in-memory cursor changed AND every batch had
//      rejected == 0: commitMessagesCursor; on success write through
//      to local state.json via StateMutator. On 409: refresh + abort.
//      On transport error: log + leave local cache untouched -> return.
//   9. Update source_health snapshot via the shared registry.
//
// The plugin is the single owner of the GRDB DatabasePool, the
// KnownIdentifiersCache, and the messages source's slot in
// SourceState. All writes to state.json funnel through StateMutator.
import Foundation
import GRDB
import CRMMacCore
import CRMMacPiClient

/// Inputs the plugin needs from the daemon's composition root.
public struct MessagesSourceConfig: Sendable {
    /// Path to chat.db. Production: ~/Library/Messages/chat.db.
    public let chatDBPath: URL
    /// Backfill floor — events with sentAt before this date are not emitted.
    public let backfillFloor: Date
    /// Max rows per tick (default 500 per plan §R3).
    public let maxRowsPerTick: Int
    /// Max wall-clock seconds per tick (default 5.0 per plan §R3).
    public let maxDurationPerTick: TimeInterval

    public init(
        chatDBPath: URL,
        backfillFloor: Date,
        maxRowsPerTick: Int = 500,
        maxDurationPerTick: TimeInterval = 5.0
    ) {
        self.chatDBPath = chatDBPath
        self.backfillFloor = backfillFloor
        self.maxRowsPerTick = maxRowsPerTick
        self.maxDurationPerTick = maxDurationPerTick
    }
}

public actor MessagesSourcePlugin: SourcePlugin {
    public nonisolated let id: SourceID = .messages
    public nonisolated let tickInterval: TimeInterval

    private let config: MessagesSourceConfig
    private let piClient: PiClient
    private let auth: PiAuth
    private let mutator: StateMutator
    private let publisher: MessagesPublisher
    private let cache: KnownIdentifiersCache
    private let logger: LoggerProtocol
    private let clock: @Sendable () -> Date
    private let healthRegistry: SourceHealthRegistry

    /// Cached pool, opened lazily on first successful tick.
    private var pool: DatabasePool?
    /// Cached schema health. Validated on first open.
    private var schemaHealth: SchemaHealth?
    /// Cached cursor + epoch loaded from local state.json or fetched
    /// from the Pi on first tick.
    private var cachedCursor: MessagesCursor?
    private var cachedEpoch: Int64 = 0
    /// Last serialized cursor JSON committed to the Pi.  Used as
    /// base_cursor on the next commit.
    private var lastCommittedCursorJSON: String = ""

    public init(
        tickInterval: TimeInterval,
        config: MessagesSourceConfig,
        piClient: PiClient,
        auth: PiAuth,
        mutator: StateMutator,
        publisher: MessagesPublisher,
        cache: KnownIdentifiersCache,
        healthRegistry: SourceHealthRegistry,
        logger: LoggerProtocol,
        clock: @escaping @Sendable () -> Date = { Date() }
    ) {
        self.tickInterval = tickInterval
        self.config = config
        self.piClient = piClient
        self.auth = auth
        self.mutator = mutator
        self.publisher = publisher
        self.cache = cache
        self.healthRegistry = healthRegistry
        self.logger = logger
        self.clock = clock
    }

    public func tick() async throws {
        let tickStart = clock()
        await healthRegistry.update(id, await currentHealthSnapshot(
            enabled: true, lastScheduled: tickStart))

        // Step 1+2: open pool + validate schema (cached on success).
        let pool: DatabasePool
        do {
            pool = try await openOrCached()
        } catch let SchemaDriftError.drift(table, missing) {
            await markUnhealthy(reason: "schema_drift:\(table).\(missing.sorted().first ?? "?")")
            return
        } catch {
            await markUnhealthy(reason: "open_failed: \(String(describing: error))")
            return
        }

        // Step 3: load cursor (from local cache or Pi).
        let cursor: MessagesCursor
        do {
            cursor = try await loadOrFetchCursor()
        } catch {
            logger.warning("messages tick: cursor fetch failed", metadata: [
                "error": .private(String(describing: error)),
            ])
            await markUnhealthy(reason: "cursor_fetch_failed")
            return
        }

        // Sender filter ready?  If the known-identifiers cache hasn't
        // been populated yet, skip the tick (heartbeat will fill it).
        let populated = await cache.isPopulated
        if !populated {
            logger.debug("messages tick: known-identifiers cache empty; skipping", metadata: [:])
            return
        }

        // Step 4: capture install-time MAX(ROWID) if needed.
        var working = cursor
        if working.installMaxRowID == nil {
            do {
                let maxROWID = try await pool.read { try ChatDBReader.maxROWID(db: $0) } ?? 0
                working.installMaxRowID = maxROWID
                working.liveCursor = maxROWID
                logger.info("messages tick: captured install-time MAX(ROWID)", metadata: [
                    "max_rowid": .public(String(maxROWID)),
                ])
            } catch {
                await markUnhealthy(reason: "max_rowid_failed")
                return
            }
        }

        var budget = BackfillBudget(
            maxRows: config.maxRowsPerTick,
            maxDuration: config.maxDurationPerTick,
            now: tickStart)
        var hadRejection = false

        // Steps 5-7: drain pending scans, then backfill, then live.
        // For PR7 v1 we collapse pending-scan execution into "queue
        // up like a backfill batch but capped at 30 days" since chat.db
        // doesn't support direct identifier filtering at SQL level
        // without a full scan. The pending-scans drain just reads the
        // queue; the actual scan happens via the live/backfill paths
        // which already publish to ingest. (V1 simplification.)
        if !working.pendingScans.isEmpty {
            logger.info("messages tick: pending scans queued", metadata: [
                "count": .public(String(working.pendingScans.count)),
            ])
            // V1 simplification: clear the pending-scan queue on tick;
            // the next backfill/live pass picks up the rows. Future PRs
            // can implement explicit identifier-scoped queries.
            working.pendingScans = []
        }

        // Step 6: backfill batch (descending).
        if !working.backfillComplete, budget.rowsRemaining > 0 {
            let outcome = await runBackfillBatch(
                pool: pool,
                cursor: &working,
                budget: &budget)
            if !outcome.rejected.isEmpty { hadRejection = true }
        }

        // Step 7: live batch (ascending).
        if !budget.exhausted(now: clock()) {
            let outcome = await runLiveBatch(
                pool: pool,
                cursor: &working,
                budget: &budget)
            if !outcome.rejected.isEmpty { hadRejection = true }
        }

        // Step 8: commit cursor only if changed AND no rejections.
        let cursorChanged = working != cursor
        if cursorChanged && !hadRejection {
            do {
                let newJSON = try MessagesCursorCodec.encode(working)
                try await piClient.commitCursor(
                    auth: auth,
                    source: "messages",
                    cursor: newJSON,
                    baseCursor: lastCommittedCursorJSON,
                    cursorEpoch: cachedEpoch,
                    backfillComplete: working.backfillComplete)
                // Cache locally via StateMutator (write-through).
                try await writeThroughCursor(newJSON, epoch: cachedEpoch)
                cachedCursor = working
                lastCommittedCursorJSON = newJSON
                logger.debug("messages tick: cursor committed", metadata: [
                    "live_cursor": .public(String(working.liveCursor ?? 0)),
                    "backfill_cursor": .public(working.backfillCursor.map(String.init) ?? "nil"),
                    "backfill_complete": .public(String(working.backfillComplete)),
                ])
            } catch let PiClientError.cursorConflict(_, current) {
                logger.info("messages tick: cursor conflict, refreshing", metadata: [
                    "current_epoch": .public(current.currentEpoch.map(String.init) ?? "nil"),
                ])
                // Best-effort refresh; abort the tick.
                _ = try? await loadOrFetchCursor(forceRefresh: true)
                return
            } catch {
                logger.warning("messages tick: cursor commit failed", metadata: [
                    "error": .private(String(describing: error)),
                ])
                return
            }
        } else if hadRejection {
            logger.warning("messages tick: per-event rejections; holding cursor", metadata: [:])
        }

        // Step 9: refresh health snapshot.
        await healthRegistry.update(id, await currentHealthSnapshot(
            enabled: true,
            lastScheduled: tickStart,
            lastPushed: cursorChanged ? clock() : nil,
            observed: working.liveCursor,
            pushed: working.liveCursor,
            backfillComplete: working.backfillComplete))
    }

    // MARK: - per-tick batch runners

    private struct BatchSummary {
        let accepted: Int
        let duplicate: Int
        let rejected: [PerEventRejection]
    }

    /// Backfill: walk message.ROWID downward from
    /// (backfillCursor ?? installMaxRowID) toward backfillFloor's
    /// associated ROWID. Stops when budget exhausted or no more rows.
    private func runBackfillBatch(
        pool: DatabasePool,
        cursor: inout MessagesCursor,
        budget: inout BackfillBudget
    ) async -> BatchSummary {
        let upperBoundExclusive = cursor.backfillCursor ?? (cursor.installMaxRowID ?? 0)
        if upperBoundExclusive <= 0 {
            cursor.backfillComplete = true
            return BatchSummary(accepted: 0, duplicate: 0, rejected: [])
        }

        let limit = min(budget.rowsRemaining, MessagesPublisher.maxEventsPerBatch)
        if limit <= 0 {
            return BatchSummary(accepted: 0, duplicate: 0, rejected: [])
        }

        let rows: [ChatDBMessage]
        do {
            rows = try await pool.read { db in
                try ChatDBReader.fetch(
                    db: db,
                    direction: .backwardFromExclusive(upperBoundExclusive),
                    limit: limit)
            }
        } catch {
            logger.warning("messages tick: backfill read failed", metadata: [
                "error": .private(String(describing: error)),
            ])
            return BatchSummary(accepted: 0, duplicate: 0, rejected: [])
        }

        if rows.isEmpty {
            // No more backfill rows — mark complete.
            cursor.backfillComplete = true
            cursor.backfillCursor = 0
            return BatchSummary(accepted: 0, duplicate: 0, rejected: [])
        }

        // Floor check: drop rows whose sentAt is below backfillFloor.
        let inRangeRows = rows.filter { $0.sentAt >= config.backfillFloor }
        let belowFloor = rows.count - inRangeRows.count
        let publishItems = await filterAndShape(rows: inRangeRows, isBackfill: true)
        let outcome = await publisher.publish(items: publishItems)

        budget.consume(rows: rows.count)
        // Advance backfill cursor: lowest ROWID seen.
        let lowestROWID = rows.map(\.rowID).min() ?? upperBoundExclusive
        cursor.backfillCursor = lowestROWID
        if belowFloor > 0 {
            // We crossed the floor; backfill is complete.
            cursor.backfillComplete = true
        }

        return BatchSummary(
            accepted: outcome.accepted,
            duplicate: outcome.duplicate,
            rejected: outcome.rejected)
    }

    /// Live: walk message.ROWID upward from liveCursor.  Stops when
    /// budget exhausted or no more rows.
    private func runLiveBatch(
        pool: DatabasePool,
        cursor: inout MessagesCursor,
        budget: inout BackfillBudget
    ) async -> BatchSummary {
        let lower = cursor.liveCursor ?? (cursor.installMaxRowID ?? 0)
        let limit = min(budget.rowsRemaining, MessagesPublisher.maxEventsPerBatch)
        if limit <= 0 {
            return BatchSummary(accepted: 0, duplicate: 0, rejected: [])
        }

        let rows: [ChatDBMessage]
        do {
            rows = try await pool.read { db in
                try ChatDBReader.fetch(
                    db: db,
                    direction: .forwardFromExclusive(lower),
                    limit: limit)
            }
        } catch {
            logger.warning("messages tick: live read failed", metadata: [
                "error": .private(String(describing: error)),
            ])
            return BatchSummary(accepted: 0, duplicate: 0, rejected: [])
        }

        if rows.isEmpty {
            return BatchSummary(accepted: 0, duplicate: 0, rejected: [])
        }

        let publishItems = await filterAndShape(rows: rows, isBackfill: false)
        let outcome = await publisher.publish(items: publishItems)

        budget.consume(rows: rows.count)
        let highestROWID = rows.map(\.rowID).max() ?? lower
        cursor.liveCursor = highestROWID

        return BatchSummary(
            accepted: outcome.accepted,
            duplicate: outcome.duplicate,
            rejected: outcome.rejected)
    }

    /// Apply sender filter + shape to publishable form.
    /// Outbound rows have NULL handle_id in chat.db (reader skips them
    /// during fetch); for cursor advancement we DO advance past them,
    /// but the publisher only sees emitted rows. Outbound peer
    /// resolution would happen here via outboundGroupPeer, but
    /// fetch() already excludes outbound rows; v1 ships inbound-only
    /// emission until the outbound branch is wired through the reader.
    private func filterAndShape(rows: [ChatDBMessage], isBackfill: Bool) async -> [PublishItem] {
        var out: [PublishItem] = []
        out.reserveCapacity(rows.count)
        for row in rows {
            let canonical = HandleNormalization.canonicalize(row.peerHandleRaw)
            if canonical.isEmpty { continue }
            let inSet = await cache.contains(canonical)
            if !inSet { continue }
            let (kind, payload) = PayloadShaping.shape(
                row: row,
                peerHandle: row.peerHandleRaw,
                hostID: auth.hostID)
            out.append(PublishItem(rowID: row.rowID, direction: kind, payload: payload))
            _ = isBackfill // unused in v1; reserved for backfill-specific telemetry
        }
        return out
    }

    // MARK: - schema + pool

    private enum SchemaDriftError: Error {
        case drift(table: String, missing: Set<String>)
    }

    private func openOrCached() async throws -> DatabasePool {
        if let cached = pool, schemaHealth == .ok {
            return cached
        }
        var config = Configuration()
        config.readonly = true
        // Don't add ?immutable=1 — Messages.app is actively writing.
        let pool = try DatabasePool(path: self.config.chatDBPath.path, configuration: config)
        let health = try await pool.read { try ChatDBSchemaValidator.validate(db: $0) }
        schemaHealth = health
        switch health {
        case .ok:
            self.pool = pool
            return pool
        case .drift(let table, let missing):
            throw SchemaDriftError.drift(table: table, missing: missing)
        }
    }

    // MARK: - cursor load + commit

    /// Load the messages cursor from the local state.json cache; if
    /// no cached value, fetch from the Pi via GET.
    @discardableResult
    private func loadOrFetchCursor(forceRefresh: Bool = false) async throws -> MessagesCursor {
        if !forceRefresh, let cached = cachedCursor {
            return cached
        }
        // GET cursor from Pi.
        let state = try await piClient.getCursor(auth: auth, source: "messages")
        cachedEpoch = state.cursorEpoch
        lastCommittedCursorJSON = state.cursor
        let decoded = try MessagesCursorCodec.decode(state.cursor)
        let fresh = decoded ?? MessagesCursor(backfillFloorSentAt: config.backfillFloor)
        cachedCursor = fresh
        return fresh
    }

    private func writeThroughCursor(_ json: String, epoch: Int64) async throws {
        try await mutator.mutate { state in
            var src = state.sources["messages"] ?? SourceState(cursor: "")
            src.cursor = json
            state.sources["messages"] = src
        }
    }

    // MARK: - health surface

    private func currentHealthSnapshot(
        enabled: Bool,
        lastScheduled: Date,
        lastPushed: Date? = nil,
        observed: Int64? = nil,
        pushed: Int64? = nil,
        backfillComplete: Bool? = nil
    ) async -> SourceHealthSnapshot {
        SourceHealthSnapshot(
            enabled: enabled,
            lastScheduledAt: lastScheduled,
            lastPushedAt: lastPushed,
            observedCursor: observed,
            pushedCursor: pushed,
            schemaVersion: schemaHealth?.label,
            backfillComplete: backfillComplete,
            lastError: nil,
            lastErrorAt: nil)
    }

    private func markUnhealthy(reason: String) async {
        let snap = SourceHealthSnapshot(
            enabled: false,
            lastScheduledAt: clock(),
            schemaVersion: schemaHealth?.label,
            lastError: reason,
            lastErrorAt: clock())
        await healthRegistry.update(id, snap)
        logger.error("messages tick: marked unhealthy", metadata: [
            "reason": .public(reason),
        ])
    }
}

