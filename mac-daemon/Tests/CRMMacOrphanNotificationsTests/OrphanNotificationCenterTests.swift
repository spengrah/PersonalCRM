// Coverage for OrphanNotificationCenter actor — both ingest
// (consume) and reconcile paths.
//
// Strategy: inject FakeUserNotificationPresenter (records every
// add/remove call), FakeWorkspaceOpener (records every open),
// FakeSessionMetadataLookup (returns canned metadata),
// in-memory StateStore + StateMutator (real persistence behavior
// without touching disk), and synthetic needsAttentionFetcher
// closures.
//
// Tests use synthetic UUIDs and "Synthetic …" session titles —
// no PII per CLAUDE.md privacy rules.
import XCTest
import CRMMacCore
import CRMMacPiClient
@testable import CRMMacOrphanNotifications

final class OrphanNotificationCenterTests: XCTestCase {

    // MARK: - test rig

    private let piURL = URL(string: "https://pi.example")!
    private let session1 = "deadbeef-1111-2222-3333-444455556661"
    private let session2 = "deadbeef-1111-2222-3333-444455556662"
    private let session3 = "deadbeef-1111-2222-3333-444455556663"

    private var tempStateURL: URL!
    private var stateStore: StateStore!
    private var mutator: StateMutator!

    override func setUpWithError() throws {
        try super.setUpWithError()
        let tmpDir = FileManager.default.temporaryDirectory
            .appendingPathComponent("crm-mac-orphan-tests-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: tmpDir, withIntermediateDirectories: true)
        tempStateURL = tmpDir.appendingPathComponent("state.json")
        stateStore = StateStore(fileURL: tempStateURL)
        // Initialize a fresh state.json so mutator.read() succeeds.
        try stateStore.save(DaemonState())
        mutator = StateMutator(store: stateStore)
    }

    override func tearDownWithError() throws {
        try? FileManager.default.removeItem(at: tempStateURL.deletingLastPathComponent())
        try super.tearDownWithError()
    }

    private func makeCenter(
        presenter: FakeUserNotificationPresenter,
        opener: FakeWorkspaceOpener = FakeWorkspaceOpener(),
        lookup: FakeSessionMetadataLookup = FakeSessionMetadataLookup(),
        fetcher: @escaping NeedsAttentionFetcher = { [] },
        clock: @escaping @Sendable () -> Date = { Date(timeIntervalSince1970: 1_716_000_000) }
    ) -> OrphanNotificationCenter {
        OrphanNotificationCenter(
            presenter: presenter,
            opener: opener,
            mutator: mutator,
            metadataLookup: lookup,
            piURL: piURL,
            needsAttentionFetcher: fetcher,
            logger: NoopLogger(),
            clock: clock)
    }

    // MARK: - TC-OC1: consume orphan raises notification and persists

    func testConsumeOrphanRaisesAndPersists() async throws {
        let presenter = FakeUserNotificationPresenter(authorizationResult: true)
        let lookup = FakeSessionMetadataLookup(canned: [
            session1: SessionMetadata(
                title: "Synthetic Test Session",
                createdAt: Date(timeIntervalSince1970: 1_716_000_000),
                sessionDirURL: URL(fileURLWithPath: "/tmp/anarlog/sessions/\(session1)")),
        ])
        let center = makeCenter(presenter: presenter, lookup: lookup)
        await center.consume(needsAttention: [
            NotificationConsumeItem(sessionID: session1, reason: "orphan"),
        ])
        let calls = await presenter.recordedAddCalls()
        XCTAssertEqual(calls.count, 1)
        XCTAssertEqual(calls[0].identifier, "orphan:\(session1)")
        XCTAssertEqual(calls[0].title, "Untagged session")
        XCTAssertTrue(calls[0].body.contains("Tag participants in Anarlog"))
        XCTAssertTrue(calls[0].body.contains("Synthetic Test Session"))
        XCTAssertEqual(calls[0].userInfo["session_uuid"], session1)
        XCTAssertEqual(calls[0].userInfo["reason"], "orphan")

        let state = try stateStore.load()
        XCTAssertEqual(state.pendingOrphanNotifications.count, 1)
        XCTAssertEqual(state.pendingOrphanNotifications[0].sessionUUID, session1)
        XCTAssertEqual(state.pendingOrphanNotifications[0].reason, "orphan")
        XCTAssertEqual(state.pendingOrphanNotifications[0].deliveryState, "queued")
        XCTAssertGreaterThan(state.notificationMutationSequence, 0)
    }

    // MARK: - TC-OC2: consume conflict uses conflict identifier + userInfo

    func testConsumeConflictHasDistinctIdentifierAndUserInfo() async throws {
        let presenter = FakeUserNotificationPresenter(authorizationResult: true)
        let center = makeCenter(presenter: presenter)
        await center.consume(needsAttention: [
            NotificationConsumeItem(sessionID: session1, reason: "conflict"),
        ])
        let calls = await presenter.recordedAddCalls()
        XCTAssertEqual(calls.count, 1)
        XCTAssertEqual(calls[0].identifier, "conflict:\(session1)")
        XCTAssertEqual(calls[0].title, "Session needs CRM attention")
        XCTAssertEqual(calls[0].userInfo["reason"], "conflict")
    }

    // MARK: - TC-OC3 / TC-OC4: dedup within & across calls

    func testConsumeDedupWithinSameCall() async throws {
        let presenter = FakeUserNotificationPresenter(authorizationResult: true)
        let center = makeCenter(presenter: presenter)
        await center.consume(needsAttention: [
            NotificationConsumeItem(sessionID: session1, reason: "orphan"),
            NotificationConsumeItem(sessionID: session1, reason: "orphan"),
            NotificationConsumeItem(sessionID: session1, reason: "orphan"),
        ])
        let calls = await presenter.recordedAddCalls()
        XCTAssertEqual(calls.count, 1)
    }

    func testConsumeDedupAcrossCalls() async throws {
        let presenter = FakeUserNotificationPresenter(authorizationResult: true)
        let center = makeCenter(presenter: presenter)
        let item = NotificationConsumeItem(sessionID: session1, reason: "orphan")
        await center.consume(needsAttention: [item])
        await center.consume(needsAttention: [item])
        let calls = await presenter.recordedAddCalls()
        XCTAssertEqual(calls.count, 1)
    }

    // MARK: - TC-OC5: denied persists as denied and retries on next call

    func testConsumeAuthDeniedPersistsAndRetriesOnNextCall() async throws {
        let presenter = FakeUserNotificationPresenter(authorizationResult: false)
        let center = makeCenter(presenter: presenter)
        let item = NotificationConsumeItem(sessionID: session1, reason: "orphan")
        await center.consume(needsAttention: [item])
        var calls = await presenter.recordedAddCalls()
        XCTAssertEqual(calls.count, 0)  // Skipped due to denial.
        var state = try stateStore.load()
        XCTAssertEqual(state.pendingOrphanNotifications.count, 1)
        XCTAssertEqual(state.pendingOrphanNotifications[0].deliveryState, "denied")

        // Now permission flips to granted; next consume retries.
        await presenter.setAuthorizationResult(true)
        await center.consume(needsAttention: [item])
        calls = await presenter.recordedAddCalls()
        XCTAssertEqual(calls.count, 1, "second consume retries the denied entry")
        state = try stateStore.load()
        XCTAssertEqual(state.pendingOrphanNotifications.count, 1)
        XCTAssertEqual(state.pendingOrphanNotifications[0].deliveryState, "queued")
    }

    // MARK: - TC-OC6: add failure persists as failed and retries

    private struct TestError: Error {}

    func testConsumeAddFailurePersistsAndRetries() async throws {
        let presenter = FakeUserNotificationPresenter(authorizationResult: true,
                                                      addError: TestError())
        let center = makeCenter(presenter: presenter)
        let item = NotificationConsumeItem(sessionID: session1, reason: "orphan")
        await center.consume(needsAttention: [item])
        var state = try stateStore.load()
        XCTAssertEqual(state.pendingOrphanNotifications.count, 1)
        XCTAssertEqual(state.pendingOrphanNotifications[0].deliveryState, "failed")
        // Clear the error; next consume retries.
        await presenter.setAddError(nil)
        await center.consume(needsAttention: [item])
        let calls = await presenter.recordedAddCalls()
        XCTAssertEqual(calls.count, 2, "second consume retries")
        state = try stateStore.load()
        XCTAssertEqual(state.pendingOrphanNotifications[0].deliveryState, "queued")
    }

    // MARK: - TC-OC11: unknown reason logs + skips

    func testConsumeUnknownReasonSkips() async throws {
        let presenter = FakeUserNotificationPresenter(authorizationResult: true)
        let center = makeCenter(presenter: presenter)
        await center.consume(needsAttention: [
            NotificationConsumeItem(sessionID: session1, reason: "future-unknown-reason"),
        ])
        let calls = await presenter.recordedAddCalls()
        XCTAssertTrue(calls.isEmpty)
        let state = try stateStore.load()
        XCTAssertTrue(state.pendingOrphanNotifications.isEmpty)
    }

    // MARK: - TC-OC13: fallback to "Untitled session"

    func testConsumeFallsBackToUntitledWhenLookupReturnsNil() async throws {
        let presenter = FakeUserNotificationPresenter(authorizationResult: true)
        let center = makeCenter(presenter: presenter)
        // No canned metadata → lookup returns nil.
        await center.consume(needsAttention: [
            NotificationConsumeItem(sessionID: session1, reason: "orphan"),
        ])
        let calls = await presenter.recordedAddCalls()
        XCTAssertEqual(calls.count, 1)
        XCTAssertTrue(calls[0].body.contains("Untitled session"))
        XCTAssertFalse(calls[0].body.contains(" at "), "no time suffix when createdAt missing")
    }

    // MARK: - TC-OC14: title truncation

    func testConsumeTruncatesLongTitle() async throws {
        let longTitle = String(repeating: "X", count: 200)
        let presenter = FakeUserNotificationPresenter(authorizationResult: true)
        let lookup = FakeSessionMetadataLookup(canned: [
            session1: SessionMetadata(title: longTitle, createdAt: nil, sessionDirURL: nil),
        ])
        let center = makeCenter(presenter: presenter, lookup: lookup)
        await center.consume(needsAttention: [
            NotificationConsumeItem(sessionID: session1, reason: "orphan"),
        ])
        let calls = await presenter.recordedAddCalls()
        XCTAssertEqual(calls.count, 1)
        // Display-title is capped at 100 chars (99 Xs + ellipsis).
        XCTAssertTrue(calls[0].body.contains(String(repeating: "X", count: 99) + "…"))
        XCTAssertFalse(calls[0].body.contains(String(repeating: "X", count: 100)))
    }

    // MARK: - TC-OC7: reconcile adds missing entries

    func testReconcileAddsMissingEntries() async throws {
        let presenter = FakeUserNotificationPresenter(authorizationResult: true)
        // Pre-seed pending list with session1 (already queued).
        try await mutator.mutate { state in
            state.notificationMutationSequence = 1
            state.pendingOrphanNotifications = [
                PendingOrphanNotification(
                    sessionUUID: self.session1, reason: "orphan",
                    notifiedAt: Date(timeIntervalSince1970: 1_715_000_000),
                    deliveryState: "queued", mutationSequence: 1),
            ]
        }
        // Pi returns session1 + session2.
        let center = makeCenter(presenter: presenter, fetcher: { [self] in
            [
                NotificationReconcileItem(
                    anarlogSessionID: self.session1,
                    linkageState: "orphan_needs_review",
                    title: "Synthetic Session 1",
                    meetingAt: "2026-05-27T14:00:00Z"),
                NotificationReconcileItem(
                    anarlogSessionID: self.session2,
                    linkageState: "conflict_pending",
                    title: "Synthetic Session 2",
                    meetingAt: "2026-05-27T15:00:00Z"),
            ]
        })
        await center.reconcile()
        // session1 was already queued → no re-raise. session2 is new → raised.
        let calls = await presenter.recordedAddCalls()
        XCTAssertEqual(calls.count, 1)
        XCTAssertEqual(calls[0].identifier, "conflict:\(session2)")
        let state = try stateStore.load()
        XCTAssertEqual(state.pendingOrphanNotifications.count, 2)
    }

    // MARK: - TC-OC8: reconcile removes stale entries

    func testReconcileRemovesStaleEntries() async throws {
        let presenter = FakeUserNotificationPresenter(authorizationResult: true)
        // Pre-seed two entries.
        try await mutator.mutate { state in
            state.notificationMutationSequence = 2
            state.pendingOrphanNotifications = [
                PendingOrphanNotification(
                    sessionUUID: self.session1, reason: "orphan",
                    notifiedAt: Date(timeIntervalSince1970: 1_715_000_000),
                    deliveryState: "queued", mutationSequence: 1),
                PendingOrphanNotification(
                    sessionUUID: self.session2, reason: "conflict",
                    notifiedAt: Date(timeIntervalSince1970: 1_715_000_000),
                    deliveryState: "queued", mutationSequence: 2),
            ]
        }
        // Pi returns only session1 → session2 should be removed.
        let center = makeCenter(presenter: presenter, fetcher: { [self] in
            [
                NotificationReconcileItem(
                    anarlogSessionID: self.session1,
                    linkageState: "orphan_needs_review",
                    title: "Synthetic Session 1",
                    meetingAt: "2026-05-27T14:00:00Z"),
            ]
        })
        await center.reconcile()
        let removedDelivered = await presenter.recordedRemoveDelivered()
        let removedPending = await presenter.recordedRemovePending()
        XCTAssertEqual(removedDelivered.count, 1)
        XCTAssertEqual(removedDelivered[0], ["conflict:\(session2)"])
        XCTAssertEqual(removedPending.count, 1)
        XCTAssertEqual(removedPending[0], ["conflict:\(session2)"])
        let state = try stateStore.load()
        XCTAssertEqual(state.pendingOrphanNotifications.count, 1)
        XCTAssertEqual(state.pendingOrphanNotifications[0].sessionUUID, session1)
    }

    // MARK: - TC-OC9: reconcile with Pi error is a no-op

    func testReconcileWithPiErrorIsNoOp() async throws {
        let presenter = FakeUserNotificationPresenter(authorizationResult: true)
        try await mutator.mutate { state in
            state.pendingOrphanNotifications = [
                PendingOrphanNotification(
                    sessionUUID: self.session1, reason: "orphan",
                    notifiedAt: Date(timeIntervalSince1970: 1_715_000_000),
                    deliveryState: "queued", mutationSequence: 1),
            ]
        }
        let center = makeCenter(presenter: presenter, fetcher: {
            throw TestError()
        })
        await center.reconcile()
        let calls = await presenter.recordedAddCalls()
        XCTAssertTrue(calls.isEmpty)
        let removed = await presenter.recordedRemoveDelivered()
        XCTAssertTrue(removed.isEmpty)
        let state = try stateStore.load()
        XCTAssertEqual(state.pendingOrphanNotifications.count, 1)
    }

    // MARK: - TC-OC10: no changes → no presenter calls

    func testReconcileNoChangesMakesNoPresenterCalls() async throws {
        let presenter = FakeUserNotificationPresenter(authorizationResult: true)
        try await mutator.mutate { state in
            state.notificationMutationSequence = 1
            state.pendingOrphanNotifications = [
                PendingOrphanNotification(
                    sessionUUID: self.session1, reason: "orphan",
                    notifiedAt: Date(timeIntervalSince1970: 1_715_000_000),
                    deliveryState: "queued", mutationSequence: 1),
            ]
        }
        let center = makeCenter(presenter: presenter, fetcher: { [self] in
            [
                NotificationReconcileItem(
                    anarlogSessionID: self.session1,
                    linkageState: "orphan_needs_review",
                    title: "Synthetic Session 1",
                    meetingAt: "2026-05-27T14:00:00Z"),
            ]
        })
        await center.reconcile()
        let adds = await presenter.recordedAddCalls()
        let removes = await presenter.recordedRemoveDelivered()
        XCTAssertTrue(adds.isEmpty)
        XCTAssertTrue(removes.isEmpty)
    }

    // MARK: - TC-OC19: reconcile uses Pi title directly

    func testReconcileUsesPiTitleDirectly() async throws {
        let presenter = FakeUserNotificationPresenter(authorizationResult: true)
        let lookup = FakeSessionMetadataLookup(canned: [
            session1: SessionMetadata(title: "Stale Filesystem Title",
                                      createdAt: nil, sessionDirURL: nil),
        ])
        let center = makeCenter(presenter: presenter, lookup: lookup, fetcher: { [self] in
            [
                NotificationReconcileItem(
                    anarlogSessionID: self.session1,
                    linkageState: "orphan_needs_review",
                    title: "Authoritative Pi Title",
                    meetingAt: "2026-05-27T14:00:00Z"),
            ]
        })
        await center.reconcile()
        let calls = await presenter.recordedAddCalls()
        XCTAssertEqual(calls.count, 1)
        XCTAssertTrue(calls[0].body.contains("Authoritative Pi Title"))
        XCTAssertFalse(calls[0].body.contains("Stale Filesystem Title"))
    }

    // MARK: - TC-OC20: reconcile falls back to lookup when Pi title nil

    func testReconcileFallsBackToLookupWhenPiTitleNil() async throws {
        let presenter = FakeUserNotificationPresenter(authorizationResult: true)
        let lookup = FakeSessionMetadataLookup(canned: [
            session1: SessionMetadata(title: "Fallback Filesystem Title",
                                      createdAt: nil, sessionDirURL: nil),
        ])
        let center = makeCenter(presenter: presenter, lookup: lookup, fetcher: { [self] in
            [
                NotificationReconcileItem(
                    anarlogSessionID: self.session1,
                    linkageState: "orphan_needs_review",
                    title: nil,
                    meetingAt: "2026-05-27T14:00:00Z"),
            ]
        })
        await center.reconcile()
        let calls = await presenter.recordedAddCalls()
        XCTAssertEqual(calls.count, 1)
        XCTAssertTrue(calls[0].body.contains("Fallback Filesystem Title"))
    }

    // MARK: - TC-OC22: race-safe removal with sequence guard

    func testReconcileRaceSafeRemovalWithSequenceGuard() async throws {
        // Snapshot has entry X (mutationSequence=1). Mid-reconcile,
        // a concurrent consume(...) upserts entry Y bumping the
        // sequence to 2. Pi returns [] (nothing). Reconcile's
        // remove loop iterates over the snapshot, finds X
        // (sequence=1 ≤ snapshot.sequence=1 → remove) and Y
        // wouldn't be in the snapshot. We can simulate this by
        // seeding the state with both entries directly: an entry
        // with sequence > snapshotSequence must NOT be removed.
        let presenter = FakeUserNotificationPresenter(authorizationResult: true)
        try await mutator.mutate { state in
            state.notificationMutationSequence = 1
            state.pendingOrphanNotifications = [
                PendingOrphanNotification(
                    sessionUUID: self.session1, reason: "orphan",
                    notifiedAt: Date(timeIntervalSince1970: 1_715_000_000),
                    deliveryState: "queued", mutationSequence: 1),
                // Y arrived after snapshot — sequence > snapshotSequence.
                PendingOrphanNotification(
                    sessionUUID: self.session2, reason: "orphan",
                    notifiedAt: Date(timeIntervalSince1970: 1_715_000_001),
                    deliveryState: "queued", mutationSequence: 99),
            ]
        }
        let center = makeCenter(presenter: presenter, fetcher: { [] })
        await center.reconcile()
        let state = try stateStore.load()
        // X removed (sequence=1 ≤ snapshot.sequence=1).
        // Y preserved (sequence=99 > snapshot.sequence=1).
        let remaining = state.pendingOrphanNotifications
        XCTAssertEqual(remaining.count, 1)
        XCTAssertEqual(remaining[0].sessionUUID, session2)
    }

    // MARK: - TC-OC22b: in-mutator sequence guard re-applied

    func testReconcileSkipsRemovalIfEntrySequenceBumpedMidRun() async throws {
        // Simulate: snapshot.sequence=1 captures session1 at
        // sequence=1. Mid-reconcile (BEFORE removeNotificationIfStale
        // runs), a concurrent consume() upserts session1 bumping
        // mutationSequence to 99. Reconcile's removal loop iterates
        // — the outer guard at iteration time may still pass (it
        // saw sequence=1 in the snapshot), but the INNER guard
        // inside the mutator closure must see the fresh sequence
        // and preserve the entry.
        //
        // We simulate the concurrent upsert by mutating state
        // BEFORE calling reconcile. The snapshot will capture
        // sequence=1; the in-mutator check will see sequence=99.
        let presenter = FakeUserNotificationPresenter(authorizationResult: true)
        // Initial state: snapshot.sequence=1, entry with sequence=1.
        try await mutator.mutate { state in
            state.notificationMutationSequence = 1
            state.pendingOrphanNotifications = [
                PendingOrphanNotification(
                    sessionUUID: self.session1, reason: "orphan",
                    notifiedAt: Date(timeIntervalSince1970: 1_715_000_000),
                    deliveryState: "queued", mutationSequence: 1),
            ]
        }

        // Build a custom fetcher closure that, BEFORE returning,
        // mutates the persisted state to bump session1 to
        // sequence=99 — simulating a concurrent consume() that
        // landed between snapshot read and Pi fetch.
        let mutatorRef = mutator!
        let session1Ref = session1
        let center = OrphanNotificationCenter(
            presenter: presenter,
            opener: FakeWorkspaceOpener(),
            mutator: mutator,
            metadataLookup: FakeSessionMetadataLookup(),
            piURL: piURL,
            needsAttentionFetcher: {
                try await mutatorRef.mutate { state in
                    state.notificationMutationSequence = 99
                    state.pendingOrphanNotifications[0].mutationSequence = 99
                    _ = session1Ref
                }
                return []  // Pi returns empty → triggers removal path.
            },
            logger: NoopLogger())
        await center.reconcile()

        // The entry's sequence (99) exceeded snapshot.sequence (1)
        // by the time the in-mutator check ran → entry preserved,
        // OS notification untouched.
        let state = try stateStore.load()
        XCTAssertEqual(state.pendingOrphanNotifications.count, 1,
                       "entry with sequence > snapshot must survive removal pass")
        XCTAssertEqual(state.pendingOrphanNotifications[0].sessionUUID, session1)
        let removed = await presenter.recordedRemoveDelivered()
        XCTAssertTrue(removed.isEmpty,
                      "no OS-side removal when persisted entry is preserved")
    }

    // MARK: - TC-OC23: reconcile re-raises denied entries still in Pi set

    func testReconcileReRaisesDeniedEntries() async throws {
        let presenter = FakeUserNotificationPresenter(authorizationResult: true)
        try await mutator.mutate { state in
            state.notificationMutationSequence = 5
            state.pendingOrphanNotifications = [
                PendingOrphanNotification(
                    sessionUUID: self.session1, reason: "orphan",
                    notifiedAt: Date(timeIntervalSince1970: 1_715_000_000),
                    deliveryState: "denied", mutationSequence: 5),
            ]
        }
        let center = makeCenter(presenter: presenter, fetcher: { [self] in
            [
                NotificationReconcileItem(
                    anarlogSessionID: self.session1,
                    linkageState: "orphan_needs_review",
                    title: "Synthetic Session 1",
                    meetingAt: "2026-05-27T14:00:00Z"),
            ]
        })
        await center.reconcile()
        let calls = await presenter.recordedAddCalls()
        XCTAssertEqual(calls.count, 1, "denied entry re-raised on reconcile")
        XCTAssertEqual(calls[0].identifier, "orphan:\(session1)")
        let state = try stateStore.load()
        XCTAssertEqual(state.pendingOrphanNotifications[0].deliveryState, "queued")
        XCTAssertGreaterThan(state.pendingOrphanNotifications[0].mutationSequence, 5)
    }

    // MARK: - TC-OC24: reconcile removes stale reason on flip

    func testReconcileRemovesStaleReasonOnFlip() async throws {
        let presenter = FakeUserNotificationPresenter(authorizationResult: true)
        try await mutator.mutate { state in
            state.notificationMutationSequence = 1
            state.pendingOrphanNotifications = [
                PendingOrphanNotification(
                    sessionUUID: self.session1, reason: "orphan",
                    notifiedAt: Date(timeIntervalSince1970: 1_715_000_000),
                    deliveryState: "queued", mutationSequence: 1),
            ]
        }
        // Pi now says the same session is conflict (reason flipped).
        let center = makeCenter(presenter: presenter, fetcher: { [self] in
            [
                NotificationReconcileItem(
                    anarlogSessionID: self.session1,
                    linkageState: "conflict_pending",
                    title: "Synthetic Session 1",
                    meetingAt: "2026-05-27T14:00:00Z"),
            ]
        })
        await center.reconcile()
        let adds = await presenter.recordedAddCalls()
        let removed = await presenter.recordedRemoveDelivered()
        XCTAssertEqual(adds.count, 1)
        XCTAssertEqual(adds[0].identifier, "conflict:\(session1)")
        XCTAssertEqual(removed.count, 1)
        XCTAssertEqual(removed[0], ["orphan:\(session1)"])
        let state = try stateStore.load()
        XCTAssertEqual(state.pendingOrphanNotifications.count, 1)
        XCTAssertEqual(state.pendingOrphanNotifications[0].reason, "conflict")
    }

    // MARK: - TC-OC18: delegate tap opens workspace with correct URL

    func testDelegateDidReceiveOpensWorkspaceWithCorrectURL() async throws {
        let presenter = FakeUserNotificationPresenter(authorizationResult: true)
        let opener = FakeWorkspaceOpener(openResult: true)
        let sessionDir = URL(fileURLWithPath: "/tmp/anarlog/sessions/\(session1)")
        let lookup = FakeSessionMetadataLookup(canned: [
            session1: SessionMetadata(
                title: "Session 1", createdAt: nil, sessionDirURL: sessionDir),
            session2: SessionMetadata(title: "Session 2", createdAt: nil, sessionDirURL: nil),
        ])
        let center = makeCenter(presenter: presenter, opener: opener, lookup: lookup)

        // Orphan tap → opens session dir URL.
        await center.handleTap(reason: "orphan", sessionUUID: session1)
        var opened = await opener.recordedOpenedURLs()
        XCTAssertEqual(opened.count, 1)
        XCTAssertEqual(opened[0], sessionDir)

        // Conflict tap → opens Pi UI deep link.
        await center.handleTap(reason: "conflict", sessionUUID: session2)
        opened = await opener.recordedOpenedURLs()
        XCTAssertEqual(opened.count, 2)
        let comps = URLComponents(url: opened[1], resolvingAgainstBaseURL: false)
        XCTAssertEqual(comps?.path, "/imports")
        let q = comps?.queryItems ?? []
        XCTAssertEqual(q.first(where: { $0.name == "session" })?.value, session2)
    }

    // MARK: - TC-OC25: delegate is retained after actor init

    func testDelegateIsRetainedAfterActorInit() async throws {
        let presenter = FakeUserNotificationPresenter(authorizationResult: true)
        let center = makeCenter(presenter: presenter)
        await center.installDelegate()
        let stillInstalled1 = await center.hasDelegateInstalled()
        XCTAssertTrue(stillInstalled1)
        // Push the actor through several await boundaries.
        await center.consume(needsAttention: [
            NotificationConsumeItem(sessionID: session1, reason: "orphan"),
        ])
        let stillInstalled2 = await center.hasDelegateInstalled()
        XCTAssertTrue(stillInstalled2, "delegate must survive across await boundaries")
        // Verify the FakeUserNotificationPresenter received the
        // delegate registration call.
        let delegateCount = await presenter.recordedSetDelegateCount()
        XCTAssertEqual(delegateCount, 1)
        let stored = await presenter.currentDelegate()
        XCTAssertNotNil(stored)
    }
}
