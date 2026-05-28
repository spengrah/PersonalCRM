// OrphanNotificationCenter — actor that owns the daemon-side
// orphan/conflict notification flow.
//
// Two entry points, distinct code paths, single shared
// pending-notification persistence:
//
//   consume(needsAttention:) — fired from AnarlogSessionsSourcePlugin
//     after every successful publish. Items carry (sessionID, reason)
//     only; the title + time + click target come from the local
//     filesystem via SessionMetadataLookup.
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
        await presenter.setDelegate(d)
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
    /// title/time/click-target.
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
            let key = notificationIdentifier(reason: item.reason, sessionUUID: item.sessionID)
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

        // Build the Pi-set keyed by (uuid, reason). Drop entries
        // whose linkage_state isn't a recognized value.
        var piByKey: [String: NotificationReconcileItem] = [:]
        for pi in piItems {
            guard let reason = mapLinkageStateToReason(pi.linkageState) else {
                logger.warning("orphan-notify: reconcile dropped unknown linkage_state", metadata: [
                    "linkage_state": .public(pi.linkageState),
                ])
                continue
            }
            let key = notificationIdentifier(
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
                (notificationIdentifier(reason: $0.reason, sessionUUID: $0.sessionUUID), $0)
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
            let key = notificationIdentifier(reason: snap.reason, sessionUUID: snap.sessionUUID)
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
        // Ask for authorization (lazy first-use).
        let granted = await ensureAuthorization()
        let identifier = notificationIdentifier(reason: reason, sessionUUID: sessionUUID)

        if !granted {
            logger.warning("orphan-notify: authorization denied; persisting as 'denied'", metadata: [
                "session_uuid": .private(sessionUUID),
                "reason": .public(reason),
            ])
            await upsertPending(
                sessionUUID: sessionUUID,
                reason: reason,
                deliveryState: PendingDeliveryState.denied)
            return
        }

        let spec = makeSpec(
            identifier: identifier,
            sessionUUID: sessionUUID,
            reason: reason,
            title: title,
            createdAt: createdAt)
        do {
            try await presenter.add(spec)
            await upsertPending(
                sessionUUID: sessionUUID,
                reason: reason,
                deliveryState: PendingDeliveryState.queued)
        } catch {
            logger.warning("orphan-notify: presenter.add threw; persisting as 'failed'", metadata: [
                "session_uuid": .private(sessionUUID),
                "reason": .public(reason),
                "error": .private(String(describing: error)),
            ])
            await upsertPending(
                sessionUUID: sessionUUID,
                reason: reason,
                deliveryState: PendingDeliveryState.failed)
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
    private func removeNotificationIfStale(
        sessionUUID: String,
        reason: String,
        maxSequence: UInt64
    ) async {
        let identifier = notificationIdentifier(reason: reason, sessionUUID: sessionUUID)
        // Atomically check + remove. The mutator closure can't
        // mutate captured vars under Swift 6 strict-concurrency,
        // so we read back the persisted state after the mutate
        // call and inspect whether the entry is gone — that's the
        // signal to also remove from the OS queue.
        let preExisted: Bool
        do {
            preExisted = try await mutator.read().pendingOrphanNotifications.contains {
                $0.sessionUUID == sessionUUID && $0.reason == reason
            }
        } catch {
            logger.warning("orphan-notify: removeNotificationIfStale pre-read failed", metadata: [
                "error": .private(String(describing: error)),
            ])
            return
        }
        guard preExisted else { return }
        do {
            try await mutator.mutate { state in
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
            return
        }
        // Post-mutate read: was the entry removed? If yes, the
        // in-mutator sequence check passed → also remove from OS.
        // If no, a concurrent consume() bumped the sequence above
        // maxSequence and the mutate left the entry in place →
        // skip the OS-side removal so the fresh notification
        // stays on screen.
        let stillPresent: Bool
        do {
            stillPresent = try await mutator.read().pendingOrphanNotifications.contains {
                $0.sessionUUID == sessionUUID && $0.reason == reason
            }
        } catch {
            logger.warning("orphan-notify: removeNotificationIfStale post-read failed", metadata: [
                "error": .private(String(describing: error)),
            ])
            return
        }
        if stillPresent { return }
        await presenter.removeDelivered(withIdentifiers: [identifier])
        await presenter.removePending(withIdentifiers: [identifier])
    }

    // MARK: - persistence helpers

    private func upsertPending(
        sessionUUID: String,
        reason: String,
        deliveryState: String
    ) async {
        let now = clock()
        do {
            try await mutator.mutate { state in
                state.notificationMutationSequence &+= 1
                let seq = state.notificationMutationSequence
                if let idx = state.pendingOrphanNotifications.firstIndex(where: {
                    $0.sessionUUID == sessionUUID && $0.reason == reason
                }) {
                    state.pendingOrphanNotifications[idx].deliveryState = deliveryState
                    state.pendingOrphanNotifications[idx].mutationSequence = seq
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
                            mutationSequence: seq))
                }
            }
        } catch {
            logger.warning("orphan-notify: upsert persist failed", metadata: [
                "error": .private(String(describing: error)),
            ])
        }
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

    private static func parseRFC3339(_ s: String) -> Date? {
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime]
        if let d = f.date(from: s) { return d }
        f.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        return f.date(from: s)
    }
}
