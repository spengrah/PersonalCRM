// AnarlogHumansSourcePlugin — actor that orchestrates one
// anarlog_humans tick.
//
// Per-tick flow:
//   1. Bump state.sources[anarlog_humans].lastScheduledAt = NOW.
//   2. Load config; mark not_configured / path_missing /
//      humans_subdir_missing / files_folders_permission_denied as
//      appropriate.
//   3. GET /sync/anarlog_humans/cursor.
//   4. Check recovery flag (state.lastError starts with
//      "recovery_requested:").
//   5. Route selection:
//        - recovery flag set → .recovery (consults /known-ids)
//        - empty/malformed cursor → .bootstrapViaKnownIDs (consults
//          /known-ids); demoted to .firstRun if /known-ids returns
//          empty
//        - else → .delta (uses prior cursor as tombstone basis)
//   6. Walk the directory, parse each file, build:
//        - seenPhysicalUUIDs (every UUID actually on disk this scan)
//        - desiredCursor (UUIDs we have a clean cursor entry for)
//        - publishItems
//        Per-file failure (parse_failed / payload_too_large):
//          - prior cursor entry carried forward verbatim, OR
//          - synthesized from /known-ids on bootstrap/recovery, OR
//          - skipped if neither (no future deterministic delete possible)
//        Critical invariant: tombstoneBasis MINUS seenPhysicalUUIDs
//        is what fires deletes, NOT (basis MINUS desiredCursor). A
//        previously-cursor'd file that became malformed this tick is
//        STILL in seenPhysicalUUIDs, so its cursor entry is preserved
//        and no delete event is emitted.
//   7. Emit tombstones for tombstoneBasis - seenPhysicalUUIDs.
//   8. Publish via AnarlogHumansPublisher.
//   9. Set recovery flag on hash-mismatch.
//  10. Commit cursor ONLY when rejected.isEmpty && unconfirmed == 0.
//  11. On clean commit: clear recovery flag if route was .recovery;
//      record per-tick anomalies (parse_failed / payload_too_large
//      counts) in lastError EVEN ON SUCCESS so they're visible via
//      `crm-mac status` without conflating with tick-aborting errors.
import Foundation
import CryptoKit
import CRMMacCore
import CRMMacPiClient

/// Source of the AnarlogConfig. Production wraps ConfigStore; tests
/// inject a closure to return canned values.
public protocol AnarlogConfigSource: Sendable {
    func load() throws -> AnarlogConfig?
}

public struct AnarlogConfigStoreSource: AnarlogConfigSource {
    private let store: ConfigStore
    public init(store: ConfigStore) { self.store = store }
    public func load() throws -> AnarlogConfig? {
        try store.loadAnarlogConfig()
    }
}

/// Filesystem abstraction the plugin uses for directory walking +
/// file reads. The production impl wraps Foundation FileManager +
/// Data(contentsOf:); tests inject a stub that returns canned bytes.
public protocol AnarlogFilesystem: Sendable {
    /// True when `path` exists (file or directory).
    func exists(_ path: String) -> Bool
    /// True when `path` is a readable directory. False on permission
    /// denied OR on non-directory.
    func isReadableDirectory(_ path: String) -> Bool
    /// List immediate children of `dir` (filenames only, no path
    /// prefix). Throws AnarlogFilesystemError.permissionDenied on
    /// EACCES so the plugin can surface
    /// `files_folders_permission_denied` to Doctor + status.
    func listDirectory(_ dir: String) throws -> [String]
    /// Read raw file bytes. Throws on EACCES or any other underlying
    /// error.
    func readFile(_ path: String) throws -> Data
    /// File modification time as Date, or nil if unavailable. Used
    /// as cursor diagnostic only (NOT for skip decisions).
    func mtime(_ path: String) -> Date?
}

public enum AnarlogFilesystemError: Error, Equatable, Sendable {
    case permissionDenied(String)
    case ioError(String)
}

public final class ProductionAnarlogFilesystem: AnarlogFilesystem {
    public init() {}

    public func exists(_ path: String) -> Bool {
        FileManager.default.fileExists(atPath: path)
    }

    public func isReadableDirectory(_ path: String) -> Bool {
        var isDir: ObjCBool = false
        let exists = FileManager.default.fileExists(atPath: path, isDirectory: &isDir)
        guard exists, isDir.boolValue else { return false }
        return FileManager.default.isReadableFile(atPath: path)
    }

    public func listDirectory(_ dir: String) throws -> [String] {
        do {
            return try FileManager.default.contentsOfDirectory(atPath: dir)
        } catch let nsErr as NSError where nsErr.domain == NSCocoaErrorDomain &&
            (nsErr.code == NSFileReadNoPermissionError ||
             nsErr.code == NSFileReadCorruptFileError) {
            throw AnarlogFilesystemError.permissionDenied(dir)
        } catch let posixErr as POSIXError where posixErr.code == .EACCES {
            throw AnarlogFilesystemError.permissionDenied(dir)
        } catch {
            throw AnarlogFilesystemError.ioError(String(describing: error))
        }
    }

    public func readFile(_ path: String) throws -> Data {
        do {
            return try Data(contentsOf: URL(fileURLWithPath: path))
        } catch let nsErr as NSError where nsErr.domain == NSCocoaErrorDomain &&
            nsErr.code == NSFileReadNoPermissionError {
            throw AnarlogFilesystemError.permissionDenied(path)
        } catch {
            throw AnarlogFilesystemError.ioError(String(describing: error))
        }
    }

    public func mtime(_ path: String) -> Date? {
        guard let attrs = try? FileManager.default.attributesOfItem(atPath: path) else {
            return nil
        }
        return attrs[.modificationDate] as? Date
    }
}

/// Inputs the plugin's runTick assembles for the publish phase.
struct AnarlogTickRoute: Equatable {
    let kind: Kind
    enum Kind: Equatable {
        case firstRun
        case bootstrapViaKnownIDs
        case delta
        case recovery
    }
}

public actor AnarlogHumansSourcePlugin: SourcePlugin {
    public nonisolated let id: SourceID = .anarlogHumans
    public nonisolated let tickInterval: TimeInterval

    private let piClient: PiClient
    private let auth: PiAuth
    private let mutator: StateMutator
    private let publisher: AnarlogHumansPublisher
    private let filesystem: AnarlogFilesystem
    private let configSource: AnarlogConfigSource
    private let healthRegistry: SourceHealthRegistry
    private let logger: LoggerProtocol
    private let clock: @Sendable () -> Date

    public init(
        tickInterval: TimeInterval = CRMMacAnarlogSource.humansTickInterval,
        piClient: PiClient,
        auth: PiAuth,
        mutator: StateMutator,
        publisher: AnarlogHumansPublisher,
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
        try await runTick()
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
        guard let cfg = config, cfg.humansEnabled else {
            await markUnhealthy("not_configured")
            return
        }

        let rootExpanded = AnarlogPathResolver.expand(cfg.rootPath).path
        guard filesystem.exists(rootExpanded) else {
            await markUnhealthy("path_missing")
            return
        }
        let humansPath = AnarlogPathResolver.humansDir(rootPath: cfg.rootPath).path
        guard filesystem.exists(humansPath) else {
            await markUnhealthy("humans_subdir_missing")
            return
        }
        guard filesystem.isReadableDirectory(humansPath) else {
            await markUnhealthy("files_folders_permission_denied")
            return
        }

        // Cursor fetch.
        let cursorState: SourceCursorState
        do {
            cursorState = try await piClient.getCursor(auth: auth, source: id.rawValue)
        } catch {
            logger.warning("anarlog_humans tick: cursor fetch failed", metadata: [
                "error": .private(String(describing: error)),
            ])
            await markUnhealthy("cursor_fetch_failed")
            return
        }
        let decodedOpt = AnarlogHumansCursorCodec.decodeOrNil(cursorState.cursor)

        // Recovery flag check.
        let state = try? await mutator.read()
        let priorError = state?.sources[id.rawValue]?.lastError ?? ""
        let recoveryRequested = priorError.hasPrefix("recovery_requested:")

        // Route selection.
        var entryRoute: AnarlogTickRoute.Kind
        let decoded = decodedOpt ?? [:]
        if recoveryRequested {
            entryRoute = .recovery
        } else if decodedOpt == nil {
            entryRoute = .bootstrapViaKnownIDs
        } else {
            entryRoute = .delta
        }

        // /known-ids fetch (bootstrap + recovery only).
        var knownIDs: KnownIDsData?
        if entryRoute == .bootstrapViaKnownIDs || entryRoute == .recovery {
            do {
                knownIDs = try await piClient.knownIDs(auth: auth, source: id.rawValue)
            } catch {
                logger.warning("anarlog_humans tick: known_ids fetch failed", metadata: [
                    "error": .private(String(describing: error)),
                ])
                await markUnhealthy("known_ids_fetch_failed")
                return
            }
            if entryRoute == .bootstrapViaKnownIDs && (knownIDs?.ids.isEmpty ?? true) {
                // Pi has no rows for this source → demote to firstRun
                // so we skip the tombstone scan entirely.
                entryRoute = .firstRun
                knownIDs = nil
            }
        }

        // /known-ids lookup by entity ID (after stripping the `@hash`
        // suffix from source_id). Used by parse_failed /
        // payload_too_large carry-forward synthesis on bootstrap /
        // recovery routes.
        var knownByEntityID: [String: String?] = [:]
        if let kids = knownIDs {
            for entry in kids.ids {
                let entityID = Self.entityIDFromSourceID(entry.sourceID)
                knownByEntityID[entityID] = entry.lastContentHash
            }
        }

        // Full inventory scan.
        var seenPhysicalUUIDs: Set<String> = []
        var desiredCursor: [String: AnarlogHumansCursorEntry] = [:]
        var publishItems: [AnarlogHumansPublishItem] = []
        var parseFailedCount = 0
        var payloadTooLargeCount = 0

        let entries: [String]
        do {
            entries = try filesystem.listDirectory(humansPath)
        } catch AnarlogFilesystemError.permissionDenied {
            await markUnhealthy("files_folders_permission_denied")
            return
        } catch {
            await markUnhealthy("dir_list_failed:\(error)")
            return
        }

        for entry in entries {
            // Skip-list (e.g. .DS_Store, AGENTS.md, self-human file).
            if CRMMacAnarlogSource.humanSkipEntries.contains(entry) {
                continue
            }
            // Filename shape: `<uuid>.md`. Anything else is silently
            // ignored.
            guard entry.hasSuffix(".md") else { continue }
            let nameWithoutSuffix = String(entry.dropLast(3))
            guard let canonicalUUID = AnarlogUUIDValidator.canonicalize(nameWithoutSuffix) else {
                continue
            }
            // Defense-in-depth: also skip the self-UUID even if it's
            // not in the static skip list (handles uppercase variants).
            if canonicalUUID == CRMMacAnarlogSource.selfHumanUUID {
                continue
            }
            seenPhysicalUUIDs.insert(canonicalUUID)

            let filePath = (humansPath as NSString).appendingPathComponent(entry)
            let fileBytes: Data
            do {
                fileBytes = try filesystem.readFile(filePath)
            } catch AnarlogFilesystemError.permissionDenied {
                // A single-file permission denial doesn't bring down
                // the whole tick; carry forward + continue.
                parseFailedCount += 1
                logger.warning("anarlog_humans tick: read_failed", metadata: [
                    "uuid": .private(canonicalUUID),
                ])
                carryForward(
                    uuid: canonicalUUID,
                    fileBytesHash: "",
                    mtime: filesystem.mtime(filePath),
                    prior: decoded[canonicalUUID],
                    entryRoute: entryRoute,
                    knownByEntityID: knownByEntityID,
                    desiredCursor: &desiredCursor)
                continue
            } catch {
                parseFailedCount += 1
                logger.warning("anarlog_humans tick: read_failed", metadata: [
                    "uuid": .private(canonicalUUID),
                    "error": .private(String(describing: error)),
                ])
                carryForward(
                    uuid: canonicalUUID,
                    fileBytesHash: "",
                    mtime: filesystem.mtime(filePath),
                    prior: decoded[canonicalUUID],
                    entryRoute: entryRoute,
                    knownByEntityID: knownByEntityID,
                    desiredCursor: &desiredCursor)
                continue
            }
            let fileBytesHash = AnarlogFileHash.sha256Hex(fileBytes)
            let prior = decoded[canonicalUUID]
            let contentChanged = (prior == nil) || (prior!.contentHash != fileBytesHash)

            guard let record = AnarlogHumanFrontmatterParser.parse(
                uuid: canonicalUUID, fileBytes: fileBytes) else {
                parseFailedCount += 1
                logger.warning("anarlog_humans tick: parse_failed", metadata: [
                    "uuid": .private(canonicalUUID),
                ])
                carryForward(
                    uuid: canonicalUUID,
                    fileBytesHash: fileBytesHash,
                    mtime: filesystem.mtime(filePath),
                    prior: prior,
                    entryRoute: entryRoute,
                    knownByEntityID: knownByEntityID,
                    desiredCursor: &desiredCursor)
                continue
            }

            // Shape + size check.
            let payload = AnarlogHumansPayloadShaping.shape(
                record: record, hostID: auth.hostID)
            let payloadBytes: Data
            do {
                let encoder = JSONEncoder()
                encoder.outputFormatting = [.withoutEscapingSlashes]
                payloadBytes = try encoder.encode(payload)
            } catch {
                parseFailedCount += 1
                logger.warning("anarlog_humans tick: payload_encode_failed", metadata: [
                    "uuid": .private(canonicalUUID),
                    "error": .private(String(describing: error)),
                ])
                carryForward(
                    uuid: canonicalUUID,
                    fileBytesHash: fileBytesHash,
                    mtime: filesystem.mtime(filePath),
                    prior: prior,
                    entryRoute: entryRoute,
                    knownByEntityID: knownByEntityID,
                    desiredCursor: &desiredCursor)
                continue
            }
            if payloadBytes.count > CRMMacAnarlogSource.maxPayloadBytes {
                payloadTooLargeCount += 1
                logger.error("anarlog_humans tick: payload_too_large", metadata: [
                    "uuid": .private(canonicalUUID),
                    "size": .public(String(payloadBytes.count)),
                ])
                carryForward(
                    uuid: canonicalUUID,
                    fileBytesHash: fileBytesHash,
                    mtime: filesystem.mtime(filePath),
                    prior: prior,
                    entryRoute: entryRoute,
                    knownByEntityID: knownByEntityID,
                    desiredCursor: &desiredCursor)
                continue
            }

            // Decide whether to emit an upsert + which payloadHash to
            // store in the cursor entry. Recovery and bootstrap
            // routes ALWAYS re-emit (Pi event-log dedups by
            // source_id); delta route only when contentChanged.
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
                    // Carry prior payload hash forward — the prior
                    // entry has a payloadHash; if somehow it's empty
                    // (legacy on the first tick after this code
                    // ships), recompute one this tick so the next
                    // delete is deterministic.
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
                publishItems.append(AnarlogHumansPublishItem(
                    sourceID: sourceID,
                    kind: "external_contact.upserted",
                    payloadBytes: payloadBytes))
            }

            desiredCursor[canonicalUUID] = AnarlogHumansCursorEntry(
                contentHash: fileBytesHash,
                payloadHash: payloadHash,
                mtimeEpochMs: filesystem.mtime(filePath).map { Int64($0.timeIntervalSince1970 * 1000) })
        }

        // Tombstone basis selection.
        var tombstoneBasis: [AnarlogTombstoneBasisEntry] = []
        switch entryRoute {
        case .firstRun:
            tombstoneBasis = []
        case .delta:
            for (uuid, entry) in decoded {
                tombstoneBasis.append(AnarlogTombstoneBasisEntry(
                    uuid: uuid,
                    priorPayloadHash: entry.payloadHash))
            }
        case .bootstrapViaKnownIDs, .recovery:
            for (uuid, lastHash) in knownByEntityID {
                tombstoneBasis.append(AnarlogTombstoneBasisEntry(
                    uuid: uuid,
                    priorPayloadHash: lastHash))
            }
        }

        // P0 invariant: tombstone keyed on physical presence, NOT
        // desiredCursor. A previously-cursor'd UUID whose file became
        // malformed this tick is still in seenPhysicalUUIDs and stays.
        for basis in tombstoneBasis {
            if seenPhysicalUUIDs.contains(basis.uuid) { continue }
            let deleted = AnarlogHumansPayloadShaping.shapeDeleted(
                entityID: basis.uuid, hostID: auth.hostID)
            let deletedBytes: Data
            do {
                let encoder = JSONEncoder()
                encoder.outputFormatting = [.withoutEscapingSlashes]
                deletedBytes = try encoder.encode(deleted)
            } catch {
                logger.warning("anarlog_humans tick: delete_encode_failed", metadata: [
                    "uuid": .private(basis.uuid),
                    "error": .private(String(describing: error)),
                ])
                continue
            }
            let sourceID = AnarlogSourceIDBuilder.deleteSourceID(
                entityID: basis.uuid,
                priorPayloadHash: basis.priorPayloadHash)
            publishItems.append(AnarlogHumansPublishItem(
                sourceID: sourceID,
                kind: "external_contact.deleted",
                payloadBytes: deletedBytes))
        }

        // Publish.
        let outcome = await publisher.publish(items: publishItems)
        let hadHashMismatch = outcome.rejected.contains { rej in
            AnarlogHumansPublisher.recoveryCodes.contains(rej.code)
        }
        if hadHashMismatch {
            await setRecoveryFlag(reason: "hash_mismatch")
        }

        let cleanBatch = outcome.rejected.isEmpty && outcome.unconfirmed == 0
        if !cleanBatch {
            // Hold cursor; record reason + per-tick anomalies.
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

        // Cursor commit.
        let desiredCursorBytes: String
        do {
            desiredCursorBytes = try AnarlogHumansCursorCodec.encode(desiredCursor)
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
            logger.warning("anarlog_humans tick: cursor commit failed", metadata: [
                "error": .private(String(describing: error)),
            ])
            return
        }

        // Clean commit — write cursor + lastPushedAt + per-tick
        // anomaly summary (or clear lastError if truly clean).
        let pushedAt = clock()
        let anomalyMsg: String? = (parseFailedCount > 0 || payloadTooLargeCount > 0)
            ? "anomalies parse_failed=\(parseFailedCount) payload_too_large=\(payloadTooLargeCount)"
            : nil
        await commitCleanTick(
            cursor: desiredCursorBytes,
            cursorEpoch: cursorState.cursorEpoch,
            pushedAt: pushedAt,
            anomalyError: anomalyMsg,
            wasRecovery: entryRoute == .recovery)

        await healthRegistry.update(
            id, healthSnapshot(
                enabled: true,
                lastScheduled: tickStart,
                lastPushed: pushedAt))

        logger.debug("anarlog_humans tick: complete", metadata: [
            "route": .public(String(describing: entryRoute)),
            "emitted": .public(String(publishItems.count)),
            "accepted": .public(String(outcome.accepted)),
            "duplicate": .public(String(outcome.duplicate)),
        ])
    }

    // MARK: - per-file failure helper

    /// Carry forward the cursor entry for a present-but-failed-shape
    /// file:
    ///   1. If we have a prior cursor entry, keep it verbatim — the
    ///      file is still physically present so it stays in
    ///      seenPhysicalUUIDs and never tombstones.
    ///   2. Else if we're on bootstrap / recovery and /known-ids has
    ///      a row for this entity, synthesize a cursor entry using
    ///      knownIDs.lastContentHash as the payloadHash (or the
    ///      "unknown" sentinel when the Pi row has no last hash).
    ///   3. Else no cursor entry is written — the next scan re-evaluates.
    ///      Important: NOT writing an entry here is fine because the
    ///      file IS in seenPhysicalUUIDs (so it can't be tombstoned)
    ///      AND no prior knowledge exists to construct a deterministic
    ///      future delete (so writing a placeholder would mislead).
    private func carryForward(
        uuid: String,
        fileBytesHash: String,
        mtime: Date?,
        prior: AnarlogHumansCursorEntry?,
        entryRoute: AnarlogTickRoute.Kind,
        knownByEntityID: [String: String?],
        desiredCursor: inout [String: AnarlogHumansCursorEntry]
    ) {
        if let prior {
            desiredCursor[uuid] = prior
            return
        }
        if entryRoute == .bootstrapViaKnownIDs || entryRoute == .recovery,
           let knownLastHash = knownByEntityID[uuid] {
            // The lookup can return Optional<Optional<String>> — the
            // outer Some confirms /known-ids has the row, the inner
            // Optional carries the last_content_hash which is itself
            // nullable per the KnownContactID schema.
            let payloadHash = knownLastHash ?? "unknown"
            desiredCursor[uuid] = AnarlogHumansCursorEntry(
                contentHash: fileBytesHash,
                payloadHash: payloadHash,
                mtimeEpochMs: mtime.map { Int64($0.timeIntervalSince1970 * 1000) })
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
            logger.warning("anarlog_humans tick: lastScheduledAt mutate failed", metadata: [
                "error": .private(String(describing: error)),
            ])
        }
    }

    private func commitCleanTick(
        cursor: String,
        cursorEpoch: Int64,
        pushedAt: Date,
        anomalyError: String?,
        wasRecovery: Bool
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
                    // Truly clean: clear stale error (incl. cleared
                    // recovery flag).
                    src.lastError = nil
                    src.lastErrorAt = nil
                }
                state.sources[self.id.rawValue] = src
            }
        } catch {
            logger.warning("anarlog_humans tick: commitCleanTick mutate failed", metadata: [
                "error": .private(String(describing: error)),
            ])
        }
        // wasRecovery is consumed by the lastError-clearing branch
        // above (no separate flag-clear needed since lastError IS the
        // flag).
        _ = wasRecovery
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
            logger.warning("anarlog_humans tick: setRecoveryFlag failed", metadata: [
                "error": .private(String(describing: error)),
            ])
        }
    }

    private func recordLastError(_ msg: String) async {
        do {
            try await mutator.mutate { state in
                var src = state.sources[self.id.rawValue] ?? SourceState()
                // Don't stomp on a recovery_requested flag that was
                // just set this tick (hash mismatch) — append instead.
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
            logger.warning("anarlog_humans tick: recordLastError failed", metadata: [
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
        // Also persist to state.json so `crm-mac status` (which reads
        // state.json, NOT heartbeat) sees the unhealthy state. Don't
        // stomp recovery flags.
        await recordLastError(reason)
        logger.warning("anarlog_humans tick: marked unhealthy", metadata: [
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

    /// Recover the entity_id from a source_id of the form
    /// `<entity_id>@<hash>` or `<entity_id>@deleted@<prior_hash>`.
    static func entityIDFromSourceID(_ sourceID: String) -> String {
        if let atIndex = sourceID.firstIndex(of: "@") {
            return String(sourceID[..<atIndex])
        }
        return sourceID
    }
}

// MARK: - File-bytes hashing helper

/// Lowercase-hex SHA-256 of raw bytes. Distinct from
/// `ContentHasher.contentHash(for:)` which does JCS canonicalization
/// for the payload-hash recipe; this is the file-bytes hash that
/// drives change detection only (file-bytes hash and payload hash
/// are two distinct hash concepts).
/// Lives in this target so CRMMacCore stays out of the surface area
/// for the anarlog source.
public enum AnarlogFileHash {
    public static func sha256Hex(_ data: Data) -> String {
        let digest = SHA256.hash(data: data)
        return digest.map { String(format: "%02x", $0) }.joined()
    }
}
