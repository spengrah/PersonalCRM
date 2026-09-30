// AnarlogHumansSourcePlugin reads every eligible session through the
// Anarlog CLI and syncs its participants as external contacts.
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

public actor AnarlogHumansSourcePlugin: DataSourcePlugin {
    public nonisolated let id: SourceID = .anarlogHumans
    public nonisolated let tickInterval: TimeInterval

    private let piClient: PiClient
    private let auth: PiAuth
    // nonisolated: read by the DataSourcePlugin extension's tick() from
    // a nonisolated context. Sound because all three are immutable lets
    // holding Sendable values (same pattern as `id`/`tickInterval`).
    public nonisolated let mutator: StateMutator
    private let publisher: AnarlogHumansPublisher
    private let configSource: AnarlogConfigSource
    private let makeCLIClient: @Sendable (_ cliPath: String?) -> any AnarlogCLIClient
    private let healthSink: any AnarlogHealthSink
    private let healthRegistry: SourceHealthRegistry
    public nonisolated let logger: LoggerProtocol
    public nonisolated let clock: @Sendable () -> Date

    init(
        tickInterval: TimeInterval = CRMMacAnarlogSource.humansTickInterval,
        piClient: PiClient,
        auth: PiAuth,
        mutator: StateMutator,
        publisher: AnarlogHumansPublisher,
        configSource: AnarlogConfigSource,
        makeCLIClient: @escaping @Sendable (_ cliPath: String?) -> any AnarlogCLIClient = CRMMacAnarlogSource.makeCLIClient,
        healthSink: any AnarlogHealthSink,
        healthRegistry: SourceHealthRegistry,
        logger: LoggerProtocol,
        clock: @escaping @Sendable () -> Date = { Date() }
    ) {
        self.tickInterval = tickInterval
        self.piClient = piClient
        self.auth = auth
        self.mutator = mutator
        self.publisher = publisher
        self.configSource = configSource
        self.makeCLIClient = makeCLIClient
        self.healthSink = healthSink
        self.healthRegistry = healthRegistry
        self.logger = logger
        self.clock = clock
    }

    public func performTick() async throws {
        try await runTick()
    }

    private func runTick() async throws {
        let tickStart = clock()
        await healthRegistry.update(
            id, healthSnapshot(enabled: true, lastScheduled: tickStart))

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
        guard let operatorPersonID = cfg.operatorPersonID else {
            await markUnhealthy("operator_person_id_unset")
            await healthSink.record(source: .anarlogHumans, outcome: .failed(.operatorPersonIDUnset))
            return
        }

        let client = makeCLIClient(cfg.cliPath)
        let listing: [AnarlogSessionListEntry]
        do {
            listing = try await client.listSessions()
        } catch let failure {
            await markUnhealthy("anarlog_cli_failed:\(String(describing: failure))")
            await healthSink.record(source: .anarlogHumans, outcome: .failed(failure))
            return
        }

        let newest = listing.map(\.createdAt).max()
        var records: [AnarlogSessionRecord] = []
        for entry in listing where AnarlogEligibility.isEligible(createdAt: entry.createdAt, now: tickStart) {
            let record: AnarlogSessionRecord?
            do {
                record = try await client.getSession(id: entry.id)
            } catch let failure {
                await markUnhealthy("anarlog_cli_failed:\(String(describing: failure))")
                await healthSink.record(source: .anarlogHumans, outcome: .failed(failure))
                return
            }
            if let record {
                records.append(record)
            }
        }
        await healthSink.record(source: .anarlogHumans, outcome: .clean(newestSessionCreatedAt: newest))

        var people: [String: AnarlogParticipant] = [:]
        for record in records {
            for participant in record.participants(excludingOperator: operatorPersonID) {
                if people[participant.personID] == nil {
                    people[participant.personID] = participant
                }
            }
        }

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
        let decoded = AnarlogHumansCursorCodec.decodeOrNil(cursorState.cursor)
        let state = try? await mutator.read()
        let priorError = state?.sources[id.rawValue]?.lastError ?? ""
        let recoveryRequested = priorError.hasPrefix("recovery_requested:")
        let resendAll = decoded == nil || recoveryRequested

        var desiredCursor: [String: AnarlogHumansCursorEntry] = [:]
        var publishItems: [AnarlogHumansPublishItem] = []
        var encodeFailedCount = 0
        var payloadTooLargeCount = 0

        for participant in people.values.sorted(by: { $0.personID < $1.personID }) {
            let personID = participant.personID
            let prior = decoded?[personID]
            let payload = AnarlogHumansPayloadShaping.shape(
                participant: participant, hostID: auth.hostID)
            let payloadBytes: Data
            do {
                let encoder = JSONEncoder()
                encoder.outputFormatting = [.withoutEscapingSlashes]
                payloadBytes = try encoder.encode(payload)
            } catch {
                encodeFailedCount += 1
                if let prior { desiredCursor[personID] = prior }
                continue
            }
            if payloadBytes.count > CRMMacAnarlogSource.maxPayloadBytes {
                payloadTooLargeCount += 1
                if let prior { desiredCursor[personID] = prior }
                continue
            }
            let hash: String
            do {
                hash = try ContentHasher.contentHash(for: payloadBytes)
            } catch {
                encodeFailedCount += 1
                if let prior { desiredCursor[personID] = prior }
                continue
            }

            desiredCursor[personID] = AnarlogHumansCursorEntry(recordHash: hash)
            if resendAll || prior == nil || prior?.recordHash != hash {
                publishItems.append(AnarlogHumansPublishItem(
                    sourceID: AnarlogSourceIDBuilder.upsertSourceID(entityID: personID, payloadHash: hash),
                    kind: "external_contact.upserted",
                    payloadBytes: payloadBytes))
            }
        }

        let outcome = await publisher.publish(items: publishItems)
        let hadHashMismatch = outcome.rejected.contains { rejection in
            AnarlogHumansPublisher.recoveryCodes.contains(rejection.code)
        }
        if hadHashMismatch {
            await setRecoveryFlag(reason: "hash_mismatch")
        }

        let cleanBatch = outcome.rejected.isEmpty && outcome.unconfirmed == 0
        if !cleanBatch {
            var errorMessage = "publish_held_due_to_rejections (\(outcome.rejected.count) rejected"
            if outcome.unconfirmed > 0 {
                errorMessage += ", \(outcome.unconfirmed) unconfirmed"
            }
            errorMessage += ")"
            if encodeFailedCount + payloadTooLargeCount > 0 {
                errorMessage += "; anomalies encode_failed=\(encodeFailedCount) payload_too_large=\(payloadTooLargeCount)"
            }
            await recordLastError(errorMessage)
            return
        }

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

        let pushedAt = clock()
        let anomalyMsg: String? = encodeFailedCount + payloadTooLargeCount > 0
            ? "anomalies encode_failed=\(encodeFailedCount) payload_too_large=\(payloadTooLargeCount)"
            : nil
        await commitCleanTick(
            cursor: desiredCursorBytes,
            cursorEpoch: cursorState.cursorEpoch,
            pushedAt: pushedAt,
            anomalyError: anomalyMsg,
            wasRecovery: recoveryRequested)

        await healthRegistry.update(
            id, healthSnapshot(
                enabled: true,
                lastScheduled: tickStart,
                lastPushed: pushedAt))

        logger.debug("anarlog_humans tick: complete", metadata: [
            "route": .public(resendAll ? "resend_all" : "delta"),
            "emitted": .public(String(publishItems.count)),
            "accepted": .public(String(outcome.accepted)),
            "duplicate": .public(String(outcome.duplicate)),
        ])
    }

    // MARK: - state mutators

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
