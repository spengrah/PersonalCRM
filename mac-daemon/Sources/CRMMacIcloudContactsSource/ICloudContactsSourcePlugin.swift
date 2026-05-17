// ICloudContactsSourcePlugin — actor that orchestrates one
// icloud_contacts tick.
//
// Per-tick flow (REVISED per plan §11 — Codex r1/r2/r3 deltas):
//   1.  Stamp state.sources[icloud_contacts].lastScheduledAt = NOW.
//   2.  Authorization probe: .denied/.restricted/.notDetermined →
//       mark unhealthy + return (no cursor change, no prompt — the
//       daemon never prompts, only install does).
//   3.  Load CNContainer allowlist from ICloudContactsConfig.
//       Empty list → mark unhealthy with no_containers_configured.
//   4.  Fetch the Pi-side cursor for the source.
//   5.  Check the recovery flag (state.lastError startsWith
//       "recovery_requested:"). If set → recovery path.
//   6.  First-run path (empty cursor AND no recovery flag AND
//       /known-ids returns empty):
//         a. Capture currentToken BEFORE the snapshot read.
//         b. Full fetch via the reader.
//         c. Filter by allowlist (defense in depth — the reader
//            already scopes by container).
//         d. Shape + hash + emit .upserted; applyUpdates per event.
//         e. Use the pre-snapshot token as the new cursor.
//         f. If /known-ids returns non-empty and we entered first
//            run anyway → route to recovery (state-loss guard).
//   7.  Delta path (cursor present, recovery flag NOT set):
//         a. Walk change history from the cursor.
//         b. Fail-closed on any .unknown event: set recovery flag,
//            mark unhealthy, abort.
//         c. For each .add/.update/.delete:
//              - filter by allowlist (delete events bypass the
//                filter — they don't carry container info; emit
//                unconditionally).
//              - shape upserts; emit .upserted + applyUpdates.
//              - emit .deleted with prior hash from the cache;
//                stagePendingRemovals.
//   8.  Recovery path (entered from 5/6f/7b OR token-invalid in 7a):
//         a. Capture currentToken BEFORE the scan.
//         b. GET /known-ids for the source.
//         c. fullFetch via the reader.
//         d. Diff:
//              - in Pi but NOT in scan → .deleted with last_content_hash
//                from /known-ids (Pi authoritative); stagePending if
//                the identifier is in the cache.
//              - in scan but NOT in Pi → .upserted; applyUpdates.
//              - in both → .upserted (the Pi event-log dedups);
//                applyUpdates.
//         e. Use the pre-scan token as the new cursor.
//   9.  Publish in 100-event batches via ICloudContactsPublisher.
//       Any hash-mismatch rejection sets the recovery flag AND
//       aborts further batches in this tick.
//  10.  Commit cursor ONLY if every batch had no rejections AND
//       no unconfirmed items.
//  11.  Finalize cache: commitPendingRemovals on success;
//       discardPendingRemovals on any abort (the per-tick defer
//       ensures this fires exactly once even on throw paths).
//
// The defer ensures `pendingRemovals` can never accumulate across
// ticks regardless of where the abort happens (per Codex r3 P1-1).
import Foundation
import CRMMacCore
import CRMMacLifecycle
import CRMMacPiClient

/// Inputs the plugin needs from the daemon's composition root.
public struct ICloudContactsConfigInput: Sendable {
    public let containerIdentifiers: [String]
    public init(containerIdentifiers: [String]) {
        self.containerIdentifiers = containerIdentifiers
    }
}

/// Source the plugin reads its allowlist from. The production impl
/// wraps `ConfigStore.loadICloudContactsConfig()`; tests inject a
/// closure to return canned values.
public protocol ICloudContactsConfigSource: Sendable {
    func load() throws -> ICloudContactsConfig?
}

public struct ICloudContactsConfigStoreSource: ICloudContactsConfigSource {
    private let store: ConfigStore
    public init(store: ConfigStore) { self.store = store }
    public func load() throws -> ICloudContactsConfig? {
        try store.loadICloudContactsConfig()
    }
}

public actor ICloudContactsSourcePlugin: SourcePlugin {
    public nonisolated let id: SourceID = .icloudContacts
    public nonisolated let tickInterval: TimeInterval

    private let piClient: PiClient
    private let auth: PiAuth
    private let mutator: StateMutator
    private let publisher: ICloudContactsPublisher
    private let cache: ContactHashCache
    private let reader: ContactStoreReader
    private let authAdapter: ContactsAuthorizationAdapter
    private let configSource: ICloudContactsConfigSource
    private let healthRegistry: SourceHealthRegistry
    private let logger: LoggerProtocol
    private let clock: @Sendable () -> Date

    public init(
        tickInterval: TimeInterval = CRMMacIcloudContactsSource.defaultTickInterval,
        piClient: PiClient,
        auth: PiAuth,
        mutator: StateMutator,
        publisher: ICloudContactsPublisher,
        cache: ContactHashCache,
        reader: ContactStoreReader,
        authAdapter: ContactsAuthorizationAdapter,
        configSource: ICloudContactsConfigSource,
        healthRegistry: SourceHealthRegistry,
        logger: LoggerProtocol,
        clock: @escaping @Sendable () -> Date = { Date() }
    ) {
        self.tickInterval = tickInterval
        self.piClient = piClient
        self.auth = auth
        self.mutator = mutator
        self.publisher = publisher
        self.cache = cache
        self.reader = reader
        self.authAdapter = authAdapter
        self.configSource = configSource
        self.healthRegistry = healthRegistry
        self.logger = logger
        self.clock = clock
    }

    public func tick() async throws {
        let tickStart = clock()
        // Step 1: bump lastScheduledAt for staleness diagnostics —
        // a quiet-but-healthy source bumps this every tick even when
        // no events are emitted. Doctor reads this AND lastPushedAt
        // to surface a meaningful staleness signal.
        await updateScheduled(at: tickStart)
        await healthRegistry.update(id, currentHealthSnapshot(
            enabled: true, lastScheduled: tickStart))

        // Per Codex r3 P1-1: per-tick defer ensures the actor's
        // pendingRemovals set is discarded on ANY abort path — throw,
        // early return, or normal completion without commit. The
        // local flag prevents discardPendingRemovals from undoing a
        // successful commit.
        var stagedRemovalsCommitted = false
        let cacheRef = cache
        defer {
            if !stagedRemovalsCommitted {
                Task { await cacheRef.discardPendingRemovals() }
            }
        }

        // Step 2: authorization probe.
        let authStatus = authAdapter.authorizationStatus()
        switch authStatus {
        case .authorized, .limited:
            break
        case .notDetermined, .denied, .restricted:
            await markUnhealthy(reason: "contacts_permission:\(authStatus)")
            return
        }

        // Step 3: load allowlist.
        let allowlist: [String]
        do {
            allowlist = try configSource.load()?.containers ?? []
        } catch {
            await markUnhealthy(reason: "config_load_failed")
            return
        }
        if allowlist.isEmpty {
            await markUnhealthy(reason: "no_containers_configured")
            return
        }
        let allowSet = Set(allowlist)

        // Step 4: fetch Pi cursor.
        let cursorState: MessagesCursorState
        do {
            cursorState = try await piClient.getCursor(auth: auth, source: id.rawValue)
        } catch {
            logger.warning("icloud tick: cursor fetch failed", metadata: [
                "error": .private(String(describing: error)),
            ])
            await markUnhealthy(reason: "cursor_fetch_failed")
            return
        }

        // Step 5: check recovery flag.
        let state = try? await mutator.read()
        let priorError = state?.sources[id.rawValue]?.lastError ?? ""
        let recoveryRequested = priorError.hasPrefix("recovery_requested:")

        var events: [ContactChange] = []
        let newCursorBytes: Data
        var entryRoute: TickRoute

        let cursorBytes = decodeCursorBase64(cursorState.cursor)
        if recoveryRequested {
            // Route to recovery regardless of cursor validity.
            let (recEvents, recToken) = try await recoveryWalk(allowSet: allowSet)
            events = recEvents
            newCursorBytes = recToken
            entryRoute = .recovery
        } else if cursorBytes == nil || cursorBytes?.isEmpty == true {
            // First-run path. Capture token BEFORE the snapshot read.
            do {
                let knownIDs = try await piClient.knownIDs(auth: auth, source: id.rawValue)
                if !knownIDs.ids.isEmpty {
                    // State-loss case: cursor empty but Pi has rows.
                    // Route to recovery so tombstones get reconciled.
                    let (recEvents, recToken) = try await recoveryWalkUsingKnownIDs(
                        allowSet: allowSet, known: knownIDs)
                    events = recEvents
                    newCursorBytes = recToken
                    entryRoute = .recovery
                } else {
                    let (frEvents, frToken) = try await firstRunWalk(allowSet: allowSet)
                    events = frEvents
                    newCursorBytes = frToken
                    entryRoute = .firstRun
                }
            } catch let e as PiClientError {
                await markUnhealthy(reason: "known_ids_fetch_failed:\(e)")
                return
            } catch {
                await markUnhealthy(reason: "first_run_failed:\(error)")
                return
            }
        } else {
            // Delta path.
            do {
                let (deltaEvents, deltaToken, hadUnknown) =
                    try await deltaWalk(token: cursorBytes!, allowSet: allowSet)
                if hadUnknown {
                    await setRecoveryFlag(reason: "unknown_change_event")
                    await markUnhealthy(reason: "unknown_change_event")
                    return
                }
                events = deltaEvents
                newCursorBytes = deltaToken
                entryRoute = .delta
            } catch CNContactStoreReaderError.tokenInvalid {
                // Route to recovery — the cursor is no longer valid.
                let (recEvents, recToken) = try await recoveryWalk(allowSet: allowSet)
                events = recEvents
                newCursorBytes = recToken
                entryRoute = .recovery
            } catch {
                logger.warning("icloud tick: delta walk failed", metadata: [
                    "error": .private(String(describing: error)),
                ])
                await markUnhealthy(reason: "delta_walk_failed")
                return
            }
        }

        // Build publish items from events (shape + hash + record cache updates).
        // For .upserted: apply update to live cache map immediately.
        // For .deleted:  stage removal (NOT yet finalized).
        var publishItems: [ICloudContactsPublishItem] = []
        var updatesToApply: [String: String] = [:]
        var removalsToStage: Set<String> = []
        for event in events {
            switch event {
            case .add(let record), .update(let record):
                let payload = ICloudContactPayloadShaping.shape(
                    record: record, hostID: auth.hostID, source: id.rawValue)
                let payloadBytes: Data
                let hash: String
                do {
                    payloadBytes = try encodePayload(payload)
                    hash = try ContentHasher.contentHash(for: payloadBytes)
                } catch {
                    logger.warning("icloud tick: hash failed", metadata: [
                        "entity_id": .private(record.identifier),
                        "error": .private(String(describing: error)),
                    ])
                    continue
                }
                let sourceID = SourceIDBuilder.upsertSourceID(
                    entityID: record.identifier, contentHash: hash)
                publishItems.append(ICloudContactsPublishItem(
                    sourceID: sourceID,
                    kind: "external_contact.upserted",
                    payloadBytes: payloadBytes))
                updatesToApply[record.identifier] = hash
            case .delete(let identifier):
                let prior = await cache.get(identifier)
                let deletedPayload = ICloudContactPayloadShaping.shapeDeleted(
                    identifier: identifier, hostID: auth.hostID, source: id.rawValue)
                let deletedBytes: Data
                do {
                    deletedBytes = try encodeDeletedPayload(deletedPayload)
                } catch {
                    logger.warning("icloud tick: encode delete payload failed", metadata: [
                        "entity_id": .private(identifier),
                        "error": .private(String(describing: error)),
                    ])
                    continue
                }
                let sourceID = SourceIDBuilder.deleteSourceID(
                    entityID: identifier, priorContentHash: prior)
                publishItems.append(ICloudContactsPublishItem(
                    sourceID: sourceID,
                    kind: "external_contact.deleted",
                    payloadBytes: deletedBytes))
                removalsToStage.insert(identifier)
            case .unknown:
                // Should not appear here; the delta path drops out
                // of the tick before building publishItems. Belt +
                // suspenders.
                logger.warning("icloud tick: unexpected .unknown event in build phase", metadata: [:])
                continue
            }
        }

        // Apply updates to the cache IMMEDIATELY (per D-JC2 two-phase
        // model). Stage removals in-memory; they don't mutate the
        // file until the cursor commit succeeds.
        do {
            try await cache.applyUpdates(updatesToApply)
        } catch {
            logger.warning("icloud tick: cache.applyUpdates failed", metadata: [
                "error": .private(String(describing: error)),
            ])
            await markUnhealthy(reason: "cache_write_failed")
            return
        }
        if !removalsToStage.isEmpty {
            await cache.stagePendingRemovals(removalsToStage)
        }

        // Publish.
        let outcome = await publisher.publish(items: publishItems)

        // Check for hash-mismatch rejection: routes the daemon to a
        // recovery on the next tick.
        var hadHashMismatch = false
        for r in outcome.rejected {
            if r.code == "EXTERNAL_CONTACT_HASH_MISMATCH"
                || r.code == "EXTERNAL_CONTACT_DELETE_HASH_MISMATCH" {
                hadHashMismatch = true
                break
            }
        }
        if hadHashMismatch {
            await setRecoveryFlag(reason: "hash_mismatch")
        }

        // Commit cursor ONLY if every batch had no rejections AND no
        // unconfirmed items.
        let canCommit = outcome.rejected.isEmpty && outcome.unconfirmed == 0
        if !canCommit {
            // Don't advance the cursor; the next tick replays the
            // same events. cache.discardPendingRemovals runs via the
            // defer.
            if !outcome.rejected.isEmpty {
                logger.warning("icloud tick: per-event rejections; holding cursor", metadata: [
                    "rejected": .public(String(outcome.rejected.count)),
                ])
            }
            if outcome.unconfirmed > 0 {
                logger.warning("icloud tick: publish unconfirmed; holding cursor", metadata: [
                    "unconfirmed": .public(String(outcome.unconfirmed)),
                ])
            }
            return
        }

        let newCursorB64 = newCursorBytes.base64EncodedString()
        do {
            try await piClient.commitCursor(
                auth: auth,
                source: id.rawValue,
                cursor: newCursorB64,
                baseCursor: cursorState.cursor,
                cursorEpoch: cursorState.cursorEpoch,
                backfillComplete: true)
        } catch {
            logger.warning("icloud tick: cursor commit failed", metadata: [
                "error": .private(String(describing: error)),
            ])
            return
        }

        // Cursor committed successfully: finalize cache removals.
        do {
            try await cache.commitPendingRemovals()
            stagedRemovalsCommitted = true
        } catch {
            // The cursor is already committed but the cache file
            // write failed — log + leave the live map state intact
            // (the in-memory removals are still in the actor; next
            // tick's defer will discard them). The Pi already has
            // the deletes; replays are idempotent.
            logger.warning("icloud tick: cache.commitPendingRemovals failed post-cursor", metadata: [
                "error": .private(String(describing: error)),
            ])
        }

        // Clear recovery flag if we entered via the recovery path
        // (or had previously set it but the publish succeeded).
        if entryRoute == .recovery || hadHashMismatch == false {
            await clearRecoveryFlagIfPresent()
        }

        // Bump lastPushedAt only when we actually committed.
        let pushedAt = clock()
        await updatePushed(at: pushedAt)
        await healthRegistry.update(id, currentHealthSnapshot(
            enabled: true, lastScheduled: tickStart, lastPushed: pushedAt))

        logger.debug("icloud tick: complete", metadata: [
            "route": .public(String(describing: entryRoute)),
            "emitted": .public(String(publishItems.count)),
            "accepted": .public(String(outcome.accepted)),
            "duplicate": .public(String(outcome.duplicate)),
        ])
    }

    // MARK: - tick routing

    private enum TickRoute {
        case firstRun
        case delta
        case recovery
    }

    // MARK: - walkers

    private func firstRunWalk(
        allowSet: Set<String>
    ) async throws -> ([ContactChange], Data) {
        // Capture the token FIRST (per plan D-JC2 + Codex r1 P1-1).
        // Any contact edited during the snapshot fetch shows up on
        // the next delta tick + dedup-absorbs.
        let newToken = try reader.currentToken()
        let records = try reader.fullFetch(containerIdentifiers: Array(allowSet))
        var events: [ContactChange] = []
        for r in records {
            // Defense-in-depth allowlist filter (the reader already
            // scopes by container).
            if !allowSet.contains(r.containerIdentifier) {
                continue
            }
            events.append(.add(r))
        }
        return (events, newToken)
    }

    private func deltaWalk(
        token: Data,
        allowSet: Set<String>
    ) async throws -> ([ContactChange], Data, Bool) {
        let result = try reader.changeHistory(from: token)
        var hadUnknown = false
        var events: [ContactChange] = []
        for event in result.events {
            switch event {
            case .add(let r), .update(let r):
                if allowSet.contains(r.containerIdentifier) {
                    events.append(event)
                }
            case .delete:
                // Deletes don't carry container info; emit
                // unconditionally. The Pi tombstones by source_id;
                // if the identifier was never allowlisted it's a
                // no-op on the Pi side.
                events.append(event)
            case .unknown(let desc):
                hadUnknown = true
                logger.warning("icloud tick: unknown change-history event", metadata: [
                    "event": .public(desc),
                ])
                // Stop processing further events — fail-closed.
                return ([], token, true)
            }
        }
        return (events, result.newToken, hadUnknown)
    }

    private func recoveryWalk(
        allowSet: Set<String>
    ) async throws -> ([ContactChange], Data) {
        let known = try await piClient.knownIDs(auth: auth, source: id.rawValue)
        return try await recoveryWalkUsingKnownIDs(
            allowSet: allowSet, known: known)
    }

    private func recoveryWalkUsingKnownIDs(
        allowSet: Set<String>,
        known: KnownIDsData
    ) async throws -> ([ContactChange], Data) {
        let newToken = try reader.currentToken()
        let records = try reader.fullFetch(containerIdentifiers: Array(allowSet))

        // Pi-side known entity IDs + their last_content_hash. The
        // source_id wire shape is `<entity_id>@<hash>` for upserts
        // and `<entity_id>@deleted@<prior>` for deletes; we strip
        // the suffix to recover the entity_id.
        var piKnownByEntity: [String: String?] = [:]
        for entry in known.ids {
            let entityID = Self.entityIDFromSourceID(entry.sourceID)
            piKnownByEntity[entityID] = entry.lastContentHash
        }

        var events: [ContactChange] = []
        var scannedIDs: Set<String> = []
        for r in records {
            if !allowSet.contains(r.containerIdentifier) {
                continue
            }
            scannedIDs.insert(r.identifier)
            events.append(.update(r))
        }
        // Tombstone Pi-known entities the scan no longer sees.
        for (entityID, lastHashOpt) in piKnownByEntity {
            if !scannedIDs.contains(entityID) {
                // Cache that hash so the delete source_id picks it up.
                if let lastHash = lastHashOpt {
                    try await cache.applyUpdates([entityID: lastHash])
                }
                events.append(.delete(identifier: entityID))
            }
        }
        return (events, newToken)
    }

    /// Recover the entity_id from a source_id of the form
    /// `<entity_id>@<hash>` or `<entity_id>@deleted@<prior_hash>`.
    /// Returns the substring before the FIRST `@` so any
    /// hyphen/underscore-containing entity IDs survive.
    static func entityIDFromSourceID(_ sourceID: String) -> String {
        if let atIndex = sourceID.firstIndex(of: "@") {
            return String(sourceID[..<atIndex])
        }
        return sourceID
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
            logger.warning("icloud tick: lastScheduledAt mutate failed", metadata: [
                "error": .private(String(describing: error)),
            ])
        }
    }

    private func updatePushed(at date: Date) async {
        do {
            try await mutator.mutate { state in
                var src = state.sources[self.id.rawValue] ?? SourceState()
                src.lastPushedAt = date
                state.sources[self.id.rawValue] = src
            }
        } catch {
            logger.warning("icloud tick: lastPushedAt mutate failed", metadata: [
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
            logger.warning("icloud tick: setRecoveryFlag failed", metadata: [
                "error": .private(String(describing: error)),
            ])
        }
    }

    private func clearRecoveryFlagIfPresent() async {
        do {
            try await mutator.mutate { state in
                var src = state.sources[self.id.rawValue] ?? SourceState()
                if let lastError = src.lastError, lastError.hasPrefix("recovery_requested:") {
                    src.lastError = nil
                    src.lastErrorAt = nil
                    state.sources[self.id.rawValue] = src
                }
            }
        } catch {
            logger.warning("icloud tick: clearRecoveryFlag failed", metadata: [
                "error": .private(String(describing: error)),
            ])
        }
    }

    private func markUnhealthy(reason: String) async {
        let snap = SourceHealthSnapshot(
            enabled: false,
            lastScheduledAt: clock(),
            lastError: reason,
            lastErrorAt: clock())
        await healthRegistry.update(id, snap)
        logger.warning("icloud tick: marked unhealthy", metadata: [
            "reason": .public(reason),
        ])
    }

    private func currentHealthSnapshot(
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

    // MARK: - cursor encoding

    /// Decode a base64-encoded cursor to raw bytes. An empty string
    /// (first run) or invalid bytes return nil.
    private func decodeCursorBase64(_ s: String) -> Data? {
        if s.isEmpty { return nil }
        return Data(base64Encoded: s)
    }

    // MARK: - encoding helpers

    private func encodePayload(_ p: ExternalContactUpsertedPayload) throws -> Data {
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.withoutEscapingSlashes]
        return try encoder.encode(p)
    }

    private func encodeDeletedPayload(_ p: ExternalContactDeletedPayload) throws -> Data {
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.withoutEscapingSlashes]
        return try encoder.encode(p)
    }
}
