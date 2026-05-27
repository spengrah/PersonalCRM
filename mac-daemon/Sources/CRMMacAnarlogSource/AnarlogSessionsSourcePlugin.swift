// AnarlogSessionsSourcePlugin — actor that orchestrates one
// anarlog_sessions tick.
//
// Same route + P0 invariants as the humans plugin but with a few
// session-specific twists:
//   - reads three files per session (`_meta.json`, `_summary.md?`,
//     `_memo.md?`); each contributes to the cursor entry's hash
//     fields and only file-bytes changes drive contentChanged
//   - pre-backfill-floor sessions (created_at < 2026-01-01) get a
//     `floor_skip` sentinel cursor entry and never emit an event
//   - skip-list filters at the top level (chats/, settings.json, etc.)
//   - in-flight coalescing per JC7 — FSEvents and the safety timer
//     can both fire near-simultaneously; only one tick runs at a
//     time, with a flag to re-tick once if a request arrived mid-tick
//   - calls /known-ids on bootstrap / recovery routes per round-4
//     P1#6 (gets empty results in PR 2 — `external_contact` table
//     has no anarlog_sessions rows — but the code path is symmetric
//     with humans; PR 3 lights up tombstones via a Pi-side query body
//     change)
import Foundation
import CRMMacCore
import CRMMacPiClient

public actor AnarlogSessionsSourcePlugin: SourcePlugin {
    public nonisolated let id: SourceID = .anarlogSessions
    public nonisolated let tickInterval: TimeInterval

    private let piClient: PiClient
    private let auth: PiAuth
    private let mutator: StateMutator
    private let publisher: AnarlogSessionsPublisher
    private let filesystem: AnarlogFilesystem
    private let configSource: AnarlogConfigSource
    private let healthRegistry: SourceHealthRegistry
    private let logger: LoggerProtocol
    private let clock: @Sendable () -> Date

    // In-flight coalescing (JC7).
    private var tickInFlight: Bool = false
    private var pendingRequest: Bool = false

    public init(
        tickInterval: TimeInterval = CRMMacAnarlogSource.sessionsSafetyTickInterval,
        piClient: PiClient,
        auth: PiAuth,
        mutator: StateMutator,
        publisher: AnarlogSessionsPublisher,
        filesystem: AnarlogFilesystem,
        configSource: AnarlogConfigSource,
        healthRegistry: SourceHealthRegistry,
        logger: LoggerProtocol,
        clock: @escaping @Sendable () -> Date = { Date() }
    ) {
        self.tickInterval = tickInterval
        self.piClient = piClient
        self.auth = auth
        self.mutator = mutator
        self.publisher = publisher
        self.filesystem = filesystem
        self.configSource = configSource
        self.healthRegistry = healthRegistry
        self.logger = logger
        self.clock = clock
    }

    public func tick() async throws {
        // In-flight coalescing per JC7. The actor's serial mailbox
        // already prevents concurrent tick() bodies; this loop
        // additionally absorbs MULTIPLE pending requests while a tick
        // is running so the next tick fires exactly once after the
        // current one finishes.
        if tickInFlight {
            pendingRequest = true
            return
        }
        tickInFlight = true
        defer { tickInFlight = false }
        repeat {
            pendingRequest = false
            try await runTick()
        } while pendingRequest
    }

    private func runTick() async throws {
        let tickStart = clock()
        await updateScheduled(at: tickStart)
        await healthRegistry.update(
            id, healthSnapshot(enabled: true, lastScheduled: tickStart))

        // Config check.
        let config: AnarlogConfig?
        do {
            config = try configSource.load()
        } catch {
            await markUnhealthy("config_load_failed:\(error)")
            return
        }
        guard let cfg = config, cfg.sessionsEnabled else {
            await markUnhealthy("not_configured")
            return
        }

        let rootExpanded = AnarlogPathResolver.expand(cfg.rootPath).path
        guard filesystem.exists(rootExpanded) else {
            await markUnhealthy("path_missing")
            return
        }
        let sessionsPath = AnarlogPathResolver.sessionsDir(rootPath: cfg.rootPath).path
        guard filesystem.exists(sessionsPath) else {
            await markUnhealthy("sessions_subdir_missing")
            return
        }
        guard filesystem.isReadableDirectory(sessionsPath) else {
            await markUnhealthy("files_folders_permission_denied")
            return
        }

        // Cursor fetch.
        let cursorState: SourceCursorState
        do {
            cursorState = try await piClient.getCursor(auth: auth, source: id.rawValue)
        } catch {
            logger.warning("anarlog_sessions tick: cursor fetch failed", metadata: [
                "error": .private(String(describing: error)),
            ])
            await markUnhealthy("cursor_fetch_failed")
            return
        }
        let decodedOpt = AnarlogSessionsCursorCodec.decodeOrNil(cursorState.cursor)

        // Recovery flag check.
        let state = try? await mutator.read()
        let priorError = state?.sources[id.rawValue]?.lastError ?? ""
        let recoveryRequested = priorError.hasPrefix("recovery_requested:")

        // Route selection per D4.
        var entryRoute: AnarlogTickRoute.Kind
        let decoded = decodedOpt ?? [:]
        if recoveryRequested {
            entryRoute = .recovery
        } else if decodedOpt == nil {
            entryRoute = .bootstrapViaKnownIDs
        } else {
            entryRoute = .delta
        }

        // /known-ids fetch. In PR 2 the Pi-side query returns empty
        // for source=anarlog_sessions (no `external_contact` rows
        // with that source — sessions live in a future `meeting_note`
        // table). PR 3 changes the Pi-side query body, NOT this code.
        // We still call /known-ids on bootstrap/recovery routes for
        // symmetry with humans + so PR 3 lights up automatically.
        var knownIDs: KnownIDsData?
        if entryRoute == .bootstrapViaKnownIDs || entryRoute == .recovery {
            do {
                knownIDs = try await piClient.knownIDs(auth: auth, source: id.rawValue)
            } catch {
                logger.warning("anarlog_sessions tick: known_ids fetch failed", metadata: [
                    "error": .private(String(describing: error)),
                ])
                await markUnhealthy("known_ids_fetch_failed")
                return
            }
            if entryRoute == .bootstrapViaKnownIDs && (knownIDs?.ids.isEmpty ?? true) {
                entryRoute = .firstRun
                knownIDs = nil
            }
        }

        var knownByEntityID: [String: String?] = [:]
        if let kids = knownIDs {
            for entry in kids.ids {
                let entityID = AnarlogHumansSourcePlugin.entityIDFromSourceID(entry.sourceID)
                knownByEntityID[entityID] = entry.lastContentHash
            }
        }

        // Full inventory scan.
        var seenPhysicalUUIDs: Set<String> = []
        var desiredCursor: [String: AnarlogSessionsCursorEntry] = [:]
        var publishItems: [AnarlogSessionsPublishItem] = []
        var parseFailedCount = 0
        var payloadTooLargeCount = 0

        let entries: [String]
        do {
            entries = try filesystem.listDirectory(sessionsPath)
        } catch AnarlogFilesystemError.permissionDenied {
            await markUnhealthy("files_folders_permission_denied")
            return
        } catch {
            await markUnhealthy("dir_list_failed:\(error)")
            return
        }

        for entry in entries {
            if CRMMacAnarlogSource.sessionSkipEntries.contains(entry) { continue }
            // Session dirs are UUID-named. Anything else (foo.txt,
            // bare files, dirs that don't UUID-parse) is silently
            // skipped — covers Anarlog dropping unknown junk in the
            // sessions/ root.
            guard let canonicalUUID = AnarlogUUIDValidator.canonicalize(entry) else {
                continue
            }
            let sessionDir = (sessionsPath as NSString).appendingPathComponent(entry)
            guard filesystem.isReadableDirectory(sessionDir) else { continue }
            seenPhysicalUUIDs.insert(canonicalUUID)

            let metaPath = (sessionDir as NSString).appendingPathComponent("_meta.json")
            let summaryPath = (sessionDir as NSString).appendingPathComponent("_summary.md")
            let memoPath = (sessionDir as NSString).appendingPathComponent("_memo.md")

            // _meta.json is REQUIRED; without it the session is
            // malformed (P0 carry-forward).
            guard filesystem.exists(metaPath) else {
                parseFailedCount += 1
                logger.warning("anarlog_sessions tick: meta_missing", metadata: [
                    "uuid": .private(canonicalUUID),
                ])
                carrySessionForward(
                    uuid: canonicalUUID,
                    metaBytesHash: "",
                    summaryBytesHash: nil,
                    memoBytesHash: nil,
                    prior: decoded[canonicalUUID],
                    entryRoute: entryRoute,
                    knownByEntityID: knownByEntityID,
                    desiredCursor: &desiredCursor)
                continue
            }
            let metaBytes: Data
            do {
                metaBytes = try filesystem.readFile(metaPath)
            } catch {
                parseFailedCount += 1
                logger.warning("anarlog_sessions tick: meta_read_failed", metadata: [
                    "uuid": .private(canonicalUUID),
                    "error": .private(String(describing: error)),
                ])
                carrySessionForward(
                    uuid: canonicalUUID,
                    metaBytesHash: "",
                    summaryBytesHash: nil,
                    memoBytesHash: nil,
                    prior: decoded[canonicalUUID],
                    entryRoute: entryRoute,
                    knownByEntityID: knownByEntityID,
                    desiredCursor: &desiredCursor)
                continue
            }
            let metaBytesHash = AnarlogFileHash.sha256Hex(metaBytes)
            let summaryBytes: Data? = filesystem.exists(summaryPath)
                ? (try? filesystem.readFile(summaryPath)) : nil
            let memoBytes: Data? = filesystem.exists(memoPath)
                ? (try? filesystem.readFile(memoPath)) : nil
            let summaryHash = summaryBytes.map(AnarlogFileHash.sha256Hex)
            let memoHash = memoBytes.map(AnarlogFileHash.sha256Hex)

            let prior = decoded[canonicalUUID]
            let contentChanged: Bool
            if let prior {
                contentChanged = prior.metaHash != metaBytesHash
                    || prior.summaryHash != summaryHash
                    || prior.memoHash != memoHash
            } else {
                contentChanged = true
            }

            guard let meta = AnarlogSessionMetaParser.parse(
                uuid: canonicalUUID, metaJSONBytes: metaBytes) else {
                parseFailedCount += 1
                logger.warning("anarlog_sessions tick: meta_parse_failed", metadata: [
                    "uuid": .private(canonicalUUID),
                ])
                carrySessionForward(
                    uuid: canonicalUUID,
                    metaBytesHash: metaBytesHash,
                    summaryBytesHash: summaryHash,
                    memoBytesHash: memoHash,
                    prior: prior,
                    entryRoute: entryRoute,
                    knownByEntityID: knownByEntityID,
                    desiredCursor: &desiredCursor)
                continue
            }

            // Pre-backfill-floor: park as sentinel. Never emit.
            if AnarlogSessionsPayloadShaping.isPreBackfillFloor(meta) {
                desiredCursor[canonicalUUID] = AnarlogSessionsCursorCodec.floorSkippedEntry()
                continue
            }

            // Shape + size check.
            let summary = summaryBytes.flatMap { String(data: $0, encoding: .utf8) }
            let memo = memoBytes.flatMap { String(data: $0, encoding: .utf8) }
            let payload = AnarlogSessionsPayloadShaping.shape(
                meta: meta, summary: summary, memo: memo, hostID: auth.hostID)
            let payloadBytes: Data
            do {
                let encoder = JSONEncoder()
                encoder.outputFormatting = [.withoutEscapingSlashes]
                payloadBytes = try encoder.encode(payload)
            } catch {
                parseFailedCount += 1
                logger.warning("anarlog_sessions tick: payload_encode_failed", metadata: [
                    "uuid": .private(canonicalUUID),
                    "error": .private(String(describing: error)),
                ])
                carrySessionForward(
                    uuid: canonicalUUID,
                    metaBytesHash: metaBytesHash,
                    summaryBytesHash: summaryHash,
                    memoBytesHash: memoHash,
                    prior: prior,
                    entryRoute: entryRoute,
                    knownByEntityID: knownByEntityID,
                    desiredCursor: &desiredCursor)
                continue
            }
            if payloadBytes.count > CRMMacAnarlogSource.maxPayloadBytes {
                payloadTooLargeCount += 1
                logger.error("anarlog_sessions tick: payload_too_large", metadata: [
                    "uuid": .private(canonicalUUID),
                    "size": .public(String(payloadBytes.count)),
                ])
                carrySessionForward(
                    uuid: canonicalUUID,
                    metaBytesHash: metaBytesHash,
                    summaryBytesHash: summaryHash,
                    memoBytesHash: memoHash,
                    prior: prior,
                    entryRoute: entryRoute,
                    knownByEntityID: knownByEntityID,
                    desiredCursor: &desiredCursor)
                continue
            }

            let payloadHash: String
            let shouldEmit: Bool
            switch entryRoute {
            case .recovery, .bootstrapViaKnownIDs, .firstRun:
                payloadHash = (try? ContentHasher.contentHash(for: payloadBytes)) ?? ""
                shouldEmit = !payloadHash.isEmpty
            case .delta:
                if contentChanged {
                    payloadHash = (try? ContentHasher.contentHash(for: payloadBytes)) ?? ""
                    shouldEmit = !payloadHash.isEmpty
                } else {
                    if let prior, !prior.payloadHash.isEmpty {
                        payloadHash = prior.payloadHash
                    } else {
                        payloadHash = (try? ContentHasher.contentHash(for: payloadBytes)) ?? ""
                    }
                    shouldEmit = false
                }
            }

            if shouldEmit {
                let sourceID = AnarlogSourceIDBuilder.upsertSourceID(
                    entityID: canonicalUUID, payloadHash: payloadHash)
                publishItems.append(AnarlogSessionsPublishItem(
                    sourceID: sourceID,
                    kind: "meeting_note.recorded",
                    payloadBytes: payloadBytes))
            }

            desiredCursor[canonicalUUID] = AnarlogSessionsCursorEntry(
                metaHash: metaBytesHash,
                summaryHash: summaryHash,
                memoHash: memoHash,
                payloadHash: payloadHash)
        }

        // Tombstone basis per D4. The floor_skip sentinel entries
        // never produce tombstones (isFloorSkipped guard).
        var tombstoneBasis: [AnarlogTombstoneBasisEntry] = []
        switch entryRoute {
        case .firstRun:
            tombstoneBasis = []
        case .delta:
            for (uuid, entry) in decoded {
                tombstoneBasis.append(AnarlogTombstoneBasisEntry(
                    uuid: uuid,
                    priorPayloadHash: entry.payloadHash.isEmpty ? nil : entry.payloadHash,
                    isFloorSkipped: entry.isFloorSkipped))
            }
        case .bootstrapViaKnownIDs, .recovery:
            for (uuid, lastHash) in knownByEntityID {
                tombstoneBasis.append(AnarlogTombstoneBasisEntry(
                    uuid: uuid,
                    priorPayloadHash: lastHash,
                    isFloorSkipped: false))
            }
        }

        for basis in tombstoneBasis {
            if basis.isFloorSkipped { continue }
            if seenPhysicalUUIDs.contains(basis.uuid) { continue }
            let deleted = AnarlogSessionsPayloadShaping.shapeDeleted(
                sessionID: basis.uuid, hostID: auth.hostID)
            let deletedBytes: Data
            do {
                let encoder = JSONEncoder()
                encoder.outputFormatting = [.withoutEscapingSlashes]
                deletedBytes = try encoder.encode(deleted)
            } catch {
                logger.warning("anarlog_sessions tick: delete_encode_failed", metadata: [
                    "uuid": .private(basis.uuid),
                    "error": .private(String(describing: error)),
                ])
                continue
            }
            let sourceID = AnarlogSourceIDBuilder.deleteSourceID(
                entityID: basis.uuid,
                priorPayloadHash: basis.priorPayloadHash)
            publishItems.append(AnarlogSessionsPublishItem(
                sourceID: sourceID,
                kind: "meeting_note.deleted",
                payloadBytes: deletedBytes))
        }

        // Publish.
        let outcome = await publisher.publish(items: publishItems)
        let hadHashMismatch = outcome.rejected.contains { rej in
            AnarlogSessionsPublisher.recoveryCodes.contains(rej.code)
        }
        if hadHashMismatch {
            await setRecoveryFlag(reason: "hash_mismatch")
        }

        let cleanBatch = outcome.rejected.isEmpty && outcome.unconfirmed == 0
        if !cleanBatch {
            var errMsg = "publish_held_due_to_rejections (\(outcome.rejected.count) rejected"
            if outcome.unconfirmed > 0 {
                errMsg += ", \(outcome.unconfirmed) unconfirmed"
            }
            errMsg += ")"
            if parseFailedCount > 0 || payloadTooLargeCount > 0 {
                errMsg += "; anomalies parse_failed=\(parseFailedCount) payload_too_large=\(payloadTooLargeCount)"
            }
            await recordLastError(errMsg)
            return
        }

        let desiredCursorBytes: String
        do {
            desiredCursorBytes = try AnarlogSessionsCursorCodec.encode(desiredCursor)
        } catch {
            await markUnhealthy("cursor_encode_failed:\(error)")
            return
        }
        do {
            try await piClient.commitCursor(
                auth: auth,
                source: id.rawValue,
                cursor: desiredCursorBytes,
                baseCursor: cursorState.cursor,
                cursorEpoch: cursorState.cursorEpoch,
                backfillComplete: true)
        } catch {
            logger.warning("anarlog_sessions tick: cursor commit failed", metadata: [
                "error": .private(String(describing: error)),
            ])
            return
        }

        let pushedAt = clock()
        let anomalyMsg: String? = (parseFailedCount > 0 || payloadTooLargeCount > 0)
            ? "anomalies parse_failed=\(parseFailedCount) payload_too_large=\(payloadTooLargeCount)"
            : nil
        await commitCleanTick(
            cursor: desiredCursorBytes,
            cursorEpoch: cursorState.cursorEpoch,
            pushedAt: pushedAt,
            anomalyError: anomalyMsg)

        await healthRegistry.update(
            id, healthSnapshot(
                enabled: true,
                lastScheduled: tickStart,
                lastPushed: pushedAt))

        logger.debug("anarlog_sessions tick: complete", metadata: [
            "route": .public(String(describing: entryRoute)),
            "emitted": .public(String(publishItems.count)),
            "accepted": .public(String(outcome.accepted)),
            "duplicate": .public(String(outcome.duplicate)),
        ])
    }

    // MARK: - per-file failure helper

    private func carrySessionForward(
        uuid: String,
        metaBytesHash: String,
        summaryBytesHash: String?,
        memoBytesHash: String?,
        prior: AnarlogSessionsCursorEntry?,
        entryRoute: AnarlogTickRoute.Kind,
        knownByEntityID: [String: String?],
        desiredCursor: inout [String: AnarlogSessionsCursorEntry]
    ) {
        if let prior {
            desiredCursor[uuid] = prior
            return
        }
        if entryRoute == .bootstrapViaKnownIDs || entryRoute == .recovery,
           let knownLastHash = knownByEntityID[uuid] {
            let payloadHash = knownLastHash ?? "unknown"
            desiredCursor[uuid] = AnarlogSessionsCursorEntry(
                metaHash: metaBytesHash.isEmpty ? "unknown" : metaBytesHash,
                summaryHash: summaryBytesHash,
                memoHash: memoBytesHash,
                payloadHash: payloadHash)
        }
    }

    // MARK: - state mutators

    private func updateScheduled(at date: Date) async {
        do {
            try await mutator.mutate { state in
                var src = state.sources[self.id.rawValue] ?? SourceState()
                src.lastScheduledAt = date
                state.sources[self.id.rawValue] = src
            }
        } catch {
            logger.warning("anarlog_sessions tick: lastScheduledAt mutate failed", metadata: [
                "error": .private(String(describing: error)),
            ])
        }
    }

    private func commitCleanTick(
        cursor: String,
        cursorEpoch: Int64,
        pushedAt: Date,
        anomalyError: String?
    ) async {
        do {
            try await mutator.mutate { state in
                var src = state.sources[self.id.rawValue] ?? SourceState()
                src.cursor = cursor
                src.cursorEpoch = cursorEpoch
                src.lastPushedAt = pushedAt
                if let anomalyError {
                    src.lastError = anomalyError
                    src.lastErrorAt = pushedAt
                } else {
                    src.lastError = nil
                    src.lastErrorAt = nil
                }
                state.sources[self.id.rawValue] = src
            }
        } catch {
            logger.warning("anarlog_sessions tick: commitCleanTick mutate failed", metadata: [
                "error": .private(String(describing: error)),
            ])
        }
    }

    private func setRecoveryFlag(reason: String) async {
        do {
            try await mutator.mutate { state in
                var src = state.sources[self.id.rawValue] ?? SourceState()
                src.lastError = "recovery_requested:\(reason)"
                src.lastErrorAt = self.clock()
                state.sources[self.id.rawValue] = src
            }
        } catch {
            logger.warning("anarlog_sessions tick: setRecoveryFlag failed", metadata: [
                "error": .private(String(describing: error)),
            ])
        }
    }

    private func recordLastError(_ msg: String) async {
        do {
            try await mutator.mutate { state in
                var src = state.sources[self.id.rawValue] ?? SourceState()
                if let existing = src.lastError,
                   existing.hasPrefix("recovery_requested:") {
                    src.lastError = "\(existing); \(msg)"
                } else {
                    src.lastError = msg
                }
                src.lastErrorAt = self.clock()
                state.sources[self.id.rawValue] = src
            }
        } catch {
            logger.warning("anarlog_sessions tick: recordLastError failed", metadata: [
                "error": .private(String(describing: error)),
            ])
        }
    }

    private func markUnhealthy(_ reason: String) async {
        let now = clock()
        let snap = SourceHealthSnapshot(
            enabled: false,
            lastScheduledAt: now,
            lastError: reason,
            lastErrorAt: now)
        await healthRegistry.update(id, snap)
        await recordLastError(reason)
        logger.warning("anarlog_sessions tick: marked unhealthy", metadata: [
            "reason": .public(reason),
        ])
    }

    private func healthSnapshot(
        enabled: Bool,
        lastScheduled: Date,
        lastPushed: Date? = nil
    ) -> SourceHealthSnapshot {
        SourceHealthSnapshot(
            enabled: enabled,
            lastScheduledAt: lastScheduled,
            lastPushedAt: lastPushed,
            backfillComplete: true,
            lastError: nil,
            lastErrorAt: nil)
    }
}
