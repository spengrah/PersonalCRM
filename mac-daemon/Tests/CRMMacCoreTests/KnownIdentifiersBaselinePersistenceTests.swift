// KnownIdentifiersBaselinePersistenceTests — exercises the
// single-writer baseline-persistence sequence the DaemonCommand
// heartbeat refresher runs: after cache.replace, read each source's
// `persistableBaseline` and idempotently assign it to
// DaemonState.knownIdentifierBaselines, preserving `establishedAt`.
//
// The DaemonCommand closure itself isn't unit-testable in isolation, so
// this focused harness replicates its exact persist logic against a
// real StateMutator + temp StateStore + the production cache, asserting
// the single-writer invariants from the plan.
//
// Synthetic handles only (+15550000001, +15550000002); no real PII.
import XCTest
import Foundation
@testable import CRMMacCore

final class KnownIdentifiersBaselinePersistenceTests: XCTestCase {
    private var tempDir: URL!
    private let fixedNow = Date(timeIntervalSince1970: 1_779_000_000)

    override func setUpWithError() throws {
        try super.setUpWithError()
        tempDir = FileManager.default.temporaryDirectory
            .appendingPathComponent("crm-mac-baseline-persist-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: tempDir, withIntermediateDirectories: true)
    }

    override func tearDownWithError() throws {
        try? FileManager.default.removeItem(at: tempDir)
        try super.tearDownWithError()
    }

    private func makeMutator() throws -> StateMutator {
        let store = StateStore(fileURL: tempDir.appendingPathComponent("state.json"))
        try store.save(DaemonState(schemaVersion: 1))
        return StateMutator(store: store)
    }

    /// Replicates the DaemonCommand refresher's per-source persist step:
    /// read persistableBaseline OUTSIDE the mutate closure, then plain-
    /// assign it inside (idempotent, establishedAt preserved).
    private func persistBaselines(
        cache: KnownIdentifiersCache,
        consumers: Set<SourceID>,
        mutator: StateMutator,
        now: Date
    ) async throws {
        for consumer in consumers {
            guard let observed = await cache.persistableBaseline(for: consumer) else {
                continue
            }
            let key = consumer.rawValue
            let sorted = observed.sorted()
            try await mutator.mutate { state in
                var map = state.knownIdentifierBaselines ?? [:]
                let existing = map[key]
                if existing?.canonical == sorted { return }
                map[key] = KnownIdentifiersBaseline(
                    canonical: sorted,
                    establishedAt: existing?.establishedAt ?? now)
                state.knownIdentifierBaselines = map
            }
        }
    }

    // MARK: - first-upgrade seed persists full set with zero scans

    func testFirstUpgradeSeedPersistsFullSet() async throws {
        let mutator = try makeMutator()
        let cache = KnownIdentifiersCache(
            baselines: [:], consumers: [.messages, .phoneCalls])
        // First fetch seeds the baseline (noBaseline → seed, zero scans).
        await cache.replace(with: ["+15550000001", "+15550000002"])

        try await persistBaselines(cache: cache, consumers: [.messages, .phoneCalls],
                                   mutator: mutator, now: fixedNow)

        let state = try await mutator.read()
        let messages = state.knownIdentifierBaselines?["messages"]
        XCTAssertEqual(messages?.canonical, ["+15550000001", "+15550000002"])
        XCTAssertEqual(messages?.establishedAt, fixedNow)
        // No scans were enqueued on the seed.
        let drained = await cache.drainNewlyAdded(for: .messages)
        XCTAssertTrue(drained.isEmpty, "first-upgrade seed enqueues no scans")
    }

    // MARK: - idempotent no-op when unchanged

    func testRepeatedPersistIsIdempotentAndPreservesEstablishedAt() async throws {
        let mutator = try makeMutator()
        let cache = KnownIdentifiersCache(
            baselines: [.messages: []], consumers: [.messages])
        await cache.replace(with: ["+15550000001"])

        try await persistBaselines(cache: cache, consumers: [.messages],
                                   mutator: mutator, now: fixedNow)
        let first = try await mutator.read().knownIdentifierBaselines?["messages"]
        XCTAssertEqual(first?.establishedAt, fixedNow)

        // A second persist with a DIFFERENT `now` must NOT change the
        // baseline (unchanged set → no-op) nor bump establishedAt.
        let laterNow = fixedNow.addingTimeInterval(3600)
        try await persistBaselines(cache: cache, consumers: [.messages],
                                   mutator: mutator, now: laterNow)
        let second = try await mutator.read().knownIdentifierBaselines?["messages"]
        XCTAssertEqual(second?.canonical, ["+15550000001"])
        XCTAssertEqual(second?.establishedAt, fixedNow,
                       "establishedAt preserved across idempotent persists")
    }

    // MARK: - owed scan excluded until durably enqueued

    func testOwedScanExcludedFromPersistUntilConfirmed() async throws {
        let mutator = try makeMutator()
        let cache = KnownIdentifiersCache(
            baselines: [.messages: ["+15550000001"]], consumers: [.messages])
        // X appears new → in pendingNewlyAdded → owed → excluded.
        await cache.replace(with: ["+15550000001", "+15550000002"])

        try await persistBaselines(cache: cache, consumers: [.messages],
                                   mutator: mutator, now: fixedNow)
        let beforeConfirm = try await mutator.read().knownIdentifierBaselines?["messages"]
        XCTAssertEqual(beforeConfirm?.canonical, ["+15550000001"],
                       "owed-but-not-durable X excluded from persisted baseline")

        // Source tick durably enqueues X: drain (→ inFlight) then confirm.
        _ = await cache.drainNewlyAdded(for: .messages)
        await cache.confirmDrained(for: .messages)

        try await persistBaselines(cache: cache, consumers: [.messages],
                                   mutator: mutator, now: fixedNow)
        let afterConfirm = try await mutator.read().knownIdentifierBaselines?["messages"]
        XCTAssertEqual(afterConfirm?.canonical, ["+15550000001", "+15550000002"],
                       "baseline advances to include X once its scan is durable")
    }

    // MARK: - removal shrinks the persisted baseline

    func testRemovalShrinksPersistedBaseline() async throws {
        let mutator = try makeMutator()
        let cache = KnownIdentifiersCache(
            baselines: [.messages: ["+15550000001", "+15550000002"]], consumers: [.messages])
        // A removal: only +...0001 remains.
        await cache.replace(with: ["+15550000001"])

        try await persistBaselines(cache: cache, consumers: [.messages],
                                   mutator: mutator, now: fixedNow)
        let state = try await mutator.read().knownIdentifierBaselines?["messages"]
        XCTAssertEqual(state?.canonical, ["+15550000001"], "removal persists the shrunk set")
    }
}
