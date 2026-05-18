// ICloudContactsSourcePlugin — actor that orchestrates one
// icloud_contacts tick.
//
// Per-tick flow:
//   1.  Stamp state.sources[icloud_contacts].lastScheduledAt = NOW
//       so Doctor can read it as the staleness floor even on a
//       quiet tick.
//   2.  Authorization probe: .denied/.restricted/.notDetermined →
//       mark unhealthy + return (no cursor change, no prompt — the
//       daemon runs headless; install is the only caller that
//       prompts).
//   3.  Load CNContainer allowlist from ICloudContactsConfig.
//       Empty list → mark unhealthy with no_containers_configured.
//   4.  Fetch the Pi-side cursor for the source.
//   5.  Check the recovery flag (state.lastError startsWith
//       "recovery_requested:"). If set → recovery path.
//   6.  First-run path (empty cursor AND no recovery flag AND
//       /known-ids returns empty):
//         a. Capture currentToken BEFORE the snapshot read so any
//            edit during the snapshot window is naturally caught on
//            the next delta tick (and dedup-absorbed if a no-op).
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
//            mark unhealthy, abort. Silently dropping unknown
//            event subtypes risks losing real updates.
//         c. For each .add/.update/.delete:
//              - filter by allowlist (delete events bypass the
//                filter — they don't carry container info; emit
//                unconditionally and let the Pi tombstone by
//                source_id).
//              - apply cache changes IN EVENT ORDER so a same-tick
//                `.delete X` followed by `.update X` ends with the
//                fresh hash in the live map and the staged removal
//                cancelled before commit.
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
//   9.  Publish in batches via ICloudContactsPublisher. Any
//       hash-mismatch rejection aborts subsequent batches in this
//       tick and sets the recovery flag for the next tick.
//  10.  Commit cursor ONLY if every batch had no rejections AND
//       no unconfirmed items.
//  11.  Finalize cache: commitPendingRemovals on success;
//       discardPendingRemovals on any abort. The outer tick wraps
//       runTick() in a do/catch envelope so the discard fires
//       synchronously before the next tick can stage its own set.
import Foundation
import CRMMacCore
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
        // Wrap the whole tick in a do/catch envelope so any abort
        // path (throw, early return, end-of-tick) deterministically
        // discards staged cache removals BEFORE the next tick can
        // start. A previous version used `defer { Task { ... } }`,
        // which dispatched discard asynchronously and could race
        // a back-to-back tick into wiping the next tick's freshly
        // staged removals.
        do {
            try await runTick()
        } catch {
            // Synchronous discard before re-throwing so the next
            // tick's stage starts from an empty in-memory set.
            await cache.discardPendingRemovals()
            throw error
        }
    }

    private func runTick() async throws {
        let tickStart = clock()
        // Bump lastScheduledAt for staleness diagnostics — a
        // quiet-but-healthy source bumps this every tick even when
        // no events are emitted. Doctor reads this AND lastPushedAt
        // to surface a meaningful staleness signal.
        await updateScheduled(at: tickStart)
        await healthRegistry.update(id, currentHealthSnapshot(
            enabled: true, lastScheduled: tickStart))

        // Local helper: any non-success exit point of this function
        // calls discardPendingRemovals BEFORE returning so the
        // actor's pendingRemovals set never leaks across ticks. The
        // success path skips this in favor of commitPendingRemovals.
        @Sendable func abortDiscard() async {
            await self.cache.discardPendingRemovals()
        }

        // Authorization probe.
        let authStatus = authAdapter.authorizationStatus()
        switch authStatus {
        case .authorized, .limited:
            break
        case .notDetermined, .denied, .restricted:
            await abortDiscard()
            await markUnhealthy(reason: "contacts_permission:\(authStatus)")
            return
        }

        // Load allowlist.
        let allowlist: [String]
        do {
            allowlist = try configSource.load()?.containers ?? []
        } catch {
            await abortDiscard()
            await markUnhealthy(reason: "config_load_failed")
            return
        }
        if allowlist.isEmpty {
            await abortDiscard()
            await markUnhealthy(reason: "no_containers_configured")
            return
        }
        let allowSet = Set(allowlist)

        // Fetch Pi cursor.
        let cursorState: SourceCursorState
        do {
            cursorState = try await piClient.getCursor(auth: auth, source: id.rawValue)
        } catch {
            logger.warning("icloud tick: cursor fetch failed", metadata: [
                "error": .private(String(describing: error)),
            ])
            await abortDiscard()
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
                await abortDiscard()
                await markUnhealthy(reason: "known_ids_fetch_failed:\(e)")
                return
            } catch {
                await abortDiscard()
                await markUnhealthy(reason: "first_run_failed:\(error)")
                return
            }
        } else {
            // Delta path.
            do {
                let (deltaEvents, deltaToken, hadUnknown) =
                    try await deltaWalk(token: cursorBytes!, allowSet: allowSet)
                if hadUnknown {
                    await abortDiscard()
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
                await abortDiscard()
                await markUnhealthy(reason: "delta_walk_failed")
                return
            }
        }

        // Build publish items + apply cache changes IN EVENT ORDER.
        // Iterating one event at a time preserves the same-tick
        // sequence semantics: a `.delete X` followed by `.update X`
        // first stages the removal (live map unchanged so the delete
        // event carries the correct prior hash), then the update
        // calls `applyUpdates([X: newHash])` which cancels the staged
        // removal AND writes the new hash. The end-of-tick commit
        // then drops X only if its final state is still "removed".
        var publishItems: [ICloudContactsPublishItem] = []
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
                do {
                    try await cache.applyUpdates([record.identifier: hash])
                } catch {
                    logger.warning("icloud tick: cache.applyUpdates failed", metadata: [
                        "error": .private(String(describing: error)),
                    ])
                    await abortDiscard()
                    await markUnhealthy(reason: "cache_write_failed")
                    return
                }
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
                await cache.stagePendingRemovals([identifier])
            case .unknown:
                // Belt-and-suspenders. The delta path's hadUnknown
                // branch returns before reaching this point; reaching
                // here would be a bug worth surfacing.
                logger.warning("icloud tick: unexpected .unknown event in build phase", metadata: [:])
                continue
            }
        }

        // Publish. The publisher aborts subsequent batches on the
        // first hash-mismatch rejection so the recovery flow can run
        // on the next tick without compounding the divergence.
        let outcome = await publisher.publish(items: publishItems)

        // A hash-mismatch in any batch must set the recovery flag
        // BEFORE we decide whether to advance the cursor — the flag
        // is what routes the next tick into the recovery walk.
        var hadHashMismatch = false
        for r in outcome.rejected {
            if ICloudContactsPublisher.recoveryCodes.contains(r.code) {
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
            // same events. Discard the staged removals so they don't
            // bleed into the replay (the live cache already carries
            // the prior hashes the .delete events need).
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
            await abortDiscard()
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
            await abortDiscard()
            return
        }

        // Cursor committed successfully: finalize cache removals.
        do {
            try await cache.commitPendingRemovals()
        } catch {
            // The cursor is already committed but the cache file
            // write failed — log + drop the staged set so the next
            // tick's stage starts clean. The Pi already has the
            // deletes; replays are idempotent.
            logger.warning("icloud tick: cache.commitPendingRemovals failed post-cursor", metadata: [
                "error": .private(String(describing: error)),
            ])
            await cache.discardPendingRemovals()
        }

        // Clear the recovery flag ONLY when this tick entered via
        // the recovery path AND completed without a hash mismatch.
        // Clearing on every successful tick would erase a recovery
        // request set by an earlier tick whose publish failed
        // post-stage (and never had a chance to run the recovery
        // walk).
        if entryRoute == .recovery && !hadHashMismatch {
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
        // Capture the change-history token BEFORE the snapshot
        // read. Any contact edited during the snapshot fetch is
        // naturally caught by the next delta tick (and absorbed by
        // the Pi's content-hash dedup if a no-op).
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
