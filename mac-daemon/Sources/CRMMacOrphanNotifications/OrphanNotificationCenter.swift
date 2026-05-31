// OrphanNotificationCenter — actor that owns the daemon-side
// orphan/conflict notification flow.
//
// Two entry points, distinct code paths, single shared
// pending-notification persistence:
//
//   consume(needsAttention:) — fired from AnarlogSessionsSourcePlugin
//     after every successful publish. Items carry (sessionID, reason)
//     only; the title + time come from the local filesystem via
//     SessionMetadataLookup.
//
//   reconcile() — fired (a) once on first heartbeat success at
//     startup, (b) every 300s by NotificationReconcileLoopPlugin.
//     Snapshots persisted state, calls /needs-attention, diffs:
//       - Pi has entry that we don't (or that we have but as
//         denied/failed) → raise + upsert.
//       - Pi doesn't have entry that we do → remove from OS and
//         from persisted state.
//     Uses a mutationSequence guard so a concurrent consume(...)
//     that upserts AFTER the snapshot isn't accidentally erased.
//
// The actor strongly retains an OrphanNotificationDelegate
// because Apple's UNUserNotificationCenter.delegate is a `weak`
// reference; without an owner of this strength the delegate
// deallocates at the next await boundary and taps stop firing.
import Foundation
import UserNotifications
import CRMMacCore

/// Closure that fetches the current needs-attention set from the
/// Pi. Captures auth + hostID at composition time so the actor's
/// reconcile() is parameterless. Returns domain-typed
/// NotificationReconcileItems — the composition boundary maps
/// PiClient wire DTOs to these.
public typealias NeedsAttentionFetcher =
    @Sendable () async throws -> [NotificationReconcileItem]

/// Delivery-state values for PendingOrphanNotification.
/// Tri-state because denied/failed entries are retried on the
/// next consume() / reconcile() opportunity — that prevents the
/// permanent missed-notification trap that would result from
/// treating any entry in the pending list as a hard de-dup signal.
public enum PendingDeliveryState {
    public static let queued = "queued"
    public static let denied = "denied"
    public static let failed = "failed"
}

public actor OrphanNotificationCenter {
    private let presenter: UserNotificationPresenter
    private let opener: WorkspaceOpener
    private let mutator: StateMutator
    private let metadataLookup: SessionMetadataLookup
    private let piURL: URL
    private let needsAttentionFetcher: NeedsAttentionFetcher
    private let logger: LoggerProtocol
    private let clock: @Sendable () -> Date

    // Cached lazy authorization result. nil = not yet asked.
    private var authorizationGranted: Bool?
    // Guards against overlapping reconcile() invocations.
    private var reconcileInFlight: Bool = false
    // Strong reference to the OS-level delegate. Apple's
    // UNUserNotificationCenter.delegate is `weak`; without an
    // owner of this strength the delegate deallocates at the
    // next await boundary and taps stop firing.
    private var delegate: OrphanNotificationDelegate?
    // Set of (reason, sessionUUID) pairs currently mid-raise.
    // Swift actors are reentrant across await boundaries, so a
    // concurrent consume()/reconcile() can land between our pre-
    // add persist (which marks the entry 'failed' to seed the
    // retry semantics) and the presenter.add suspension point.
    // Without this guard, the reentrant call would see 'failed'
    // and start a parallel raise — producing two OS notifications
    // for the same key, only one of which has its sequence tracked
    // for later stale-remove. Membership is per actor instance and
    // is keyed on the same semantic pair the rest of the actor
    // uses for entry identity.
    private var raisesInFlight: Set<String> = []

    public init(
        presenter: UserNotificationPresenter,
        opener: WorkspaceOpener,
        mutator: StateMutator,
        metadataLookup: SessionMetadataLookup,
        piURL: URL,
        needsAttentionFetcher: @escaping NeedsAttentionFetcher,
        logger: LoggerProtocol,
        clock: @escaping @Sendable () -> Date = { Date() }
    ) {
        self.presenter = presenter
        self.opener = opener
        self.mutator = mutator
        self.metadataLookup = metadataLookup
        self.piURL = piURL
        self.needsAttentionFetcher = needsAttentionFetcher
        self.logger = logger
        self.clock = clock
    }

    /// One-time wiring step: instantiate the OS-level delegate and
    /// register it with the presenter. The actor retains the
    /// delegate strongly so it survives across await boundaries.
    /// Idempotent — calling twice replaces the delegate (harmless).
    public func installDelegate() async {
        let d = OrphanNotificationDelegate(center: self)
        self.delegate = d
        await presenter.setDelegate(UserNotificationDelegateRef(d))
    }

    /// Startup sweep that reconciles Notification Center with the
    /// daemon's persisted state. Removes two classes of ghost
    /// notifications:
    ///
    /// 1. Legacy unversioned ids (`<reason>:<uuid>`) minted by
    ///    pre-versioning daemon builds. The new code can't target
    ///    these via stale-remove because its identifiers are
    ///    versioned.
    ///
    /// 2. Orphaned versioned ids (`<reason>:<uuid>:<seq>`) that no
    ///    persisted entry's `osIdentifierSequence` references.
    ///    These accrue when a daemon crash lands between
    ///    `presenter.add` succeeding and the post-add confirm
    ///    persisting — the OS keeps the notification but the
    ///    persisted entry is `failed` with `osIdentifierSequence:
    ///    nil`, so reconcile re-raises and a NEW versioned
    ///    notification appears alongside the orphaned one.
    ///
    /// Also downgrades persisted `queued` entries whose
    /// `osIdentifierSequence` is nil (or whose `mutationSequence`
    /// is 0 from pre-sequencing builds) so reconcile re-raises
    /// with a versioned identifier. Without this, an upgraded
    /// operator would silently lose the notification when the
    /// legacy OS id was swept.
    ///
    /// Best-effort — wired into daemon startup once and safe to
    /// run even when there's nothing to clean up.
    public func cleanupLegacyOSNotifications() async {
        let delivered = await presenter.getDeliveredIdentifiers()
        let pending = await presenter.getPendingIdentifiers()

        // Build the set of identifiers persisted state EXPECTS to
        // exist on the OS side: queued entries with a known
        // osIdentifierSequence. Anything else with an
        // orphan:/conflict: prefix is a ghost (legacy or
        // crash-orphaned) and gets swept.
        let expectedIdentifiers: Set<String>
        do {
            expectedIdentifiers = try await mutator.read()
                .pendingOrphanNotifications
                .reduce(into: Set<String>()) { acc, entry in
                    guard entry.deliveryState == PendingDeliveryState.queued,
                          let osSeq = entry.osIdentifierSequence else { return }
                    acc.insert(notificationIdentifier(
                        reason: entry.reason,
                        sessionUUID: entry.sessionUUID,
                        sequence: osSeq))
                }
        } catch {
            logger.warning("orphan-notify: startup sweep read failed", metadata: [
                "error": .private(String(describing: error)),
            ])
            return
        }

        // An id is a "ghost" iff it claims to be one of ours
        // (orphan: or conflict: prefix) AND isn't in the expected
        // set. This catches both legacy unversioned ids and
        // orphaned versioned ids in a single pass.
        func isGhost(_ id: String) -> Bool {
            if expectedIdentifiers.contains(id) { return false }
            if isLegacyNotificationIdentifier(id) { return true }
            // Versioned id whose sequence isn't expected by state.
            let parts = id.split(separator: ":", omittingEmptySubsequences: false)
            guard parts.count == 3 else { return false }
            let prefix = String(parts[0])
            return prefix == NotificationReason.orphan ||
                prefix == NotificationReason.conflict
        }

        let ghostDelivered = delivered.filter(isGhost)
        let ghostPending = pending.filter(isGhost)

        // Downgrade persisted entries FIRST, before any OS-side
        // removal. If the persist fails (next block returns early
        // on error), we leave the OS notifications alone too —
        // otherwise a downgrade-failure-after-OS-removal would
        // strand the entry in 'queued', reconcile would treat it
        // as already-delivered, and the user would silently lose
        // the notification with no retry path.
        //
        // The downgrade targets persisted entries that lack a
        // usable osIdentifierSequence while still appearing
        // 'queued'. Two markers identify these: (a)
        // osIdentifierSequence is nil (legacy decode, or post-
        // crash where the pre-add persist landed but confirm
        // didn't), or (b) mutationSequence is 0 (decoded-default
        // for pre-sequencing entries). Both cases mean reconcile
        // would treat the entry as already-delivered and never
        // re-raise. Downgrading to 'failed' enrolls them in the
        // retry loop.
        let downgradedCount: Int
        do {
            downgradedCount = try await mutator.mutateReturning { state -> Int in
                var count = 0
                for idx in state.pendingOrphanNotifications.indices {
                    let e = state.pendingOrphanNotifications[idx]
                    let isQueued = e.deliveryState == PendingDeliveryState.queued
                    let lacksOSSequence = e.osIdentifierSequence == nil
                    let legacyEntry = e.mutationSequence == 0
                    guard isQueued && (lacksOSSequence || legacyEntry) else { continue }
                    state.notificationMutationSequence &+= 1
                    let seq = state.notificationMutationSequence
                    state.pendingOrphanNotifications[idx].deliveryState =
                        PendingDeliveryState.failed
                    state.pendingOrphanNotifications[idx].mutationSequence = seq
                    state.pendingOrphanNotifications[idx].osIdentifierSequence = nil
                    count += 1
                }
                return count
            }
        } catch {
            logger.warning("orphan-notify: startup sweep persisted-state downgrade failed; OS side untouched", metadata: [
                "error": .private(String(describing: error)),
            ])
            return
        }

        // Persisted state is now consistent. Safe to remove the
        // OS-side ghosts.
        if !ghostDelivered.isEmpty {
            await presenter.removeDelivered(withIdentifiers: ghostDelivered)
        }
        if !ghostPending.isEmpty {
            await presenter.removePending(withIdentifiers: ghostPending)
        }

        if !ghostDelivered.isEmpty || !ghostPending.isEmpty || downgradedCount > 0 {
            logger.info("orphan-notify: startup sweep removed ghost notifications", metadata: [
                "delivered_count": .public(String(ghostDelivered.count)),
                "pending_count": .public(String(ghostPending.count)),
                "downgraded_persisted_count": .public(String(downgradedCount)),
            ])
        }
    }

    // Test-only accessor: returns true when a delegate has been
    // installed. Used by the delegate-retention regression test.
    public func hasDelegateInstalled() -> Bool {
        delegate != nil
    }

    // MARK: - ingest path

    /// Forward the ingest response's needs_attention entries
    /// through the notification pipeline. Each entry is rendered
    /// using the local filesystem (via SessionMetadataLookup) for
    /// title/time.
    ///
    /// De-dup posture: a (sessionUUID, reason) with an existing
    /// `queued` entry is skipped. An existing `denied`/`failed`
    /// entry is RE-ATTEMPTED — this is the missed-notification
    /// recovery hook.
    public func consume(needsAttention items: [NotificationConsumeItem]) async {
        if items.isEmpty { return }

        // Within-batch dedup so two identical entries in a single
        // response don't double-raise.
        var seenWithinBatch: Set<String> = []

        // Reset cached authorization if any pending entry isn't
        // queued — this lets a permission re-grant self-heal on
        // the next raise attempt without a daemon restart.
        await maybeResetAuthorizationCacheIfAnyNotQueued()

        for item in items {
            // Within-batch dedup is keyed on the sequence-independent
            // pair so two identical items in one batch produce a single
            // raise. The OS-side identifier is sequence-versioned
            // separately when the request is actually queued.
            let key = matchKey(reason: item.reason, sessionUUID: item.sessionID)
            if seenWithinBatch.contains(key) { continue }
            seenWithinBatch.insert(key)

            // Validate reason: ingest path uses the user-facing
            // strings already, but forward-compat protects against
            // a future Pi rolling out a new reason.
            guard item.reason == NotificationReason.orphan ||
                    item.reason == NotificationReason.conflict else {
                logger.warning("orphan-notify: consume ignored unknown reason", metadata: [
                    "reason": .public(item.reason),
                    "session_uuid": .private(item.sessionID),
                ])
                continue
            }

            // Lookup metadata from the filesystem (ingest path).
            let metadata = await metadataLookup.lookup(sessionUUID: item.sessionID)

            await raiseIfNotAlreadyQueued(
                sessionUUID: item.sessionID,
                reason: item.reason,
                metadata: metadata)
        }
    }

    // MARK: - reconcile path

    /// Snapshot the persisted pending list + the current
    /// mutationSequence, fetch the Pi's authoritative set, and
    /// reconcile. Race-safe vs. a concurrent consume(...) via the
    /// sequence guard.
    public func reconcile() async {
        if reconcileInFlight {
            logger.debug("orphan-notify: reconcile already in flight, skipping")
            return
        }
        reconcileInFlight = true
        defer { reconcileInFlight = false }

        // Snapshot persisted state + sequence.
        let snapshot: (entries: [PendingOrphanNotification], sequence: UInt64)
        do {
            let state = try await mutator.read()
            snapshot = (state.pendingOrphanNotifications,
                        state.notificationMutationSequence)
        } catch {
            logger.warning("orphan-notify: reconcile snapshot failed", metadata: [
                "error": .private(String(describing: error)),
            ])
            return
        }

        // Fetch the Pi's authoritative set.
        let piItems: [NotificationReconcileItem]
        do {
            piItems = try await needsAttentionFetcher()
        } catch {
            logger.warning("orphan-notify: reconcile fetch failed", metadata: [
                "error": .private(String(describing: error)),
            ])
            return
        }

        // Build the Pi-set keyed by (reason, uuid). Drop entries
        // whose linkage_state isn't a recognized value. The match
        // key is sequence-independent because the Pi side has no
        // notion of the daemon's sequence — matching across snapshot
        // vs Pi must be done on the semantic pair, with sequence
        // only baked into the OS-side identifier at request time.
        var piByKey: [String: NotificationReconcileItem] = [:]
        for pi in piItems {
            guard let reason = mapLinkageStateToReason(pi.linkageState) else {
                logger.warning("orphan-notify: reconcile dropped unknown linkage_state", metadata: [
                    "linkage_state": .public(pi.linkageState),
                ])
                continue
            }
            let key = matchKey(
                reason: reason,
                sessionUUID: pi.anarlogSessionID.lowercased())
            piByKey[key] = pi
        }
        let piKeys = Set(piByKey.keys)

        // Reset authorization cache once if anything in the
        // snapshot is non-queued — gives a permission re-grant a
        // chance to take effect on the re-raise below.
        let anyNotQueued = snapshot.entries.contains { $0.deliveryState != PendingDeliveryState.queued }
        if anyNotQueued {
            authorizationGranted = nil
        }

        // Compute adds + re-raises by walking Pi's set against
        // the snapshot.
        let snapByKey: [String: PendingOrphanNotification] = Dictionary(
            uniqueKeysWithValues: snapshot.entries.map {
                (matchKey(reason: $0.reason, sessionUUID: $0.sessionUUID), $0)
            })
        for (key, pi) in piByKey {
            guard let reason = mapLinkageStateToReason(pi.linkageState) else { continue }
            let sessionUUID = pi.anarlogSessionID.lowercased()
            if let existing = snapByKey[key] {
                if existing.deliveryState != PendingDeliveryState.queued {
                    // Re-raise: previously denied/failed but the
                    // user still needs to act on this session.
                    await raiseFromReconcile(
                        sessionUUID: sessionUUID,
                        reason: reason,
                        piTitle: pi.title,
                        piMeetingAt: pi.meetingAt)
                }
                // queued entries are no-ops here.
            } else {
                // New: Pi has it but snapshot doesn't.
                await raiseFromReconcile(
                    sessionUUID: sessionUUID,
                    reason: reason,
                    piTitle: pi.title,
                    piMeetingAt: pi.meetingAt)
            }
            _ = key
        }

        // Compute removes: snapshot entries no longer in Pi's set,
        // and whose mutationSequence ≤ snapshotSequence (so a
        // concurrent consume(...) that appended AFTER the snapshot
        // isn't erased). The sequence guard is also re-applied
        // INSIDE the mutator closure (see removeNotificationIfStale)
        // so a concurrent upsert that lands between the snapshot
        // and the OS-side removal is still preserved.
        for snap in snapshot.entries {
            let key = matchKey(reason: snap.reason, sessionUUID: snap.sessionUUID)
            if piKeys.contains(key) { continue }
            if snap.mutationSequence > snapshot.sequence { continue }
            await removeNotificationIfStale(
                sessionUUID: snap.sessionUUID,
                reason: snap.reason,
                maxSequence: snapshot.sequence)
        }
    }

    // MARK: - tap handler

    /// Called by OrphanNotificationDelegate when the user taps a
    /// notification. Constructs the click-target URL via
    /// clickTargetURL(...) and asks the WorkspaceOpener to open it.
    public func handleTap(reason: String?, sessionUUID: String?) async {
        guard let reason, let sessionUUID else {
            logger.warning("orphan-notify: tap missing userInfo fields")
            return
        }
        let metadata = await metadataLookup.lookup(sessionUUID: sessionUUID)
        guard let url = clickTargetURL(
            reason: reason,
            sessionUUID: sessionUUID,
            metadata: metadata,
            piURL: piURL
        ) else {
            logger.warning("orphan-notify: tap produced no URL", metadata: [
                "reason": .public(reason),
                "session_uuid": .private(sessionUUID),
            ])
            return
        }
        let opened = await opener.open(url)
        if !opened {
            logger.warning("orphan-notify: WorkspaceOpener refused URL", metadata: [
                "reason": .public(reason),
                "url": .private(url.absoluteString),
            ])
        }
    }

    // MARK: - raise helpers

    private func raiseIfNotAlreadyQueued(
        sessionUUID: String,
        reason: String,
        metadata: SessionMetadata?
    ) async {
        // Read current state once. If a queued entry matches,
        // no-op. Else, raise + upsert.
        let snapshot: [PendingOrphanNotification]
        do {
            snapshot = try await mutator.read().pendingOrphanNotifications
        } catch {
            logger.warning("orphan-notify: raise pre-read failed", metadata: [
                "error": .private(String(describing: error)),
            ])
            return
        }
        let existing = snapshot.first {
            $0.sessionUUID == sessionUUID && $0.reason == reason
        }
        if let existing, existing.deliveryState == PendingDeliveryState.queued {
            // Already raised + queued; no-op.
            return
        }
        await performRaiseAndPersist(
            sessionUUID: sessionUUID,
            reason: reason,
            title: metadata?.title,
            createdAt: metadata?.createdAt)
    }

    private func raiseFromReconcile(
        sessionUUID: String,
        reason: String,
        piTitle: String?,
        piMeetingAt: String
    ) async {
        // Prefer the Pi's title (authoritative for reconcile path);
        // fall back to the local filesystem only when Pi has none.
        let title: String?
        let createdAt: Date?
        if let piTitle, !piTitle.isEmpty {
            title = piTitle
            // Parse the Pi-supplied meetingAt for the time suffix.
            // RFC3339 format from the Pi handler.
            createdAt = Self.parseRFC3339(piMeetingAt)
        } else {
            let metadata = await metadataLookup.lookup(sessionUUID: sessionUUID)
            title = metadata?.title
            createdAt = metadata?.createdAt ?? Self.parseRFC3339(piMeetingAt)
        }
        await performRaiseAndPersist(
            sessionUUID: sessionUUID,
            reason: reason,
            title: title,
            createdAt: createdAt)
    }

    private func performRaiseAndPersist(
        sessionUUID: String,
        reason: String,
        title: String?,
        createdAt: Date?
    ) async {
        // Reentrancy guard: a concurrent consume()/reconcile() that
        // lands at the same key while this raise is awaiting the
        // OS call must not start a parallel raise. The pre-add
        // persist marks the entry 'failed' (retry state), which is
        // exactly the trigger that would normally cause re-raise —
        // so we need a separate in-actor signal that "a raise is
        // already in progress for this key, stand down".
        let raiseKey = matchKey(reason: reason, sessionUUID: sessionUUID)
        if raisesInFlight.contains(raiseKey) {
            return
        }
        raisesInFlight.insert(raiseKey)
        defer { raisesInFlight.remove(raiseKey) }

        // Ask for authorization (lazy first-use).
        let granted = await ensureAuthorization()

        if !granted {
            logger.warning("orphan-notify: authorization denied; persisting as 'denied'", metadata: [
                "session_uuid": .private(sessionUUID),
                "reason": .public(reason),
            ])
            _ = await upsertPending(
                sessionUUID: sessionUUID,
                reason: reason,
                deliveryState: PendingDeliveryState.denied,
                osIdentifierSequence: nil)
            return
        }

        // Optimistically persist as 'failed' first to (a) allocate
        // the mutationSequence used in the OS identifier and (b)
        // close the crash window — if the daemon dies between this
        // write and presenter.add returning, the persisted entry is
        // already in the retry state and the next reconcile re-raises.
        // The OS-identifier sequence is recorded only after we
        // actually issue the OS call, so a daemon crash leaves no
        // osIdentifierSequence pointing at a non-existent OS
        // notification.
        let preAddResult = await upsertPendingPreAdd(
            sessionUUID: sessionUUID,
            reason: reason)
        guard preAddResult.persisted else {
            // Persist failed → don't issue the OS call. We'd have
            // no reliable way to track it (no sequence to bake into
            // the identifier; no persisted entry to drive
            // stale-remove). The next reconcile re-attempts.
            return
        }
        let assignedSeq = preAddResult.sequence
        let identifier = notificationIdentifier(
            reason: reason,
            sessionUUID: sessionUUID,
            sequence: assignedSeq)
        let spec = makeSpec(
            identifier: identifier,
            sessionUUID: sessionUUID,
            reason: reason,
            title: title,
            createdAt: createdAt)
        do {
            try await presenter.add(spec)
        } catch {
            logger.warning("orphan-notify: presenter.add threw; entry remains 'failed' for retry", metadata: [
                "session_uuid": .private(sessionUUID),
                "reason": .public(reason),
                "error": .private(String(describing: error)),
            ])
            // Entry already persisted as 'failed' by the pre-add
            // step. The next consume() or reconcile() will retry.
            return
        }
        // Confirm success: transition state to 'queued' and record
        // the OS identifier's sequence so stale-remove can target
        // the actual OS notification. The mutationSequence bumps
        // (concurrent-mutation race guard) but the
        // osIdentifierSequence stays at the value baked into the
        // identifier we just gave the OS.
        let confirmed = await confirmPostAdd(
            sessionUUID: sessionUUID,
            reason: reason,
            osIdentifierSequence: assignedSeq)
        if !confirmed {
            // Persist failed AFTER the OS already accepted the
            // request. We now have an OS notification with no
            // tracked osIdentifierSequence in persisted state, so
            // future stale-remove would miss it. Best-effort:
            // remove the notification we just queued (we still
            // know its identifier locally). Worst case the user
            // briefly sees the notification before this cleanup
            // fires — preferable to a permanently-untracked
            // ghost. The persisted entry remains 'failed' so the
            // next retry can re-attempt the full cycle.
            logger.warning("orphan-notify: confirm persist failed; removing untrackable OS notification", metadata: [
                "session_uuid": .private(sessionUUID),
                "reason": .public(reason),
            ])
            await presenter.removeDelivered(withIdentifiers: [identifier])
            await presenter.removePending(withIdentifiers: [identifier])
        }
    }

    /// Remove the OS notification + persisted entry for a
    /// (sessionUUID, reason) pair, ONLY if the persisted entry's
    /// mutationSequence is ≤ maxSequence. This re-applies the
    /// reconcile-vs-consume race guard inside the mutator's
    /// serialized closure: if a concurrent consume() upserted the
    /// same key with a higher sequence between snapshot time and
    /// now, the persisted entry is preserved and the OS-side
    /// removal is skipped (the concurrent consume will have raised
    /// the OS notification freshly; we must not erase it).
    ///
    /// Ordering: OS cleanup runs BEFORE the persisted deletion.
    /// If the process crashes between the two, the next reconcile
    /// still sees the entry and retries the OS-side cleanup. The
    /// reverse order (persist first, then OS) would strand the OS
    /// notification with no signal to retry.
    private func removeNotificationIfStale(
        sessionUUID: String,
        reason: String,
        maxSequence: UInt64
    ) async {
        // Decide whether to remove by reading the current state.
        // Swift 6 strict-concurrency forbids mutating captured
        // vars inside the mutator closure, so we read here and
        // re-check inside the mutator below.
        let entry: PendingOrphanNotification?
        do {
            entry = try await mutator.read().pendingOrphanNotifications.first {
                $0.sessionUUID == sessionUUID && $0.reason == reason
            }
        } catch {
            logger.warning("orphan-notify: removeNotificationIfStale pre-read failed", metadata: [
                "error": .private(String(describing: error)),
            ])
            return
        }
        guard let entry, entry.mutationSequence <= maxSequence else {
            // Either no entry, or a concurrent consume() bumped
            // the sequence above maxSequence — preserve.
            return
        }

        // Build the identifier from the ENTRY's
        // osIdentifierSequence — the sequence actually baked into
        // the OS notification when presenter.add was called. Using
        // mutationSequence would drift if the entry was upserted
        // (e.g. failed → queued confirmation) after the add. A
        // concurrent consume() that lands between this read and the
        // OS removal will mint a NEW identifier (different sequence
        // component) and the freshly-raised OS notification is
        // therefore not stripped by the call below.
        //
        // If osIdentifierSequence is nil, no OS notification was
        // ever queued for this entry (denied / failed / legacy
        // pre-versioned entry) — skip the OS-side removal but still
        // clean up persisted state.
        if let osSeq = entry.osIdentifierSequence {
            let identifier = notificationIdentifier(
                reason: reason,
                sessionUUID: sessionUUID,
                sequence: osSeq)
            // OS cleanup first. If it returns, follow with the
            // persisted-state cleanup. The persisted entry serves
            // as the retry signal: until removed, the next reconcile
            // sees it and re-attempts OS cleanup.
            await presenter.removeDelivered(withIdentifiers: [identifier])
            await presenter.removePending(withIdentifiers: [identifier])
        }
        do {
            try await mutator.mutate { state in
                // Re-check the sequence inside the mutator closure
                // for the race-safety guarantee. A concurrent
                // consume() that landed between the read above and
                // this mutate must NOT have its fresh entry erased.
                if let idx = state.pendingOrphanNotifications.firstIndex(where: {
                    $0.sessionUUID == sessionUUID && $0.reason == reason
                }), state.pendingOrphanNotifications[idx].mutationSequence <= maxSequence {
                    state.pendingOrphanNotifications.remove(at: idx)
                }
            }
        } catch {
            logger.warning("orphan-notify: removeNotificationIfStale persist failed", metadata: [
                "error": .private(String(describing: error)),
                "session_uuid": .private(sessionUUID),
                "reason": .public(reason),
            ])
            // The OS notification is already removed at this
            // point; the persisted entry remains. Next reconcile
            // will see the entry, find it not in Pi's set, and
            // re-attempt — which becomes a no-op on the OS side
            // (removeDelivered/Pending on a non-existent id is
            // safe) and re-tries the persist.
        }
    }

    // MARK: - persistence helpers

    /// Upserts the (sessionUUID, reason) entry with the given
    /// deliveryState. Bumps `mutationSequence` for the race guard.
    /// Returns the assigned sequence, or 0 on persist failure
    /// (callers must check via the `osIdentifierSequence` argument
    /// before relying on the return value for an OS-side call).
    @discardableResult
    private func upsertPending(
        sessionUUID: String,
        reason: String,
        deliveryState: String,
        osIdentifierSequence: UInt64?
    ) async -> UInt64 {
        let now = clock()
        do {
            return try await mutator.mutateReturning { state -> UInt64 in
                state.notificationMutationSequence &+= 1
                let seq = state.notificationMutationSequence
                if let idx = state.pendingOrphanNotifications.firstIndex(where: {
                    $0.sessionUUID == sessionUUID && $0.reason == reason
                }) {
                    state.pendingOrphanNotifications[idx].deliveryState = deliveryState
                    state.pendingOrphanNotifications[idx].mutationSequence = seq
                    if let osSeq = osIdentifierSequence {
                        state.pendingOrphanNotifications[idx].osIdentifierSequence = osSeq
                    }
                    // notifiedAt is preserved from the original
                    // raise — the entry is the same notification
                    // semantically, just with updated delivery
                    // state.
                } else {
                    state.pendingOrphanNotifications.append(
                        PendingOrphanNotification(
                            sessionUUID: sessionUUID,
                            reason: reason,
                            notifiedAt: now,
                            deliveryState: deliveryState,
                            mutationSequence: seq,
                            osIdentifierSequence: osIdentifierSequence))
                }
                return seq
            }
        } catch {
            logger.warning("orphan-notify: upsert persist failed", metadata: [
                "error": .private(String(describing: error)),
            ])
            return 0
        }
    }

    /// Pre-add upsert: persist the entry as 'failed' (the retry
    /// state) before issuing the OS call. Returns the assigned
    /// sequence + a flag indicating persistence success. On persist
    /// failure (flag=false) the caller MUST NOT call presenter.add,
    /// because there's no persisted entry to drive a later stale-
    /// remove. The 'failed' state is the same one used after a
    /// real presenter.add error; reconcile re-raises it on the next
    /// pass, so a daemon crash between this write and the OS call
    /// is fully self-healing.
    private func upsertPendingPreAdd(
        sessionUUID: String,
        reason: String
    ) async -> (persisted: Bool, sequence: UInt64) {
        let now = clock()
        do {
            let seq = try await mutator.mutateReturning { state -> UInt64 in
                state.notificationMutationSequence &+= 1
                let seq = state.notificationMutationSequence
                if let idx = state.pendingOrphanNotifications.firstIndex(where: {
                    $0.sessionUUID == sessionUUID && $0.reason == reason
                }) {
                    state.pendingOrphanNotifications[idx].deliveryState =
                        PendingDeliveryState.failed
                    state.pendingOrphanNotifications[idx].mutationSequence = seq
                    // osIdentifierSequence is NOT updated here — the
                    // OS call hasn't been made yet. confirmPostAdd
                    // sets it on success.
                } else {
                    state.pendingOrphanNotifications.append(
                        PendingOrphanNotification(
                            sessionUUID: sessionUUID,
                            reason: reason,
                            notifiedAt: now,
                            deliveryState: PendingDeliveryState.failed,
                            mutationSequence: seq,
                            osIdentifierSequence: nil))
                }
                return seq
            }
            return (persisted: true, sequence: seq)
        } catch {
            logger.warning("orphan-notify: pre-add persist failed; OS call skipped", metadata: [
                "error": .private(String(describing: error)),
                "session_uuid": .private(sessionUUID),
                "reason": .public(reason),
            ])
            return (persisted: false, sequence: 0)
        }
    }

    /// Post-add confirmation: presenter.add succeeded; transition
    /// the entry to 'queued' and stamp the OS identifier's sequence
    /// onto the entry so future stale-remove calls can target the
    /// actual OS notification. Returns true on persist success.
    @discardableResult
    private func confirmPostAdd(
        sessionUUID: String,
        reason: String,
        osIdentifierSequence: UInt64
    ) async -> Bool {
        let seq = await upsertPending(
            sessionUUID: sessionUUID,
            reason: reason,
            deliveryState: PendingDeliveryState.queued,
            osIdentifierSequence: osIdentifierSequence)
        return seq != 0
    }

    private func maybeResetAuthorizationCacheIfAnyNotQueued() async {
        do {
            let pending = try await mutator.read().pendingOrphanNotifications
            if pending.contains(where: { $0.deliveryState != PendingDeliveryState.queued }) {
                authorizationGranted = nil
            }
        } catch {
            // Best-effort. Failure here doesn't block the raise.
            logger.debug("orphan-notify: pre-consume snapshot failed", metadata: [
                "error": .private(String(describing: error)),
            ])
        }
    }

    // MARK: - content + authorization

    private func ensureAuthorization() async -> Bool {
        if let cached = authorizationGranted {
            return cached
        }
        let granted = await presenter.requestAuthorization()
        authorizationGranted = granted
        return granted
    }

    private func makeSpec(
        identifier: String,
        sessionUUID: String,
        reason: String,
        title: String?,
        createdAt: Date?
    ) -> NotificationRequestSpec {
        let displayTitle = renderDisplayTitle(title)
        let timeSuffix = renderTimeSuffix(createdAt)

        let notifTitle: String
        let body: String
        switch reason {
        case NotificationReason.orphan:
            notifTitle = "Untagged session"
            body = "Tag participants in Anarlog — \(displayTitle)\(timeSuffix)"
        case NotificationReason.conflict:
            notifTitle = "Session needs CRM attention"
            body = "Open the imports page to resolve — \(displayTitle)\(timeSuffix)"
        default:
            // Defensive — should be filtered upstream.
            notifTitle = "Session needs attention"
            body = "\(displayTitle)\(timeSuffix)"
        }
        return NotificationRequestSpec(
            identifier: identifier,
            title: notifTitle,
            body: body,
            userInfo: [
                "session_uuid": sessionUUID,
                "reason": reason,
            ],
            sound: true)
    }

    private func renderDisplayTitle(_ title: String?) -> String {
        guard let title, !title.isEmpty else { return "Untitled session" }
        // Hard cap at 100 chars (deterministic test assertions;
        // Notification Center silently truncates anyway).
        if title.count <= 100 { return title }
        let cap = title.index(title.startIndex, offsetBy: 99)
        return String(title[..<cap]) + "…"
    }

    private func renderTimeSuffix(_ date: Date?) -> String {
        guard let date else { return "" }
        let f = DateFormatter()
        f.dateStyle = .short
        f.timeStyle = .short
        return " at \(f.string(from: date))"
    }

    /// Sequence-independent semantic key used for cross-snapshot
    /// matching (Pi-truth ↔ persisted snapshot). The OS-side
    /// identifier carries an extra sequence component for race
    /// safety; matching across snapshots is on the semantic pair
    /// only (a snapshot at sequence S and a Pi response describing
    /// the same session must map to the same key regardless of
    /// the snapshot's sequence).
    private func matchKey(reason: String, sessionUUID: String) -> String {
        "\(reason):\(sessionUUID)"
    }

    private static func parseRFC3339(_ s: String) -> Date? {
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime]
        if let d = f.date(from: s) { return d }
        f.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        return f.date(from: s)
    }
}
