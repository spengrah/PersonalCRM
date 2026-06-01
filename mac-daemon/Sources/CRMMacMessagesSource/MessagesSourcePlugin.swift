// MessagesSourcePlugin — actor that orchestrates one messages tick.
//
// Per-tick flow:
//   1. Check schema health: if drifted, set source_health unhealthy
//      -> return.
//   2. Open chat.db DatabasePool (cached on first successful open).
//   3. If no cached cursor: piClient.getMessagesCursor(...) -> decode
//      into MessagesCursor; cache locally via StateMutator.
//   4. Phase A — durable scan enqueue: drain the known-identifiers
//      cache's newly-added bucket (non-destructive), coverage-dedup +
//      cap them into pendingScans with a 30-day window, and commit the
//      cursor BEFORE executing any scan. On success confirm the drain;
//      on failure return it so the next tick re-enqueues.
//   5. Phase B — execute pending scans (gated on cache.hasFetched, NOT
//      isPopulated): membership-check each entry, walk one budget-
//      limited page via MessagesScanReader (resumable via the entry's
//      progress), publish, advance progress or dequeue on exhaustion,
//      re-commit. Runs BEFORE the row-emitting batches.
//   6. If the known-identifiers cache is not populated, skip the
//      row-emitting batches (the scan phases already ran).
//   7. If installMaxRowID == nil: capture MAX(ROWID) into the in-memory
//      MessagesCursor (uncommitted; committed in step 10).
//   8. If backfillComplete == false: run a backfill batch (budget-
//      bound) -> publish -> advance backfillCursor downward.
//   9. Run live batch (remaining budget): read message.ROWID > liveCursor
//      -> publish -> advance liveCursor upward.
//  10. If the in-memory cursor changed AND every batch had
//      rejected == 0: commitMessagesCursor; on success write through
//      to local state.json via StateMutator. On 409: refresh + abort.
//      On transport error: log + leave local cache untouched -> return.
//  11. Update source_health snapshot via the shared registry.
//
// The plugin is the single owner of the GRDB DatabasePool, the
// KnownIdentifiersCache, and the messages source's slot in
// SourceState. All writes to state.json funnel through StateMutator.
// The plugin NEVER writes the persisted known-identifiers baseline —
// that is the heartbeat refresher's job (single-writer model).
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
    /// Max rows per tick (default 500).
    public let maxRowsPerTick: Int
    /// Max wall-clock seconds per tick (default 5.0).
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

public actor MessagesSourcePlugin: DataSourcePlugin {
    public nonisolated let id: SourceID = .messages
    public nonisolated let tickInterval: TimeInterval

    private let config: MessagesSourceConfig
    private let piClient: PiClient
    private let auth: PiAuth
    // nonisolated: read by the DataSourcePlugin extension's tick() from
    // a nonisolated context. Sound because all three are immutable lets
    // holding Sendable values (same pattern as `id`/`tickInterval`).
    public nonisolated let mutator: StateMutator
    private let publisher: MessagesPublisher
    private let cache: KnownIdentifiersCache
    public nonisolated let logger: LoggerProtocol
    public nonisolated let clock: @Sendable () -> Date
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
    /// Sticky `last_pushed_at` so it persists across snapshot writes
    /// (each tick's start-of-tick snapshot would otherwise reset it to
    /// nil and the Pi/UI would only ever see the last_pushed value
    /// emitted in the same tick it was set).
    private var stickyLastPushedAt: Date?

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

    public func performTick() async throws {
        // The DataSourcePlugin extension already bumped state.json
        // `lastScheduledAt` via `clock()` before entering here. This
        // second `clock()` read feeds the in-memory heartbeat-payload
        // registry snapshot + budget math; with a fixed test clock the
        // two reads are equal, and in production the sub-ms drift is
        // irrelevant for a coarse liveness timestamp.
        let tickStart = clock()
        await healthRegistry.update(id, await currentHealthSnapshot(
            enabled: true,
            lastScheduled: tickStart,
            lastPushed: stickyLastPushedAt))

        // Open pool + validate schema (cached on success).
        let pool: DatabasePool
        do {
            pool = try await openOrCached()
        } catch let SchemaDriftError.drift(table, missing) {
            await markUnhealthy(reason: "schema_drift:\(table).\(missing.sorted().first ?? "?")")
            return
        } catch is FDAError {
            await markUnhealthy(reason: "fda_required")
            return
        } catch {
            await markUnhealthy(reason: "open_failed: \(String(describing: error))")
            return
        }

        // Load cursor (from local cache or Pi).
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

        var working = cursor

        // Phase A — durable scan enqueue (commit-first).
        // Drain the cache's newly-added bucket NON-DESTRUCTIVELY and
        // merge it into working.pendingScans (coverage-dedup + cap),
        // then commit the cursor with the enlarged queue BEFORE any scan
        // executes. On commit success the drained identifiers are
        // confirmed (durably enqueued); on failure they are returned to
        // the cache so the next tick re-enqueues. This runs regardless
        // of isPopulated — a newly-known identifier must be durably
        // queued even on a tick where the row-emitting batches skip.
        if await runScanEnqueue(working: &working) == .aborted {
            return
        }

        // Phase B — execute pending scans (resumable), gated on
        // hasFetched (NOT isPopulated). An empty-but-fetched CRM still
        // adjudicates pending scans (drops an operator scan for a
        // now-removed contact); a not-yet-fetched cache defers them. A
        // scan-commit conflict aborts the rest of the tick: `working` is
        // now stale relative to the refreshed Pi cursor, so the
        // row-emitting batches + final commit must NOT run against it.
        if await cache.hasFetched {
            if await runPendingScans(pool: pool, working: &working) == .aborted {
                return
            }
        }

        // Sender filter ready?  If the known-identifiers cache hasn't
        // been populated yet, skip the row-emitting batches (heartbeat
        // will fill it). The scan phases above already ran.
        let populated = await cache.isPopulated
        if !populated {
            logger.debug("messages tick: known-identifiers cache empty; skipping batches", metadata: [:])
            return
        }

        // Capture install-time MAX(ROWID) if needed.
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
        var hadUnconfirmed = false

        // Backfill batch (descending).
        if !working.backfillComplete, budget.rowsRemaining > 0 {
            let outcome = await runBackfillBatch(
                pool: pool,
                cursor: &working,
                budget: &budget)
            if !outcome.rejected.isEmpty { hadRejection = true }
            if outcome.hadUnconfirmedItems { hadUnconfirmed = true }
        }

        // Live batch (ascending).
        if !budget.exhausted(now: clock()) {
            let outcome = await runLiveBatch(
                pool: pool,
                cursor: &working,
                budget: &budget)
            if !outcome.rejected.isEmpty { hadRejection = true }
            if outcome.hadUnconfirmedItems { hadUnconfirmed = true }
        }

        // Commit cursor only if changed, no rejections, AND every
        // item we tried to publish was confirmed. A transport
        // failure mid-tick must NOT commit a cursor that skips the
        // failed rows.
        let cursorChanged = working != cursor
        if cursorChanged && !hadRejection && !hadUnconfirmed {
            switch await commitWorking(working) {
            case .committed:
                logger.debug("messages tick: cursor committed", metadata: [
                    "live_cursor": .public(String(working.liveCursor ?? 0)),
                    "backfill_cursor": .public(working.backfillCursor.map(String.init) ?? "nil"),
                    "backfill_complete": .public(String(working.backfillComplete)),
                ])
            case .conflict:
                _ = try? await loadOrFetchCursor(forceRefresh: true)
                return
            case .failed:
                return
            }
        } else if hadRejection {
            logger.warning("messages tick: per-event rejections; holding cursor", metadata: [:])
        } else if hadUnconfirmed {
            logger.warning("messages tick: publish unconfirmed; holding cursor", metadata: [:])
        }

        // Refresh health snapshot. `last_pushed_at` is sticky across
        // ticks so the UI reflects the most recent successful push,
        // not just ticks that emit events.
        if cursorChanged {
            stickyLastPushedAt = clock()
        }
        await healthRegistry.update(id, await currentHealthSnapshot(
            enabled: true,
            lastScheduled: tickStart,
            lastPushed: stickyLastPushedAt,
            observed: working.liveCursor,
            pushed: working.liveCursor,
            backfillComplete: working.backfillComplete))
    }

    // MARK: - cursor commit helper

    /// Result of a CAS cursor commit.
    private enum CommitResult {
        case committed
        case conflict
        case failed
    }

    /// CAS-commit `working` to the Pi (base = lastCommittedCursorJSON),
    /// write through to local state.json, and update the in-memory
    /// cache (`cachedCursor` / `lastCommittedCursorJSON`). Used by the
    /// scan phases AND the final backfill/live commit so the CAS base
    /// and write-through stay in lockstep across multiple commits per
    /// tick.
    private func commitWorking(_ working: MessagesCursor) async -> CommitResult {
        let newJSON: String
        do {
            newJSON = try MessagesCursorCodec.encode(working)
        } catch {
            logger.warning("messages tick: cursor encode failed", metadata: [
                "error": .private(String(describing: error)),
            ])
            return .failed
        }
        do {
            try await piClient.commitCursor(
                auth: auth,
                source: "messages",
                cursor: newJSON,
                baseCursor: lastCommittedCursorJSON,
                cursorEpoch: cachedEpoch,
                backfillComplete: working.backfillComplete)
            try await writeThroughCursor(
                newJSON,
                epoch: cachedEpoch,
                backfillComplete: working.backfillComplete,
                lastPushedAt: clock())
            cachedCursor = working
            lastCommittedCursorJSON = newJSON
            return .committed
        } catch let PiClientError.cursorConflict(_, current) {
            logger.info("messages tick: cursor conflict, refreshing", metadata: [
                "current_epoch": .public(current.currentEpoch.map(String.init) ?? "nil"),
            ])
            return .conflict
        } catch {
            logger.warning("messages tick: cursor commit failed", metadata: [
                "error": .private(String(describing: error)),
            ])
            return .failed
        }
    }

    // MARK: - Phase A: durable scan enqueue

    private enum ScanEnqueueResult {
        case ok
        case aborted
    }

    /// Drain the cache's newly-added bucket (non-destructive), merge it
    /// into `working.pendingScans` (coverage-dedup + cap) with a 30-day
    /// window, and commit the cursor BEFORE any scan executes. On commit
    /// success the drained identifiers are confirmed in the cache; on
    /// failure they are returned so the next tick re-enqueues. Returns
    /// `.aborted` when the tick must stop (commit failure/conflict).
    private func runScanEnqueue(working: inout MessagesCursor) async -> ScanEnqueueResult {
        let drained = await cache.drainNewlyAdded(for: id)
        if drained.isEmpty {
            return .ok
        }
        logger.info("messages tick: newly-known contacts detected", metadata: [
            "count": .public(String(drained.count)),
        ])
        // Sort canonical-ascending so drop-oldest + queue order are
        // deterministic across runs (the bucket is an unordered Set).
        let since = clock().addingTimeInterval(-Self.scanWindowSeconds)
        let floor = config.backfillFloor
        let scanSince = max(since, floor)
        for handle in drained.sorted() {
            mergePendingScan(
                into: &working.pendingScans,
                handle: handle,
                since: scanSince)
        }
        switch await commitWorking(working) {
        case .committed:
            // Durably enqueued — the drained identifiers leave the
            // cache's owed set, eligible for the next refresher persist.
            await cache.confirmDrained(for: id)
            return .ok
        case .conflict:
            // Not durable — return the drained identifiers so the next
            // tick re-enqueues. The tick aborts (working is discarded).
            await cache.returnInFlight(for: id)
            _ = try? await loadOrFetchCursor(forceRefresh: true)
            return .aborted
        case .failed:
            await cache.returnInFlight(for: id)
            return .aborted
        }
    }

    /// Merge identifier `handle` into a pendingScans list with
    /// COVERAGE-DEDUP: at most one entry per handle, keeping the WIDER
    /// window. A wider window (earlier `since`) resets progress to nil
    /// so the larger range is re-walked; an equal-or-narrower window
    /// leaves the existing entry (and its progress) untouched.
    private func mergePendingScan(
        into scans: inout [PendingScan],
        handle: String,
        since: Date
    ) {
        if let idx = scans.firstIndex(where: { $0.normalizedHandle == handle }) {
            let existing = scans[idx]
            if since < existing.since {
                // Wider window: widen + reset progress to re-walk.
                scans[idx] = PendingScan(
                    normalizedHandle: handle,
                    since: since,
                    progressBelowRowID: nil)
            }
            // Equal-or-narrower: keep the existing entry + its progress.
            return
        }
        var next = scans
        next.append(PendingScan(normalizedHandle: handle, since: since, progressBelowRowID: nil))
        // Bounded queue: drop oldest on overflow.
        if next.count > MessagesCursor.pendingScansCap {
            next.removeFirst(next.count - MessagesCursor.pendingScansCap)
            logger.warning("messages tick: pending-scan cap reached; dropped oldest", metadata: [:])
        }
        scans = next
    }

    // MARK: - Phase B: execute pending scans

    /// Execute the pending scans in `working.pendingScans` (resumable,
    /// budget-bound). For each entry: membership-check against the known
    /// set (drop unknown handles), walk one budget-limited page
    /// descending from the entry's progress, publish, then either
    /// advance the entry's progress (still queued) or dequeue it on
    /// confirmed exhaustion. Re-commits the cursor after each entry's
    /// progress/dequeue so a crash recovers from the persisted cursor.
    ///
    /// Returns `.aborted` on a scan-commit conflict/failure: `working`
    /// is then stale relative to the refreshed Pi cursor, so the caller
    /// must stop the tick rather than re-commit it.
    private func runPendingScans(pool: DatabasePool, working: inout MessagesCursor) async -> ScanEnqueueResult {
        guard !working.pendingScans.isEmpty else { return .ok }
        // Iterate by handle snapshot; mutate working.pendingScans in
        // place as each entry advances or completes.
        let handles = working.pendingScans.map(\.normalizedHandle)
        var scanBudget = config.maxRowsPerTick
        for handle in handles {
            if scanBudget <= 0 { break }
            guard let entry = working.pendingScans.first(where: { $0.normalizedHandle == handle }) else {
                continue
            }
            // Membership check: drop scans for handles not in the current
            // known set — operator typo / removed contact.
            let known = await cache.contains(handle)
            if !known {
                working.pendingScans.removeAll { $0.normalizedHandle == handle }
                logger.warning("messages scan: handle not in known set; dropping", metadata: [
                    "handle": .private(handle),
                ])
                if case .aborted = await commitAfterScanMutation(working) { return .aborted }
                continue
            }

            let limit = min(scanBudget, MessagesPublisher.maxEventsPerBatch)
            // Never scan below the backfill floor at EXECUTION time —
            // defense-in-depth against any already-committed entry whose
            // `since` predates the floor (e.g. queued by an older build).
            // Rows below the floor are never emitted.
            let scanSince = max(entry.since, config.backfillFloor)
            let page: MessagesScanPage
            do {
                page = try await pool.read { db in
                    try MessagesScanReader.scanPage(
                        db: db,
                        canonicalHandle: handle,
                        since: scanSince,
                        progressBelowRowID: entry.progressBelowRowID,
                        limit: limit)
                }
            } catch let dbError as DatabaseError where isFDAError(dbError) {
                await markUnhealthy(reason: "fda_required")
                return .aborted
            } catch {
                logger.warning("messages scan: read failed; holding entry", metadata: [
                    "error": .private(String(describing: error)),
                ])
                continue
            }

            scanBudget -= page.rows.count

            let publishItems = await filterAndShape(rows: page.rows, isBackfill: true)
            let outcome = await publisher.publish(items: publishItems)

            let confirmed: Bool
            if publishItems.isEmpty {
                confirmed = true
            } else {
                confirmed = outcome.rejected.isEmpty && outcome.unconfirmed == 0
            }
            if !confirmed {
                // Publish failure: leave progress unchanged so the next
                // tick retries from the same coordinate. Event-log dedup
                // absorbs any re-emit.
                logger.warning("messages scan: publish unconfirmed; holding progress", metadata: [
                    "handle": .private(handle),
                ])
                continue
            }

            if page.exhausted {
                // Final short page confirmed-published → dequeue.
                working.pendingScans.removeAll { $0.normalizedHandle == handle }
            } else if let lowest = page.lowestRowID {
                // Advance progress; entry stays queued for the next tick.
                if let idx = working.pendingScans.firstIndex(where: { $0.normalizedHandle == handle }) {
                    working.pendingScans[idx] = PendingScan(
                        normalizedHandle: handle,
                        since: entry.since,
                        progressBelowRowID: lowest)
                }
            }
            if case .aborted = await commitAfterScanMutation(working) { return .aborted }
        }
        return .ok
    }

    /// Commit `working` after a scan entry's progress/dequeue mutation.
    /// A conflict refreshes + aborts the remaining scans this tick.
    private func commitAfterScanMutation(_ working: MessagesCursor) async -> ScanEnqueueResult {
        switch await commitWorking(working) {
        case .committed:
            return .ok
        case .conflict:
            _ = try? await loadOrFetchCursor(forceRefresh: true)
            return .aborted
        case .failed:
            return .aborted
        }
    }

    /// Automatic newly-known scans always use a 30-day window.
    private static let scanWindowSeconds: TimeInterval = 30 * 24 * 60 * 60

    // MARK: - per-tick batch runners

    private struct BatchSummary {
        let accepted: Int
        let duplicate: Int
        let rejected: [PerEventRejection]
        /// True if there were publish items in the batch but the
        /// publish call did NOT confirm them (transport failure or
        /// per-event rejections). The caller must NOT advance any
        /// derived state (e.g. backfillComplete flag) when this is
        /// true.
        let hadUnconfirmedItems: Bool
    }

    /// Backfill: walk message.ROWID downward from
    /// (backfillCursor ?? installMaxRowID) toward backfillFloor's
    /// associated ROWID. Stops when budget exhausted or no more rows.
    ///
    /// Cursor advance is conditional: only advance backfillCursor past
    /// publishable rows when the publish batch came back with
    /// `rejected.isEmpty` AND advanceTo != nil. For pages where every
    /// row got filtered out (empty handle, corrupt date), we still
    /// advance past the scanned bounds — otherwise we'd loop forever
    /// re-reading the same page.
    private func runBackfillBatch(
        pool: DatabasePool,
        cursor: inout MessagesCursor,
        budget: inout BackfillBudget
    ) async -> BatchSummary {
        let upperBoundExclusive = cursor.backfillCursor ?? (cursor.installMaxRowID ?? 0)
        if upperBoundExclusive <= 0 {
            cursor.backfillComplete = true
            return BatchSummary(accepted: 0, duplicate: 0,
                                  rejected: [], hadUnconfirmedItems: false)
        }

        let limit = min(budget.rowsRemaining, MessagesPublisher.maxEventsPerBatch)
        if limit <= 0 {
            return BatchSummary(accepted: 0, duplicate: 0,
                                  rejected: [], hadUnconfirmedItems: false)
        }

        let page: ChatDBReadPage
        do {
            page = try await pool.read { db in
                try ChatDBReader.fetchPage(
                    db: db,
                    direction: .backwardFromExclusive(upperBoundExclusive),
                    limit: limit)
            }
        } catch let dbError as DatabaseError where isFDAError(dbError) {
            await markUnhealthy(reason: "fda_required")
            return BatchSummary(accepted: 0, duplicate: 0,
                                  rejected: [], hadUnconfirmedItems: false)
        } catch {
            logger.warning("messages tick: backfill read failed", metadata: [
                "error": .private(String(describing: error)),
            ])
            return BatchSummary(accepted: 0, duplicate: 0,
                                  rejected: [], hadUnconfirmedItems: false)
        }

        // No SQL rows at all -> we've walked the whole iterator.
        if page.scannedROWIDBounds == nil {
            cursor.backfillComplete = true
            cursor.backfillCursor = 0
            return BatchSummary(accepted: 0, duplicate: 0,
                                  rejected: [], hadUnconfirmedItems: false)
        }

        // Floor check: drop rows whose sentAt is below backfillFloor.
        let inRangeRows = page.rows.filter { $0.sentAt >= config.backfillFloor }
        let belowFloor = page.rows.count - inRangeRows.count
        let publishItems = await filterAndShape(rows: inRangeRows, isBackfill: true)
        let outcome = await publisher.publish(items: publishItems)

        budget.consume(rows: page.rows.count)

        // Cursor advance rules:
        //   - publishItems empty (everything filtered out): advance
        //     past scanned MIN so the iterator doesn't stall.
        //   - publishItems non-empty + clean batch: advance past the
        //     scanned MIN. (Descending walk; MIN is the new exclusive
        //     upper bound.)
        //   - publishItems non-empty + ANY transport / per-event
        //     failure: do NOT advance, AND do NOT flip
        //     backfillComplete — those rows must be retried.
        let confirmedAllItems: Bool
        if publishItems.isEmpty {
            confirmedAllItems = true
        } else {
            confirmedAllItems = outcome.advanceTo != nil
                && outcome.rejected.isEmpty
                && outcome.unconfirmed == 0
        }
        if let bounds = page.scannedROWIDBounds, confirmedAllItems {
            cursor.backfillCursor = bounds.min
        }
        if confirmedAllItems && (belowFloor > 0 || page.exhausted) {
            // We either crossed the floor or scanned every remaining
            // row AND every item we tried to publish was confirmed.
            cursor.backfillComplete = true
        }

        return BatchSummary(
            accepted: outcome.accepted,
            duplicate: outcome.duplicate,
            rejected: outcome.rejected,
            hadUnconfirmedItems: !confirmedAllItems)
    }

    /// Live: walk message.ROWID upward from liveCursor.  Stops when
    /// budget exhausted or no more rows.
    ///
    /// Cursor advance is conditional: only advance liveCursor past
    /// publishable rows when the publish batch came back with
    /// `rejected.isEmpty` AND advanceTo != nil. Pages of all-skipped
    /// rows still advance to the scanned MAX so the iterator doesn't
    /// stall.
    private func runLiveBatch(
        pool: DatabasePool,
        cursor: inout MessagesCursor,
        budget: inout BackfillBudget
    ) async -> BatchSummary {
        let lower = cursor.liveCursor ?? (cursor.installMaxRowID ?? 0)
        let limit = min(budget.rowsRemaining, MessagesPublisher.maxEventsPerBatch)
        if limit <= 0 {
            return BatchSummary(accepted: 0, duplicate: 0,
                                  rejected: [], hadUnconfirmedItems: false)
        }

        let page: ChatDBReadPage
        do {
            page = try await pool.read { db in
                try ChatDBReader.fetchPage(
                    db: db,
                    direction: .forwardFromExclusive(lower),
                    limit: limit)
            }
        } catch let dbError as DatabaseError where isFDAError(dbError) {
            await markUnhealthy(reason: "fda_required")
            return BatchSummary(accepted: 0, duplicate: 0,
                                  rejected: [], hadUnconfirmedItems: false)
        } catch {
            logger.warning("messages tick: live read failed", metadata: [
                "error": .private(String(describing: error)),
            ])
            return BatchSummary(accepted: 0, duplicate: 0,
                                  rejected: [], hadUnconfirmedItems: false)
        }

        if page.scannedROWIDBounds == nil {
            // No new rows since liveCursor.
            return BatchSummary(accepted: 0, duplicate: 0,
                                  rejected: [], hadUnconfirmedItems: false)
        }

        let publishItems = await filterAndShape(rows: page.rows, isBackfill: false)
        let outcome = await publisher.publish(items: publishItems)

        budget.consume(rows: page.rows.count)

        let confirmedAllItems: Bool
        if publishItems.isEmpty {
            confirmedAllItems = true
        } else {
            confirmedAllItems = outcome.advanceTo != nil
                && outcome.rejected.isEmpty
                && outcome.unconfirmed == 0
        }
        if let bounds = page.scannedROWIDBounds, confirmedAllItems {
            cursor.liveCursor = bounds.max
        }

        return BatchSummary(
            accepted: outcome.accepted,
            duplicate: outcome.duplicate,
            rejected: outcome.rejected,
            hadUnconfirmedItems: !confirmedAllItems)
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
        // URI shape + WAL rationale (incl. why we must NOT use
        // `immutable=1`) owned by `SQLiteSnapshotReader`.
        // `Configuration.readonly` is defense-in-depth; the URI's
        // `mode=ro` is the primary read-only/WAL-aware guard.
        let pool: DatabasePool
        do {
            pool = try DatabasePool(
                path: SQLiteSnapshotReader.readOnlyURI(for: self.config.chatDBPath),
                configuration: config)
        } catch let dbError as DatabaseError {
            // FDA / sandbox denial: SQLITE_AUTH (23), SQLITE_PERM (3),
            // or SQLITE_CANTOPEN (14). Distinguish from a transient
            // SQLITE_BUSY so the operator gets actionable feedback.
            if isFDAError(dbError) {
                throw FDAError()
            }
            throw dbError
        }
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

    private func isFDAError(_ err: DatabaseError) -> Bool {
        // GRDB's DatabaseError wraps SQLite result codes; FDA denial
        // surfaces as SQLITE_AUTH (23), SQLITE_PERM (3), or
        // SQLITE_CANTOPEN (14).
        let code = err.resultCode.rawValue
        return code == 3 || code == 14 || code == 23
    }

    private struct FDAError: Error {}

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

    private func writeThroughCursor(
        _ json: String,
        epoch: Int64,
        backfillComplete: Bool,
        lastPushedAt: Date
    ) async throws {
        try await mutator.mutate { state in
            var src = state.sources["messages"] ?? SourceState(cursor: "")
            src.cursor = json
            src.cursorEpoch = epoch
            src.backfillComplete = backfillComplete
            src.lastPushedAt = lastPushedAt
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
            observedCursor: observed.map(SourceHealthCursor.int),
            pushedCursor: pushed.map(SourceHealthCursor.int),
            schemaVersion: schemaHealth?.label,
            backfillComplete: backfillComplete,
            lastError: nil,
            lastErrorAt: nil)
    }

    private func markUnhealthy(reason: String) async {
        let snap = SourceHealthSnapshot(
            enabled: false,
            lastScheduledAt: clock(),
            lastPushedAt: stickyLastPushedAt,
            schemaVersion: schemaHealth?.label,
            lastError: reason,
            lastErrorAt: clock())
        await healthRegistry.update(id, snap)
        logger.error("messages tick: marked unhealthy", metadata: [
            "reason": .public(reason),
        ])
    }
}

