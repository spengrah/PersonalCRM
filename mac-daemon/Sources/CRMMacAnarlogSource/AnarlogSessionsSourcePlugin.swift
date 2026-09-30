// AnarlogSessionsSourcePlugin reads Anarlog sessions through the CLI,
// shapes eligible records, and reconciles them with the Pi.
import Foundation
import CRMMacCore
import CRMMacOrphanNotifications
import CRMMacPiClient

public actor AnarlogSessionsSourcePlugin: DataSourcePlugin {
    public nonisolated let id: SourceID = .anarlogSessions
    public nonisolated let tickInterval: TimeInterval

    private let piClient: PiClient
    private let auth: PiAuth
    public nonisolated let mutator: StateMutator
    private let publisher: AnarlogSessionsPublisher
    private let configSource: AnarlogConfigSource
    private let makeCLIClient: @Sendable (_ cliPath: String?) -> any AnarlogCLIClient
    private let healthSink: any AnarlogHealthSink
    private let healthRegistry: SourceHealthRegistry
    private let orphanNotificationCenter: OrphanNotificationCenter?
    public nonisolated let logger: LoggerProtocol
    public nonisolated let clock: @Sendable () -> Date

    private var tickInFlight = false
    private var pendingRequest = false

    private enum Route {
        case firstRun
        case bootstrapViaKnownIDs
        case delta
        case recovery
    }

    init(
        tickInterval: TimeInterval = CRMMacAnarlogSource.sessionsSafetyTickInterval,
        piClient: PiClient,
        auth: PiAuth,
        mutator: StateMutator,
        publisher: AnarlogSessionsPublisher,
        configSource: AnarlogConfigSource,
        makeCLIClient: @escaping @Sendable (_ cliPath: String?) -> any AnarlogCLIClient = CRMMacAnarlogSource.makeCLIClient,
        healthSink: any AnarlogHealthSink,
        healthRegistry: SourceHealthRegistry,
        orphanNotificationCenter: OrphanNotificationCenter? = nil,
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
        self.orphanNotificationCenter = orphanNotificationCenter
        self.logger = logger
        self.clock = clock
    }

    public func performTick() async throws {
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
        await healthRegistry.update(
            id, healthSnapshot(enabled: true, lastScheduled: tickStart))

        let config: AnarlogConfig?
        do {
            config = try configSource.load()
        } catch {
            await markUnhealthy("config_load_failed:\(error)", at: tickStart)
            return
        }
        guard let cfg = config, cfg.sessionsEnabled else {
            await markUnhealthy("not_configured", at: tickStart)
            return
        }
        guard let operatorPersonID = cfg.operatorPersonID else {
            await markUnhealthy("operator_person_id_unset", at: tickStart)
            await healthSink.record(source: .anarlogSessions, outcome: .failed(.operatorPersonIDUnset))
            return
        }

        let client = makeCLIClient(cfg.cliPath)
        let listing: [AnarlogSessionListEntry]
        do {
            listing = try await client.listSessions()
        } catch {
            await reportCLIFailure(error, at: tickStart)
            return
        }

        let listingIDs = Set(listing.map(\.id))
        let newest = listing.map(\.createdAt).max()
        let eligible = listing.filter {
            AnarlogEligibility.isEligible(createdAt: $0.createdAt, now: tickStart)
        }
        var records: [(id: String, record: AnarlogSessionRecord)] = []
        for entry in eligible {
            do {
                if let record = try await client.getSession(id: entry.id) {
                    records.append((entry.id, record))
                }
            } catch {
                await reportCLIFailure(error, at: tickStart)
                return
            }
        }

        let cursorState: SourceCursorState
        do {
            cursorState = try await piClient.getCursor(auth: auth, source: id.rawValue)
        } catch {
            logger.warning("anarlog_sessions tick: cursor fetch failed", metadata: [
                "error": .private(String(describing: error)),
            ])
            await markUnhealthy("cursor_fetch_failed", at: tickStart)
            return
        }
        let decodedOpt = AnarlogSessionsCursorCodec.decodeOrNil(cursorState.cursor)
        let decoded = decodedOpt ?? [:]
        let state = try? await mutator.read()
        let priorError = state?.sources[id.rawValue]?.lastError ?? ""
        let recoveryRequested = priorError.hasPrefix("recovery_requested:")

        var route: Route
        if recoveryRequested {
            route = .recovery
        } else if decodedOpt == nil {
            route = .bootstrapViaKnownIDs
        } else {
            route = .delta
        }

        var knownIDs: KnownIDsData?
        if route == .bootstrapViaKnownIDs || route == .recovery {
            do {
                knownIDs = try await piClient.knownIDs(auth: auth, source: id.rawValue)
            } catch {
                logger.warning("anarlog_sessions tick: known_ids fetch failed", metadata: [
                    "error": .private(String(describing: error)),
                ])
                await markUnhealthy("known_ids_fetch_failed", at: tickStart)
                return
            }
            if route == .bootstrapViaKnownIDs && (knownIDs?.ids.isEmpty ?? true) {
                route = .firstRun
                knownIDs = nil
            }
        }

        var knownByEntityID: [String: String?] = [:]
        if let knownIDs {
            for entry in knownIDs.ids {
                knownByEntityID[Self.entityID(fromSourceID: entry.sourceID)] = entry.lastContentHash
            }
        }

        var desiredCursor: [String: AnarlogSessionsCursorEntry] = [:]
        var publishItems: [AnarlogSessionsPublishItem] = []
        var encodeFailedCount = 0
        var payloadTooLargeCount = 0
        var recordIDs: Set<String> = []

        for (sessionID, record) in records {
            recordIDs.insert(sessionID)
            let payload = AnarlogSessionsPayloadShaping.shape(
                record: record, operatorPersonID: operatorPersonID, hostID: auth.hostID)
            let payloadBytes: Data
            do {
                let encoder = JSONEncoder()
                encoder.outputFormatting = [.withoutEscapingSlashes]
                payloadBytes = try encoder.encode(payload)
            } catch {
                encodeFailedCount += 1
                carryForward(sessionID, decoded: decoded, route: route,
                             knownByEntityID: knownByEntityID, desiredCursor: &desiredCursor)
                continue
            }
            guard payloadBytes.count <= CRMMacAnarlogSource.maxPayloadBytes else {
                payloadTooLargeCount += 1
                carryForward(sessionID, decoded: decoded, route: route,
                             knownByEntityID: knownByEntityID, desiredCursor: &desiredCursor)
                continue
            }

            let recordHash: String
            do {
                recordHash = try ContentHasher.contentHash(for: payloadBytes)
            } catch {
                encodeFailedCount += 1
                carryForward(sessionID, decoded: decoded, route: route,
                             knownByEntityID: knownByEntityID, desiredCursor: &desiredCursor)
                continue
            }
            let shouldEmit: Bool
            switch route {
            case .firstRun, .bootstrapViaKnownIDs, .recovery:
                shouldEmit = true
            case .delta:
                shouldEmit = decoded[sessionID]?.recordHash != recordHash
            }
            if shouldEmit {
                let sourceID = AnarlogSourceIDBuilder.upsertSourceID(
                    entityID: sessionID, payloadHash: recordHash)
                publishItems.append(AnarlogSessionsPublishItem(
                    sourceID: sourceID, kind: "meeting_note.recorded", payloadBytes: payloadBytes))
            }
            desiredCursor[sessionID] = AnarlogSessionsCursorEntry(recordHash: recordHash)
        }

        // Listing presence, including ineligible or not-found entries, prevents a delete.
        let listedWithoutRecord = listingIDs.subtracting(recordIDs)
        for sessionID in listedWithoutRecord {
            carryForward(sessionID, decoded: decoded, route: route,
                         knownByEntityID: knownByEntityID, desiredCursor: &desiredCursor)
        }

        let basis: [String: String?]
        switch route {
        case .firstRun:
            basis = [:]
        case .delta:
            basis = decoded.mapValues { Optional($0.recordHash) }
        case .bootstrapViaKnownIDs, .recovery:
            basis = knownByEntityID
        }
        let deletions = basis.keys.filter { !listingIDs.contains($0) }
        let decided: AnarlogTickOutcome
        if deletions.count <= cfg.deletionCap {
            for sessionID in deletions {
                let deleted = AnarlogSessionsPayloadShaping.shapeDeleted(
                    sessionID: sessionID, hostID: auth.hostID)
                let encoder = JSONEncoder()
                encoder.outputFormatting = [.withoutEscapingSlashes]
                let payloadBytes = try encoder.encode(deleted)
                publishItems.append(AnarlogSessionsPublishItem(
                    sourceID: AnarlogSourceIDBuilder.deleteSourceID(
                        entityID: sessionID, priorPayloadHash: basis[sessionID] ?? nil),
                    kind: "meeting_note.deleted",
                    payloadBytes: payloadBytes))
            }
            decided = .clean(newestSessionCreatedAt: newest)
        } else {
            for sessionID in deletions {
                switch route {
                case .delta:
                    if let prior = decoded[sessionID] { desiredCursor[sessionID] = prior }
                case .bootstrapViaKnownIDs, .recovery:
                    if let hash = knownByEntityID[sessionID] {
                        desiredCursor[sessionID] = AnarlogSessionsCursorEntry(recordHash: hash ?? "")
                    }
                case .firstRun:
                    break
                }
            }
            decided = .deletionsWithheld(withheld: deletions.count, newestSessionCreatedAt: newest)
        }

        let desiredCursorBytes: String
        do {
            desiredCursorBytes = try AnarlogSessionsCursorCodec.encode(desiredCursor)
        } catch {
            await markUnhealthy("cursor_encode_failed:\(error)", at: tickStart)
            await healthSink.record(source: .anarlogSessions, outcome: decided)
            return
        }

        let outcome = await publisher.publish(items: publishItems)
        let hadHashMismatch = outcome.rejected.contains { rejection in
            AnarlogSessionsPublisher.recoveryCodes.contains(rejection.code)
        }
        if hadHashMismatch {
            await setRecoveryFlag(reason: "hash_mismatch", at: tickStart)
        }
        if !outcome.needsAttention.isEmpty {
            let domainItems = outcome.needsAttention.map {
                NotificationConsumeItem(sessionID: $0.sessionID, reason: $0.reason)
            }
            await orphanNotificationCenter?.consume(needsAttention: domainItems)
        }

        let cleanBatch = outcome.rejected.isEmpty && outcome.unconfirmed == 0
        if !cleanBatch {
            var errorMessage = "publish_held_due_to_rejections (\(outcome.rejected.count) rejected"
            if outcome.unconfirmed > 0 {
                errorMessage += ", \(outcome.unconfirmed) unconfirmed"
            }
            errorMessage += ")"
            if encodeFailedCount > 0 || payloadTooLargeCount > 0 {
                errorMessage += "; anomalies encode_failed=\(encodeFailedCount) payload_too_large=\(payloadTooLargeCount)"
            }
            await recordLastError(errorMessage, at: tickStart)
            return
        }

        do {
            try await piClient.commitCursor(
                auth: auth, source: id.rawValue, cursor: desiredCursorBytes,
                baseCursor: cursorState.cursor, cursorEpoch: cursorState.cursorEpoch,
                backfillComplete: true)
        } catch {
            logger.warning("anarlog_sessions tick: cursor commit failed", metadata: [
                "error": .private(String(describing: error)),
            ])
            return
        }

        await healthSink.record(source: .anarlogSessions, outcome: decided)
        let anomalyMessage: String? = (encodeFailedCount > 0 || payloadTooLargeCount > 0)
            ? "anomalies encode_failed=\(encodeFailedCount) payload_too_large=\(payloadTooLargeCount)"
            : nil
        await commitCleanTick(
            cursor: desiredCursorBytes, cursorEpoch: cursorState.cursorEpoch,
            pushedAt: tickStart, anomalyError: anomalyMessage)
        await healthRegistry.update(
            id, healthSnapshot(enabled: true, lastScheduled: tickStart, lastPushed: tickStart))
        logger.debug("anarlog_sessions tick: complete", metadata: [
            "route": .public(String(describing: route)),
            "emitted": .public(String(publishItems.count)),
            "accepted": .public(String(outcome.accepted)),
            "duplicate": .public(String(outcome.duplicate)),
        ])
    }

    private static func entityID(fromSourceID sourceID: String) -> String {
        guard let separator = sourceID.firstIndex(of: "@") else { return sourceID }
        return String(sourceID[..<separator])
    }

    private func carryForward(
        _ sessionID: String,
        decoded: [String: AnarlogSessionsCursorEntry],
        route: Route,
        knownByEntityID: [String: String?],
        desiredCursor: inout [String: AnarlogSessionsCursorEntry]
    ) {
        switch route {
        case .delta:
            if let prior = decoded[sessionID] { desiredCursor[sessionID] = prior }
        case .bootstrapViaKnownIDs, .recovery:
            if let knownHash = knownByEntityID[sessionID] {
                desiredCursor[sessionID] = AnarlogSessionsCursorEntry(recordHash: knownHash ?? "")
            }
        case .firstRun:
            break
        }
    }

    private func reportCLIFailure(_ failure: AnarlogCLIFailure, at date: Date) async {
        await markUnhealthy("anarlog_cli_failed:\(String(describing: failure))", at: date)
        await healthSink.record(source: .anarlogSessions, outcome: .failed(failure))
    }

    private func commitCleanTick(
        cursor: String,
        cursorEpoch: Int64,
        pushedAt: Date,
        anomalyError: String?
    ) async {
        do {
            try await mutator.mutate { state in
                var source = state.sources[self.id.rawValue] ?? SourceState()
                source.cursor = cursor
                source.cursorEpoch = cursorEpoch
                source.lastPushedAt = pushedAt
                if let anomalyError {
                    source.lastError = anomalyError
                    source.lastErrorAt = pushedAt
                } else {
                    source.lastError = nil
                    source.lastErrorAt = nil
                }
                state.sources[self.id.rawValue] = source
            }
        } catch {
            logger.warning("anarlog_sessions tick: commitCleanTick mutate failed", metadata: [
                "error": .private(String(describing: error)),
            ])
        }
    }

    private func setRecoveryFlag(reason: String, at date: Date) async {
        do {
            try await mutator.mutate { state in
                var source = state.sources[self.id.rawValue] ?? SourceState()
                source.lastError = "recovery_requested:\(reason)"
                source.lastErrorAt = date
                state.sources[self.id.rawValue] = source
            }
        } catch {
            logger.warning("anarlog_sessions tick: setRecoveryFlag failed", metadata: [
                "error": .private(String(describing: error)),
            ])
        }
    }

    private func recordLastError(_ message: String, at date: Date) async {
        do {
            try await mutator.mutate { state in
                var source = state.sources[self.id.rawValue] ?? SourceState()
                if let existing = source.lastError, existing.hasPrefix("recovery_requested:") {
                    source.lastError = "\(existing); \(message)"
                } else {
                    source.lastError = message
                }
                source.lastErrorAt = date
                state.sources[self.id.rawValue] = source
            }
        } catch {
            logger.warning("anarlog_sessions tick: recordLastError failed", metadata: [
                "error": .private(String(describing: error)),
            ])
        }
    }

    private func markUnhealthy(_ reason: String, at date: Date) async {
        await healthRegistry.update(id, SourceHealthSnapshot(
            enabled: false, lastScheduledAt: date, lastError: reason, lastErrorAt: date))
        await recordLastError(reason, at: date)
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
