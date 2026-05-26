// PhoneCallsSourcePlugin — actor that orchestrates one phone_calls tick.
//
// Per-tick flow:
//   0. Pi-protocol-version feature-gate (R2-P2-H): consult the
//      HeartbeatStateProvider; if Pi protocol_version < 2 (or nil),
//      short-circuit the tick. Heartbeat continues normally — this
//      gate only inhibits emission of `call.*` events that an older
//      Pi can't accept.
//   1. Check schema health: if drifted, set source_health unhealthy
//      and return.
//   2. Open CallHistoryDB DatabasePool with `?mode=ro&immutable=1`
//      (cached on first successful open).
//   3. If no cached cursor: piClient.getCursor("phone_calls") -> decode
//      into PhoneCallsCursor; cache locally via StateMutator.
//   4. If installMaxZDate == nil: capture MAX(ZDATE, Z_PK) into the
//      in-memory cursor (uncommitted; committed in step 7).
//   5. KnownIdentifiersCache populated? If not, skip (heartbeat will
//      fill it). v1.5 defers identifier-scoped scans
//      (D-DEVIATION-1); we observe + log the newly-added queue but
//      don't consume it.
//   6. Backfill batch (descending) + live batch (ascending), each
//      bounded by the shared PhoneCallsBudget.
//   7. If the in-memory cursor changed AND every batch was confirmed
//      with zero rejections: commitCursor; on success write-through
//      to local state.json. On 409: refresh + abort. On transport
//      error: log + leave local cache untouched -> return.
//   8. Update source_health snapshot via the shared registry.
//
// The plugin is the single owner of the GRDB DatabasePool, but shares
// the KnownIdentifiersCache with CRMMacMessagesSource (R7 + cache
// moved to CRMMacCore in this PR's earlier commit). All writes to
// state.json funnel through StateMutator.
import Foundation
import GRDB
import CRMMacCore
import CRMMacPiClient

/// Inputs the plugin needs from the daemon's composition root.
public struct PhoneCallsSourceConfig: Sendable {
    /// Path to CallHistoryDB.storedata. Production:
    /// ~/Library/Application Support/CallHistoryDB/CallHistory.storedata.
    public let callHistoryDBPath: URL
    /// Backfill floor — events with startedAt before this date are not
    /// emitted.
    public let backfillFloor: Date
    /// Max rows per tick (default 500).
    public let maxRowsPerTick: Int
    /// Max wall-clock seconds per tick (default 5.0).
    public let maxDurationPerTick: TimeInterval

    public init(
        callHistoryDBPath: URL,
        backfillFloor: Date,
        maxRowsPerTick: Int = 500,
        maxDurationPerTick: TimeInterval = 5.0
    ) {
        self.callHistoryDBPath = callHistoryDBPath
        self.backfillFloor = backfillFloor
        self.maxRowsPerTick = maxRowsPerTick
        self.maxDurationPerTick = maxDurationPerTick
    }
}

/// Closure that canonicalizes a raw peer handle (phone or email) to its
/// E.164 / lowercased-email form. Injected from CRMMacMessagesSource's
/// HandleNormalization so the messages + phone_calls sources share the
/// same normalizer without taking a cross-source dep.
public typealias HandleCanonicalizer = @Sendable (String) -> String

public actor PhoneCallsSourcePlugin: SourcePlugin {
    public nonisolated let id: SourceID = .phoneCalls
    public nonisolated let tickInterval: TimeInterval

    private let config: PhoneCallsSourceConfig
    private let piClient: PiClient
    private let auth: PiAuth
    private let mutator: StateMutator
    private let publisher: PhoneCallsPublisher
    private let cache: KnownIdentifiersCache
    private let canonicalizer: HandleCanonicalizer
    private let heartbeatStateProvider: HeartbeatStateProvider
    private let logger: LoggerProtocol
    private let clock: @Sendable () -> Date
    private let healthRegistry: SourceHealthRegistry

    /// Cached schema health. Validated on first successful open. We
    /// do NOT cache the DatabasePool itself: `?mode=ro&immutable=1`
    /// trades observability of new writes for crash-resilience, so
    /// we MUST reopen the pool every tick to see calls made since the
    /// last tick.
    private var schemaHealth: CallHistorySchemaHealth?
    /// Cached cursor + epoch loaded from local state.json or fetched
    /// from the Pi on first tick.
    private var cachedCursor: PhoneCallsCursor?
    private var cachedEpoch: Int64 = 0
    /// Last serialized cursor JSON committed to the Pi. Used as
    /// base_cursor on the next commit.
    private var lastCommittedCursorJSON: String = ""
    /// Whether we've already logged the "protocol_version below
    /// required" gate message during this state. Reset whenever the
    /// gate observes a value >= required.
    private var protocolGateLoggedBlocked: Bool = false
    /// Telemetry counter for service-unknown rows. Surfaces in
    /// heartbeat source_health via schemaVersion sub-label (best-
    /// effort; primary visibility is the structured log).
    private var serviceUnknownTotal: Int = 0

    public init(
        tickInterval: TimeInterval,
        config: PhoneCallsSourceConfig,
        piClient: PiClient,
        auth: PiAuth,
        mutator: StateMutator,
        publisher: PhoneCallsPublisher,
        cache: KnownIdentifiersCache,
        canonicalizer: @escaping HandleCanonicalizer,
        heartbeatStateProvider: HeartbeatStateProvider,
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
        self.canonicalizer = canonicalizer
        self.heartbeatStateProvider = heartbeatStateProvider
        self.healthRegistry = healthRegistry
        self.logger = logger
        self.clock = clock
    }

    public func tick() async throws {
        let tickStart = clock()
        await healthRegistry.update(id, currentHealthSnapshot(
            enabled: true, lastScheduled: tickStart))

        // STEP 0 — Pi protocol_version feature-gate.
        let piVersion = await heartbeatStateProvider.lastKnownPiProtocolVersion
        let required = CRMMacPhoneCallsSource.minPiProtocolVersion
        guard let pv = piVersion, pv >= required else {
            if !protocolGateLoggedBlocked {
                logger.info("phone_calls disabled: Pi protocol_version below required", metadata: [
                    "required": .public(String(required)),
                    "current": .public(piVersion.map(String.init) ?? "nil"),
                ])
                protocolGateLoggedBlocked = true
            }
            return
        }
        // Re-log once when the gate opens after being blocked.
        if protocolGateLoggedBlocked {
            logger.info("phone_calls enabled: Pi protocol_version now sufficient", metadata: [
                "current": .public(String(pv)),
            ])
            protocolGateLoggedBlocked = false
        }

        // STEP 1 + 2 — Open pool fresh + validate schema (cached).
        let pool: DatabasePool
        do {
            pool = try await openFreshPool()
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

        // STEP 3 — Load cursor.
        let cursor: PhoneCallsCursor
        do {
            cursor = try await loadOrFetchCursor()
        } catch {
            logger.warning("phone_calls tick: cursor fetch failed", metadata: [
                "error": .private(String(describing: error)),
            ])
            await markUnhealthy(reason: "cursor_fetch_failed")
            return
        }

        // STEP 5 — Sender filter populated?
        let populated = await cache.isPopulated
        if !populated {
            logger.debug("phone_calls tick: known-identifiers cache empty; skipping", metadata: [:])
            return
        }

        // STEP 4 — Capture install-time MAX(ZDATE, Z_PK) if needed.
        var working = cursor
        if working.installMaxZDate == nil {
            do {
                let maxPoint = try await pool.read { try CallHistoryDBReader.maxZDate(db: $0) }
                if let p = maxPoint {
                    working.installMaxZDate = Date(
                        timeIntervalSince1970:
                            CallHistoryDBReader.appleEpochOffset + p.zdate)
                    working.installMaxZPK = p.zPK
                    working.liveCursorZDate = working.installMaxZDate
                    working.liveCursorZPK = p.zPK
                    logger.info("phone_calls tick: captured install-time MAX(ZDATE, Z_PK)", metadata: [
                        "max_zdate": .public(String(p.zdate)),
                        "max_z_pk": .public(String(p.zPK)),
                    ])
                }
                // Empty table is fine; install_max stays nil and the
                // first row inserted will start live iteration from the
                // empty floor.
            } catch {
                await markUnhealthy(reason: "max_zdate_failed")
                return
            }
        }

        var budget = PhoneCallsBudget(
            maxRows: config.maxRowsPerTick,
            maxDuration: config.maxDurationPerTick,
            now: tickStart)
        var hadRejection = false
        var hadUnconfirmed = false

        // v1.5 deferred: identifier-scoped 30-day backwards scan. We
        // observe the newly-added queue for visibility but do NOT
        // consume it (parity with the merged messages plugin per
        // D-DEVIATION-1).
        let newlyAdded = await cache.drainNewlyAdded()
        if !newlyAdded.isEmpty {
            logger.info("phone_calls tick: newly-known contacts detected", metadata: [
                "count": .public(String(newlyAdded.count)),
            ])
        }

        // STEP 6a — Backfill batch (descending).
        if !working.backfillComplete, budget.rowsRemaining > 0 {
            let outcome = await runBackfillBatch(
                pool: pool,
                cursor: &working,
                budget: &budget)
            if !outcome.rejected.isEmpty { hadRejection = true }
            if outcome.hadUnconfirmedItems { hadUnconfirmed = true }
        }

        // STEP 6b — Live batch (ascending).
        if !budget.exhausted(now: clock()) {
            let outcome = await runLiveBatch(
                pool: pool,
                cursor: &working,
                budget: &budget)
            if !outcome.rejected.isEmpty { hadRejection = true }
            if outcome.hadUnconfirmedItems { hadUnconfirmed = true }
        }

        // STEP 7 — Commit cursor only if changed, no rejections, AND
        // every item we tried to publish was confirmed.
        let cursorChanged = working != cursor
        if cursorChanged && !hadRejection && !hadUnconfirmed {
            do {
                let newJSON = try PhoneCallsCursorCodec.encode(working)
                try await piClient.commitCursor(
                    auth: auth,
                    source: "phone_calls",
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
                logger.debug("phone_calls tick: cursor committed", metadata: [
                    "backfill_complete": .public(String(working.backfillComplete)),
                ])
            } catch let PiClientError.cursorConflict(_, current) {
                logger.info("phone_calls tick: cursor conflict, refreshing", metadata: [
                    "current_epoch": .public(current.currentEpoch.map(String.init) ?? "nil"),
                ])
                _ = try? await loadOrFetchCursor(forceRefresh: true)
                return
            } catch {
                logger.warning("phone_calls tick: cursor commit failed", metadata: [
                    "error": .private(String(describing: error)),
                ])
                return
            }
        } else if hadRejection {
            logger.warning("phone_calls tick: per-event rejections; holding cursor", metadata: [:])
        } else if hadUnconfirmed {
            logger.warning("phone_calls tick: publish unconfirmed; holding cursor", metadata: [:])
        }

        // STEP 8 — Refresh health snapshot.
        await healthRegistry.update(id, currentHealthSnapshot(
            enabled: true,
            lastScheduled: tickStart,
            lastPushed: cursorChanged ? clock() : nil,
            backfillComplete: working.backfillComplete))
    }

    // MARK: - per-tick batch runners

    private struct BatchSummary {
        let accepted: Int
        let duplicate: Int
        let rejected: [PhoneCallPerEventRejection]
        let hadUnconfirmedItems: Bool
    }

    /// Backfill: walk (ZDATE, Z_PK) downward from the cursor (or
    /// install max). Stops when budget exhausted or no more rows.
    private func runBackfillBatch(
        pool: DatabasePool,
        cursor: inout PhoneCallsCursor,
        budget: inout PhoneCallsBudget
    ) async -> BatchSummary {
        let upperZDate: Double
        let upperZPK: Int64
        if let bcDate = cursor.backfillCursorZDate, let bcPK = cursor.backfillCursorZPK {
            upperZDate = bcDate.timeIntervalSince1970 - CallHistoryDBReader.appleEpochOffset
            upperZPK = bcPK
        } else if let imDate = cursor.installMaxZDate, let imPK = cursor.installMaxZPK {
            // First descent: start from install_max + 1 effectively
            // (we use upper = install_max and the SQL is < — so we
            // skip the install_max row, but live iteration covers
            // it).
            upperZDate = imDate.timeIntervalSince1970 - CallHistoryDBReader.appleEpochOffset
            upperZPK = imPK
        } else {
            // Empty table: nothing to backfill.
            cursor.backfillComplete = true
            return BatchSummary(accepted: 0, duplicate: 0,
                                rejected: [], hadUnconfirmedItems: false)
        }

        let limit = min(budget.rowsRemaining, PhoneCallsPublisher.maxEventsPerBatch)
        if limit <= 0 {
            return BatchSummary(accepted: 0, duplicate: 0,
                                rejected: [], hadUnconfirmedItems: false)
        }

        let page: CallHistoryReadPage
        do {
            page = try await pool.read { db in
                try CallHistoryDBReader.fetchPage(
                    db: db,
                    direction: .backwardFromExclusive(zdate: upperZDate, zPK: upperZPK),
                    limit: limit)
            }
        } catch let dbError as DatabaseError where isFDAError(dbError) {
            await markUnhealthy(reason: "fda_required")
            return BatchSummary(accepted: 0, duplicate: 0,
                                rejected: [], hadUnconfirmedItems: false)
        } catch {
            logger.warning("phone_calls tick: backfill read failed", metadata: [
                "error": .private(String(describing: error)),
            ])
            return BatchSummary(accepted: 0, duplicate: 0,
                                rejected: [], hadUnconfirmedItems: false)
        }

        serviceUnknownTotal += page.serviceUnknownCount
        if page.serviceUnknownCount > 0 {
            logger.warning("phone_calls tick: service_unknown rows skipped", metadata: [
                "count": .public(String(page.serviceUnknownCount)),
            ])
        }

        if page.scannedBounds == nil {
            cursor.backfillComplete = true
            return BatchSummary(accepted: 0, duplicate: 0,
                                rejected: [], hadUnconfirmedItems: false)
        }

        let floorEpoch = config.backfillFloor.timeIntervalSince1970
            - CallHistoryDBReader.appleEpochOffset
        let inRange = page.rows.filter { $0.startedAt >= config.backfillFloor }
        let belowFloor = page.rows.count - inRange.count

        let publishItems = await filterAndShape(rows: inRange)
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
        if let bounds = page.scannedBounds, confirmedAllItems {
            cursor.backfillCursorZDate = Date(
                timeIntervalSince1970:
                    CallHistoryDBReader.appleEpochOffset + bounds.min.zdate)
            cursor.backfillCursorZPK = bounds.min.zPK
        }
        if confirmedAllItems && (belowFloor > 0 || page.exhausted) {
            cursor.backfillComplete = true
        }
        _ = floorEpoch  // documented but not currently consumed; kept for symmetry.

        return BatchSummary(
            accepted: outcome.accepted,
            duplicate: outcome.duplicate,
            rejected: outcome.rejected,
            hadUnconfirmedItems: !confirmedAllItems)
    }

    /// Live: walk (ZDATE, Z_PK) upward from the cursor. Stops when
    /// budget exhausted or no more rows.
    private func runLiveBatch(
        pool: DatabasePool,
        cursor: inout PhoneCallsCursor,
        budget: inout PhoneCallsBudget
    ) async -> BatchSummary {
        let lowerZDate: Double
        let lowerZPK: Int64
        if let lcDate = cursor.liveCursorZDate, let lcPK = cursor.liveCursorZPK {
            lowerZDate = lcDate.timeIntervalSince1970 - CallHistoryDBReader.appleEpochOffset
            lowerZPK = lcPK
        } else if let imDate = cursor.installMaxZDate, let imPK = cursor.installMaxZPK {
            lowerZDate = imDate.timeIntervalSince1970 - CallHistoryDBReader.appleEpochOffset
            lowerZPK = imPK
        } else {
            // Empty table: nothing live.
            return BatchSummary(accepted: 0, duplicate: 0,
                                rejected: [], hadUnconfirmedItems: false)
        }

        let limit = min(budget.rowsRemaining, PhoneCallsPublisher.maxEventsPerBatch)
        if limit <= 0 {
            return BatchSummary(accepted: 0, duplicate: 0,
                                rejected: [], hadUnconfirmedItems: false)
        }

        let page: CallHistoryReadPage
        do {
            page = try await pool.read { db in
                try CallHistoryDBReader.fetchPage(
                    db: db,
                    direction: .forwardFromExclusive(zdate: lowerZDate, zPK: lowerZPK),
                    limit: limit)
            }
        } catch let dbError as DatabaseError where isFDAError(dbError) {
            await markUnhealthy(reason: "fda_required")
            return BatchSummary(accepted: 0, duplicate: 0,
                                rejected: [], hadUnconfirmedItems: false)
        } catch {
            logger.warning("phone_calls tick: live read failed", metadata: [
                "error": .private(String(describing: error)),
            ])
            return BatchSummary(accepted: 0, duplicate: 0,
                                rejected: [], hadUnconfirmedItems: false)
        }

        serviceUnknownTotal += page.serviceUnknownCount
        if page.serviceUnknownCount > 0 {
            logger.warning("phone_calls tick: service_unknown rows skipped", metadata: [
                "count": .public(String(page.serviceUnknownCount)),
            ])
        }

        if page.scannedBounds == nil {
            return BatchSummary(accepted: 0, duplicate: 0,
                                rejected: [], hadUnconfirmedItems: false)
        }

        let publishItems = await filterAndShape(rows: page.rows)
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
        if let bounds = page.scannedBounds, confirmedAllItems {
            cursor.liveCursorZDate = Date(
                timeIntervalSince1970:
                    CallHistoryDBReader.appleEpochOffset + bounds.max.zdate)
            cursor.liveCursorZPK = bounds.max.zPK
        }

        return BatchSummary(
            accepted: outcome.accepted,
            duplicate: outcome.duplicate,
            rejected: outcome.rejected,
            hadUnconfirmedItems: !confirmedAllItems)
    }

    /// Apply sender filter + shape to publishable form. Missed-no-
    /// voicemail rows ARE forwarded (the Pi decides interaction
    /// creation via the decision table); the daemon's job is to push
    /// every call whose peer is in the known-identifiers set.
    private func filterAndShape(rows: [CallHistoryRow]) async -> [PhoneCallPublishItem] {
        // Take a snapshot of the cache once per call so per-row
        // membership checks are O(1) Set lookups (instead of N
        // round-trips through the actor). The snapshot is a
        // defensive copy from KnownIdentifiersCache.snapshot().
        let knownSet = await cache.snapshot()
        var out: [PhoneCallPublishItem] = []
        out.reserveCapacity(rows.count)
        for row in rows {
            guard let rawAddr = row.address, !rawAddr.isEmpty else { continue }
            let canonical = canonicalizer(rawAddr)
            if canonical.isEmpty { continue }
            // Sender filter: drop rows whose canonical peer is not in
            // the known-identifiers cache. The Pi would identity-
            // match-fail and reject the event anyway, holding the
            // cursor; filtering here keeps the cursor advancing.
            if !knownSet.contains(canonical) { continue }
            // Service derivation already happened in the reader.
            guard let (kind, payload) = CallPayloadShaping.shape(
                row: row,
                peerNormalized: canonical,
                hostID: auth.hostID
            ) else {
                continue
            }
            out.append(PhoneCallPublishItem(
                cursorPoint: CallCursorPoint(
                    zdate: row.startedAt.timeIntervalSince1970
                        - CallHistoryDBReader.appleEpochOffset,
                    zPK: row.zPK),
                direction: kind,
                payload: payload))
        }
        return out
    }

    // MARK: - schema + pool

    private enum SchemaDriftError: Error {
        case drift(table: String, missing: Set<String>)
    }

    private struct FDAError: Error {}

    /// Open CallHistoryDB fresh every tick. `?mode=ro&immutable=1` is
    /// per spec line 142 (P2-J); the daemon's ~60s tick cadence makes
    /// reopening cheap, and a long-lived immutable handle would NOT
    /// see new Phone/FaceTime writes (they're invisible to immutable
    /// readers by design).
    private func openFreshPool() async throws -> DatabasePool {
        var grdbConfig = Configuration()
        grdbConfig.readonly = true
        // Spec line 142 specifies `?mode=ro&immutable=1`. GRDB's
        // DatabasePool accepts a URI when prefixed with `file:`;
        // SQLite parses the query string. immutable=1 enables full
        // crash-resilience guarantees from SQLite (no WAL coherence
        // overhead) at the cost of NOT seeing writes from the
        // CallHistoryDB writer. The reopen-every-tick pattern above
        // converts that into normal polling.
        let uri = "file://\(config.callHistoryDBPath.path)?mode=ro&immutable=1"
        let pool: DatabasePool
        do {
            pool = try DatabasePool(path: uri, configuration: grdbConfig)
        } catch let dbError as DatabaseError {
            if isFDAError(dbError) { throw FDAError() }
            throw dbError
        }
        // Schema validation: only run on first successful open or when
        // a previous open recorded drift (so the operator's "doctor"
        // probe can re-validate after a macOS upgrade). The result is
        // stable across pool reopens.
        if schemaHealth == nil || schemaHealth != .ok {
            let health = try await pool.read { try CallHistoryDBSchemaValidator.validate(db: $0) }
            schemaHealth = health
            switch health {
            case .ok:
                return pool
            case .drift(let table, let missing):
                throw SchemaDriftError.drift(table: table, missing: missing)
            }
        }
        return pool
    }

    private func isFDAError(_ err: DatabaseError) -> Bool {
        // FDA / sandbox denial: SQLITE_AUTH (23), SQLITE_PERM (3),
        // or SQLITE_CANTOPEN (14). Same set as chat.db.
        let code = err.resultCode.rawValue
        return code == 3 || code == 14 || code == 23
    }

    // MARK: - cursor load + commit

    @discardableResult
    private func loadOrFetchCursor(forceRefresh: Bool = false) async throws -> PhoneCallsCursor {
        if !forceRefresh, let cached = cachedCursor {
            return cached
        }
        let state = try await piClient.getCursor(auth: auth, source: "phone_calls")
        cachedEpoch = state.cursorEpoch
        lastCommittedCursorJSON = state.cursor
        let decoded = try PhoneCallsCursorCodec.decode(state.cursor)
        let fresh = decoded ?? PhoneCallsCursor(backfillFloorSentAt: config.backfillFloor)
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
            var src = state.sources["phone_calls"] ?? SourceState(cursor: "")
            src.cursor = json
            src.cursorEpoch = epoch
            src.backfillComplete = backfillComplete
            src.lastPushedAt = lastPushedAt
            state.sources["phone_calls"] = src
        }
    }

    // MARK: - health surface

    private func currentHealthSnapshot(
        enabled: Bool,
        lastScheduled: Date,
        lastPushed: Date? = nil,
        backfillComplete: Bool? = nil
    ) -> SourceHealthSnapshot {
        SourceHealthSnapshot(
            enabled: enabled,
            lastScheduledAt: lastScheduled,
            lastPushedAt: lastPushed,
            observedCursor: nil,
            pushedCursor: nil,
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
        logger.error("phone_calls tick: marked unhealthy", metadata: [
            "reason": .public(reason),
        ])
    }
}
