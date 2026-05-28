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
    private static let session1 = "deadbeef-1111-2222-3333-444455556661"
    private static let session2 = "deadbeef-1111-2222-3333-444455556662"
    private static let session3 = "deadbeef-1111-2222-3333-444455556663"

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
            Self.session1: SessionMetadata(
                title: "Synthetic Test Session",
                createdAt: Date(timeIntervalSince1970: 1_716_000_000),
                sessionDirURL: URL(fileURLWithPath: "/tmp/anarlog/sessions/\(Self.session1)")),
        ])
        let center = makeCenter(presenter: presenter, lookup: lookup)
        await center.consume(needsAttention: [
            NotificationConsumeItem(sessionID: Self.session1, reason: "orphan"),
        ])
        let calls = await presenter.recordedAddCalls()
        XCTAssertEqual(calls.count, 1)
        XCTAssertEqual(calls[0].identifier, "orphan:\(Self.session1):1")
        XCTAssertEqual(calls[0].title, "Untagged session")
        XCTAssertTrue(calls[0].body.contains("Tag participants in Anarlog"))
        XCTAssertTrue(calls[0].body.contains("Synthetic Test Session"))
        XCTAssertEqual(calls[0].userInfo["session_uuid"], Self.session1)
        XCTAssertEqual(calls[0].userInfo["reason"], "orphan")

        let state = try stateStore.load()
        XCTAssertEqual(state.pendingOrphanNotifications.count, 1)
        XCTAssertEqual(state.pendingOrphanNotifications[0].sessionUUID, Self.session1)
        XCTAssertEqual(state.pendingOrphanNotifications[0].reason, "orphan")
        XCTAssertEqual(state.pendingOrphanNotifications[0].deliveryState, "queued")
        XCTAssertGreaterThan(state.notificationMutationSequence, 0)
    }

    // MARK: - TC-OC2: consume conflict uses conflict identifier + userInfo

    func testConsumeConflictHasDistinctIdentifierAndUserInfo() async throws {
        let presenter = FakeUserNotificationPresenter(authorizationResult: true)
        let center = makeCenter(presenter: presenter)
        await center.consume(needsAttention: [
            NotificationConsumeItem(sessionID: Self.session1, reason: "conflict"),
        ])
        let calls = await presenter.recordedAddCalls()
        XCTAssertEqual(calls.count, 1)
        XCTAssertEqual(calls[0].identifier, "conflict:\(Self.session1):1")
        XCTAssertEqual(calls[0].title, "Session needs CRM attention")
        XCTAssertEqual(calls[0].userInfo["reason"], "conflict")
    }

    // MARK: - TC-OC3 / TC-OC4: dedup within & across calls

    func testConsumeDedupWithinSameCall() async throws {
        let presenter = FakeUserNotificationPresenter(authorizationResult: true)
        let center = makeCenter(presenter: presenter)
        await center.consume(needsAttention: [
            NotificationConsumeItem(sessionID: Self.session1, reason: "orphan"),
            NotificationConsumeItem(sessionID: Self.session1, reason: "orphan"),
            NotificationConsumeItem(sessionID: Self.session1, reason: "orphan"),
        ])
        let calls = await presenter.recordedAddCalls()
        XCTAssertEqual(calls.count, 1)
    }

    func testConsumeDedupAcrossCalls() async throws {
        let presenter = FakeUserNotificationPresenter(authorizationResult: true)
        let center = makeCenter(presenter: presenter)
        let item = NotificationConsumeItem(sessionID: Self.session1, reason: "orphan")
        await center.consume(needsAttention: [item])
        await center.consume(needsAttention: [item])
        let calls = await presenter.recordedAddCalls()
        XCTAssertEqual(calls.count, 1)
    }

    // MARK: - TC-OC5: denied persists as denied and retries on next call

    func testConsumeAuthDeniedPersistsAndRetriesOnNextCall() async throws {
        let presenter = FakeUserNotificationPresenter(authorizationResult: false)
        let center = makeCenter(presenter: presenter)
        let item = NotificationConsumeItem(sessionID: Self.session1, reason: "orphan")
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
        let item = NotificationConsumeItem(sessionID: Self.session1, reason: "orphan")
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
            NotificationConsumeItem(sessionID: Self.session1, reason: "future-unknown-reason"),
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
            NotificationConsumeItem(sessionID: Self.session1, reason: "orphan"),
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
            Self.session1: SessionMetadata(title: longTitle, createdAt: nil, sessionDirURL: nil),
        ])
        let center = makeCenter(presenter: presenter, lookup: lookup)
        await center.consume(needsAttention: [
            NotificationConsumeItem(sessionID: Self.session1, reason: "orphan"),
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
        // Pre-seed pending list with Self.session1 (already queued).
        try await mutator.mutate { state in
            state.notificationMutationSequence = 1
            state.pendingOrphanNotifications = [
                PendingOrphanNotification(
                    sessionUUID: Self.session1, reason: "orphan",
                    notifiedAt: Date(timeIntervalSince1970: 1_715_000_000),
                    deliveryState: "queued", mutationSequence: 1,
                    osIdentifierSequence: 1),
            ]
        }
        // Pi returns Self.session1 + Self.session2.
        let center = makeCenter(presenter: presenter, fetcher: {
            [
                NotificationReconcileItem(
                    anarlogSessionID: Self.session1,
                    linkageState: "orphan_needs_review",
                    title: "Synthetic Session 1",
                    meetingAt: "2026-05-27T14:00:00Z"),
                NotificationReconcileItem(
                    anarlogSessionID: Self.session2,
                    linkageState: "conflict_pending",
                    title: "Synthetic Session 2",
                    meetingAt: "2026-05-27T15:00:00Z"),
            ]
        })
        await center.reconcile()
        // Self.session1 was already queued → no re-raise. Self.session2 is new → raised.
        // Snapshot's sequence was 1; the new upsert increments to 2.
        let calls = await presenter.recordedAddCalls()
        XCTAssertEqual(calls.count, 1)
        XCTAssertEqual(calls[0].identifier, "conflict:\(Self.session2):2")
        let state = try stateStore.load()
        XCTAssertEqual(state.pendingOrphanNotifications.count, 2)
    }

    // MARK: - TC-OC8: reconcile removes stale entries

    func testReconcileRemovesStaleEntries() async throws {
        let presenter = FakeUserNotificationPresenter(authorizationResult: true)
        // Pre-seed two entries — both with osIdentifierSequence set
        // because both represent live queued OS notifications.
        try await mutator.mutate { state in
            state.notificationMutationSequence = 2
            state.pendingOrphanNotifications = [
                PendingOrphanNotification(
                    sessionUUID: Self.session1, reason: "orphan",
                    notifiedAt: Date(timeIntervalSince1970: 1_715_000_000),
                    deliveryState: "queued", mutationSequence: 1,
                    osIdentifierSequence: 1),
                PendingOrphanNotification(
                    sessionUUID: Self.session2, reason: "conflict",
                    notifiedAt: Date(timeIntervalSince1970: 1_715_000_000),
                    deliveryState: "queued", mutationSequence: 2,
                    osIdentifierSequence: 2),
            ]
        }
        // Pi returns only Self.session1 → Self.session2 should be removed.
        let center = makeCenter(presenter: presenter, fetcher: {
            [
                NotificationReconcileItem(
                    anarlogSessionID: Self.session1,
                    linkageState: "orphan_needs_review",
                    title: "Synthetic Session 1",
                    meetingAt: "2026-05-27T14:00:00Z"),
            ]
        })
        await center.reconcile()
        let removedDelivered = await presenter.recordedRemoveDelivered()
        let removedPending = await presenter.recordedRemovePending()
        XCTAssertEqual(removedDelivered.count, 1)
        XCTAssertEqual(removedDelivered[0], ["conflict:\(Self.session2):2"])
        XCTAssertEqual(removedPending.count, 1)
        XCTAssertEqual(removedPending[0], ["conflict:\(Self.session2):2"])
        let state = try stateStore.load()
        XCTAssertEqual(state.pendingOrphanNotifications.count, 1)
        XCTAssertEqual(state.pendingOrphanNotifications[0].sessionUUID, Self.session1)
    }

    // MARK: - TC-OC9: reconcile with Pi error is a no-op

    func testReconcileWithPiErrorIsNoOp() async throws {
        let presenter = FakeUserNotificationPresenter(authorizationResult: true)
        try await mutator.mutate { state in
            state.pendingOrphanNotifications = [
                PendingOrphanNotification(
                    sessionUUID: Self.session1, reason: "orphan",
                    notifiedAt: Date(timeIntervalSince1970: 1_715_000_000),
                    deliveryState: "queued", mutationSequence: 1,
                    osIdentifierSequence: 1),
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
                    sessionUUID: Self.session1, reason: "orphan",
                    notifiedAt: Date(timeIntervalSince1970: 1_715_000_000),
                    deliveryState: "queued", mutationSequence: 1,
                    osIdentifierSequence: 1),
            ]
        }
        let center = makeCenter(presenter: presenter, fetcher: {
            [
                NotificationReconcileItem(
                    anarlogSessionID: Self.session1,
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
            Self.session1: SessionMetadata(title: "Stale Filesystem Title",
                                      createdAt: nil, sessionDirURL: nil),
        ])
        let center = makeCenter(presenter: presenter, lookup: lookup, fetcher: {
            [
                NotificationReconcileItem(
                    anarlogSessionID: Self.session1,
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
            Self.session1: SessionMetadata(title: "Fallback Filesystem Title",
                                      createdAt: nil, sessionDirURL: nil),
        ])
        let center = makeCenter(presenter: presenter, lookup: lookup, fetcher: {
            [
                NotificationReconcileItem(
                    anarlogSessionID: Self.session1,
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
                    sessionUUID: Self.session1, reason: "orphan",
                    notifiedAt: Date(timeIntervalSince1970: 1_715_000_000),
                    deliveryState: "queued", mutationSequence: 1,
                    osIdentifierSequence: 1),
                // Y arrived after snapshot — sequence > snapshotSequence.
                PendingOrphanNotification(
                    sessionUUID: Self.session2, reason: "orphan",
                    notifiedAt: Date(timeIntervalSince1970: 1_715_000_001),
                    deliveryState: "queued", mutationSequence: 99,
                    osIdentifierSequence: 99),
            ]
        }
        let center = makeCenter(presenter: presenter, fetcher: { [] })
        await center.reconcile()
        let state = try stateStore.load()
        // X removed (sequence=1 ≤ snapshot.sequence=1).
        // Y preserved (sequence=99 > snapshot.sequence=1).
        let remaining = state.pendingOrphanNotifications
        XCTAssertEqual(remaining.count, 1)
        XCTAssertEqual(remaining[0].sessionUUID, Self.session2)
    }

    // MARK: - TC-OC22b: in-mutator sequence guard re-applied

    func testReconcileSkipsRemovalIfEntrySequenceBumpedMidRun() async throws {
        // Simulate: snapshot.sequence=1 captures Self.session1 at
        // sequence=1. Mid-reconcile (BEFORE removeNotificationIfStale
        // runs), a concurrent consume() upserts Self.session1 bumping
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
                    sessionUUID: Self.session1, reason: "orphan",
                    notifiedAt: Date(timeIntervalSince1970: 1_715_000_000),
                    deliveryState: "queued", mutationSequence: 1,
                    osIdentifierSequence: 1),
            ]
        }

        // Build a custom fetcher closure that, BEFORE returning,
        // mutates the persisted state to bump Self.session1 to
        // sequence=99 — simulating a concurrent consume() that
        // landed between snapshot read and Pi fetch.
        let mutatorRef = mutator!
        let session1Ref = Self.session1
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
        XCTAssertEqual(state.pendingOrphanNotifications[0].sessionUUID, Self.session1)
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
                    sessionUUID: Self.session1, reason: "orphan",
                    notifiedAt: Date(timeIntervalSince1970: 1_715_000_000),
                    deliveryState: "denied", mutationSequence: 5,
                    osIdentifierSequence: nil),
            ]
        }
        let center = makeCenter(presenter: presenter, fetcher: {
            [
                NotificationReconcileItem(
                    anarlogSessionID: Self.session1,
                    linkageState: "orphan_needs_review",
                    title: "Synthetic Session 1",
                    meetingAt: "2026-05-27T14:00:00Z"),
            ]
        })
        await center.reconcile()
        let calls = await presenter.recordedAddCalls()
        XCTAssertEqual(calls.count, 1, "denied entry re-raised on reconcile")
        // Snapshot's sequence was 5; the re-raise upsert increments to 6.
        XCTAssertEqual(calls[0].identifier, "orphan:\(Self.session1):6")
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
                    sessionUUID: Self.session1, reason: "orphan",
                    notifiedAt: Date(timeIntervalSince1970: 1_715_000_000),
                    deliveryState: "queued", mutationSequence: 1,
                    osIdentifierSequence: 1),
            ]
        }
        // Pi now says the same session is conflict (reason flipped).
        let center = makeCenter(presenter: presenter, fetcher: {
            [
                NotificationReconcileItem(
                    anarlogSessionID: Self.session1,
                    linkageState: "conflict_pending",
                    title: "Synthetic Session 1",
                    meetingAt: "2026-05-27T14:00:00Z"),
            ]
        })
        await center.reconcile()
        let adds = await presenter.recordedAddCalls()
        let removed = await presenter.recordedRemoveDelivered()
        XCTAssertEqual(adds.count, 1)
        // Snapshot's sequence was 1; the new-reason upsert increments to 2.
        XCTAssertEqual(adds[0].identifier, "conflict:\(Self.session1):2")
        XCTAssertEqual(removed.count, 1)
        // Stale-remove targets the legacy-reason entry's recorded
        // sequence (1 from the seed), not the latest sequence.
        XCTAssertEqual(removed[0], ["orphan:\(Self.session1):1"])
        let state = try stateStore.load()
        XCTAssertEqual(state.pendingOrphanNotifications.count, 1)
        XCTAssertEqual(state.pendingOrphanNotifications[0].reason, "conflict")
    }

    // MARK: - TC-OC18: delegate tap opens workspace with correct URL

    func testDelegateDidReceiveOpensWorkspaceWithCorrectURL() async throws {
        let presenter = FakeUserNotificationPresenter(authorizationResult: true)
        let opener = FakeWorkspaceOpener(openResult: true)
        let sessionDir = URL(fileURLWithPath: "/tmp/anarlog/sessions/\(Self.session1)")
        let lookup = FakeSessionMetadataLookup(canned: [
            Self.session1: SessionMetadata(
                title: "Session 1", createdAt: nil, sessionDirURL: sessionDir),
            Self.session2: SessionMetadata(title: "Session 2", createdAt: nil, sessionDirURL: nil),
        ])
        let center = makeCenter(presenter: presenter, opener: opener, lookup: lookup)

        // Orphan tap → opens session dir URL.
        await center.handleTap(reason: "orphan", sessionUUID: Self.session1)
        var opened = await opener.recordedOpenedURLs()
        XCTAssertEqual(opened.count, 1)
        XCTAssertEqual(opened[0], sessionDir)

        // Conflict tap → opens Pi UI deep link.
        await center.handleTap(reason: "conflict", sessionUUID: Self.session2)
        opened = await opener.recordedOpenedURLs()
        XCTAssertEqual(opened.count, 2)
        let comps = URLComponents(url: opened[1], resolvingAgainstBaseURL: false)
        XCTAssertEqual(comps?.path, "/imports")
        let q = comps?.queryItems ?? []
        XCTAssertEqual(q.first(where: { $0.name == "session" })?.value, Self.session2)
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
            NotificationConsumeItem(sessionID: Self.session1, reason: "orphan"),
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

    // MARK: - TC-OC26: stale removal uses entry's own sequence

    /// Regression for the reconcile-vs-consume TOCTOU race. The
    /// stale-remove path MUST mint the OS identifier from the
    /// observed entry's mutationSequence — NOT from `maxSequence`,
    /// the latest snapshot sequence, or anything else. A
    /// concurrent consume() that lands between snapshot and OS-
    /// removal will upsert a fresh entry at a HIGHER sequence;
    /// because that fresh OS notification's identifier carries
    /// the higher sequence, the stale-remove call (which carries
    /// the lower sequence) cannot collaterally strip it.
    func testStaleRemovalIdentifierCarriesEntryRecordedSequence() async throws {
        let presenter = FakeUserNotificationPresenter(authorizationResult: true)
        try await mutator.mutate { state in
            // Snapshot sequence = 5, but the stale entry's OS
            // notification was minted at sequence 3 (an earlier
            // raise). The remove identifier must carry "3", not "5"
            // — i.e. the entry's osIdentifierSequence, not its
            // current mutationSequence and not the snapshot's
            // sequence.
            state.notificationMutationSequence = 5
            state.pendingOrphanNotifications = [
                PendingOrphanNotification(
                    sessionUUID: Self.session1, reason: "orphan",
                    notifiedAt: Date(timeIntervalSince1970: 1_715_000_000),
                    deliveryState: "queued", mutationSequence: 4,
                    osIdentifierSequence: 3),
            ]
        }
        // Pi returns empty → triggers stale-removal path.
        let center = makeCenter(presenter: presenter, fetcher: { [] })
        await center.reconcile()

        let removedDelivered = await presenter.recordedRemoveDelivered()
        let removedPending = await presenter.recordedRemovePending()
        XCTAssertEqual(removedDelivered.count, 1)
        XCTAssertEqual(removedDelivered[0], ["orphan:\(Self.session1):3"],
                       "stale-remove identifier must carry the entry's osIdentifierSequence (3)")
        XCTAssertEqual(removedPending.count, 1)
        XCTAssertEqual(removedPending[0], ["orphan:\(Self.session1):3"],
                       "stale-remove identifier must carry the entry's osIdentifierSequence (3)")
    }

    // MARK: - TC-OC27: race-survival — fresh raise has different identifier from stale remove

    /// Even when a concurrent consume() lands at the same key
    /// between the snapshot and the OS-side removal, the fresh
    /// raise's OS identifier MUST differ from the stale remove's
    /// identifier (different sequence component). This proves the
    /// versioned-identifier scheme severs the cross-contamination
    /// vector even if interleaving were possible inside actor
    /// isolation.
    ///
    /// We can't deterministically interleave inside an actor
    /// (await boundaries serialize), so we simulate the race
    /// outcome: seed two entries at the same (reason, uuid) at
    /// distinct sequences and assert the identifiers a raise and a
    /// remove WOULD mint are non-overlapping.
    func testRaceSurvivalFreshRaiseIdentifierDiffersFromStaleRemove() async throws {
        let staleSequence: UInt64 = 7
        let freshSequence: UInt64 = 8
        let staleIdentifier = notificationIdentifier(
            reason: "orphan", sessionUUID: Self.session1, sequence: staleSequence)
        let freshIdentifier = notificationIdentifier(
            reason: "orphan", sessionUUID: Self.session1, sequence: freshSequence)
        XCTAssertNotEqual(staleIdentifier, freshIdentifier,
            "stale-remove identifier and fresh-raise identifier must differ")
        // Sanity-check the format: the trailing sequence must be the
        // only differentiator (same prefix, different suffix).
        XCTAssertTrue(staleIdentifier.hasPrefix("orphan:\(Self.session1):"))
        XCTAssertTrue(freshIdentifier.hasPrefix("orphan:\(Self.session1):"))
        XCTAssertTrue(staleIdentifier.hasSuffix(":\(staleSequence)"))
        XCTAssertTrue(freshIdentifier.hasSuffix(":\(freshSequence)"))
    }

    // MARK: - TC-OC28: legacy-notification migration sweep

    /// Operators upgrading from a pre-versioned daemon build may
    /// have unversioned `<reason>:<uuid>` notifications still
    /// resident in Notification Center. The new code never mints
    /// or tracks those ids, so it must explicitly clean them up on
    /// startup. Otherwise the user sees ghost notifications that
    /// never disappear.
    func testCleanupLegacyOSNotificationsRemovesUnversionedIDs() async throws {
        let presenter = FakeUserNotificationPresenter(authorizationResult: true)
        await presenter.seedDeliveredIdentifiers([
            "orphan:\(Self.session1)",
            "conflict:\(Self.session2):4",            // versioned — keep
            "orphan:\(Self.session3)",
            "crm-mac-prod-presenter-test-xyz",        // unrelated — keep
        ])
        await presenter.seedPendingIdentifiers([
            "conflict:\(Self.session1)",
            "orphan:\(Self.session2):9",              // versioned — keep
        ])
        let center = makeCenter(presenter: presenter)

        await center.cleanupLegacyOSNotifications()

        let removedDelivered = await presenter.recordedRemoveDelivered()
        let removedPending = await presenter.recordedRemovePending()
        XCTAssertEqual(removedDelivered.count, 1)
        XCTAssertEqual(Set(removedDelivered[0]),
                       Set(["orphan:\(Self.session1)", "orphan:\(Self.session3)"]),
                       "only unversioned orphan/conflict ids should be swept")
        XCTAssertEqual(removedPending.count, 1)
        XCTAssertEqual(removedPending[0], ["conflict:\(Self.session1)"])
    }

    func testCleanupLegacyOSNotificationsIsNoOpWhenNothingLegacy() async throws {
        let presenter = FakeUserNotificationPresenter(authorizationResult: true)
        await presenter.seedDeliveredIdentifiers([
            "orphan:\(Self.session1):1",
            "conflict:\(Self.session2):2",
        ])
        await presenter.seedPendingIdentifiers([])
        let center = makeCenter(presenter: presenter)

        await center.cleanupLegacyOSNotifications()

        let removedDelivered = await presenter.recordedRemoveDelivered()
        let removedPending = await presenter.recordedRemovePending()
        XCTAssertTrue(removedDelivered.isEmpty)
        XCTAssertTrue(removedPending.isEmpty)
    }

    // MARK: - TC-OC29: legacy persisted entries are downgraded so reconcile re-raises

    /// A pre-versioned daemon build's persisted `queued` entry has
    /// no `osIdentifierSequence`. Reconcile treats `queued` entries
    /// as already-delivered no-ops, so without an explicit
    /// downgrade the user would silently lose the notification on
    /// upgrade (the OS notification gets swept away by the
    /// legacy-id sweep, but the persisted state still says
    /// `queued` so reconcile doesn't re-raise). The cleanup must
    /// downgrade these to `failed` to enroll them in the retry loop.
    func testCleanupLegacyOSNotificationsDowngradesQueuedEntriesWithoutOSIDSequence() async throws {
        let presenter = FakeUserNotificationPresenter(authorizationResult: true)
        await presenter.seedDeliveredIdentifiers([])
        await presenter.seedPendingIdentifiers([])
        try await mutator.mutate { state in
            state.notificationMutationSequence = 5
            state.pendingOrphanNotifications = [
                // Legacy: queued but no osIdentifierSequence.
                PendingOrphanNotification(
                    sessionUUID: Self.session1, reason: "orphan",
                    notifiedAt: Date(timeIntervalSince1970: 1_715_000_000),
                    deliveryState: "queued", mutationSequence: 5,
                    osIdentifierSequence: nil),
                // Fully-versioned: queued with osIdentifierSequence
                // — must be left alone.
                PendingOrphanNotification(
                    sessionUUID: Self.session2, reason: "conflict",
                    notifiedAt: Date(timeIntervalSince1970: 1_715_000_000),
                    deliveryState: "queued", mutationSequence: 5,
                    osIdentifierSequence: 5),
                // Already in retry state — must be left alone (it's
                // already enrolled in the retry loop).
                PendingOrphanNotification(
                    sessionUUID: Self.session3, reason: "orphan",
                    notifiedAt: Date(timeIntervalSince1970: 1_715_000_000),
                    deliveryState: "denied", mutationSequence: 5,
                    osIdentifierSequence: nil),
            ]
        }
        let center = makeCenter(presenter: presenter)

        await center.cleanupLegacyOSNotifications()

        let state = try stateStore.load()
        let bySession = Dictionary(
            uniqueKeysWithValues: state.pendingOrphanNotifications.map {
                ($0.sessionUUID, $0)
            })
        XCTAssertEqual(bySession[Self.session1]?.deliveryState, "failed",
                       "legacy queued entry without osIdentifierSequence must be downgraded")
        XCTAssertGreaterThan(bySession[Self.session1]?.mutationSequence ?? 0, 5,
                             "downgrade must bump mutationSequence to refresh the race guard")
        XCTAssertEqual(bySession[Self.session2]?.deliveryState, "queued",
                       "versioned queued entry must be left alone")
        XCTAssertEqual(bySession[Self.session2]?.osIdentifierSequence, 5,
                       "versioned queued entry's osIdentifierSequence must be preserved")
        XCTAssertEqual(bySession[Self.session3]?.deliveryState, "denied",
                       "non-queued entries must be left alone (already retrying)")
    }

    // MARK: - TC-OC30: reentrant raise guard

    /// Reentrancy regression. With the new persist-failed-first
    /// flow, a concurrent consume() that lands at the same key
    /// after the pre-add persist (which marks 'failed') but before
    /// the OS call returns must NOT start a parallel raise — the
    /// in-actor `raisesInFlight` guard skips it. Without the guard,
    /// both raises would queue OS notifications with consecutive
    /// sequence numbers and only the last confirmed sequence would
    /// be tracked, leaving an orphaned OS notification.
    ///
    /// We can't deterministically interleave two awaits on the
    /// same actor, so we use the FakeUserNotificationPresenter's
    /// recording semantics: a fast-firing consume that lands while
    /// another raise is mid-flight must be skipped, leaving exactly
    /// one OS add call per key per logical raise.
    ///
    /// The looser invariant test: starting two consume(...) calls
    /// for the same key back-to-back should produce exactly one OS
    /// raise, not two (the second observes 'queued' or in-flight
    /// and skips).
    func testConsecutiveConsumesForSameKeyProduceSingleRaise() async throws {
        let presenter = FakeUserNotificationPresenter(authorizationResult: true)
        let center = makeCenter(presenter: presenter)
        let item = NotificationConsumeItem(sessionID: Self.session1, reason: "orphan")
        await center.consume(needsAttention: [item])
        await center.consume(needsAttention: [item])
        await center.consume(needsAttention: [item])
        let calls = await presenter.recordedAddCalls()
        XCTAssertEqual(calls.count, 1,
            "subsequent consumes for the same (reason, session) must dedup against the first raise")
    }

    /// Entries written by builds older than sequencing itself
    /// decode with mutationSequence=0. Treat these as legacy too —
    /// the OS notification (if any) was minted with the unversioned
    /// scheme and the legacy-id sweep just removed it.
    func testCleanupLegacyOSNotificationsDowngradesEntriesWithZeroSequence() async throws {
        let presenter = FakeUserNotificationPresenter(authorizationResult: true)
        await presenter.seedDeliveredIdentifiers([])
        await presenter.seedPendingIdentifiers([])
        try await mutator.mutate { state in
            state.notificationMutationSequence = 0
            state.pendingOrphanNotifications = [
                PendingOrphanNotification(
                    sessionUUID: Self.session1, reason: "orphan",
                    notifiedAt: Date(timeIntervalSince1970: 1_715_000_000),
                    deliveryState: "queued", mutationSequence: 0,
                    osIdentifierSequence: nil),
            ]
        }
        let center = makeCenter(presenter: presenter)

        await center.cleanupLegacyOSNotifications()

        let state = try stateStore.load()
        XCTAssertEqual(state.pendingOrphanNotifications.count, 1)
        XCTAssertEqual(state.pendingOrphanNotifications[0].deliveryState, "failed")
    }
}
