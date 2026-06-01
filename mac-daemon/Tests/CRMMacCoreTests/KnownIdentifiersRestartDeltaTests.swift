// KnownIdentifiersRestartDeltaTests — the per-source diff baseline +
// non-destructive scan-queue drain + persistableBaseline semantics that
// drive the targeted offline-restart auto-scan.
//
// Synthetic handles only (+15550000001 etc.); no real PII.
import XCTest
@testable import CRMMacCore

final class KnownIdentifiersRestartDeltaTests: XCTestCase {
    private let bothSources: Set<SourceID> = [.messages, .phoneCalls]

    // MARK: - tri-state baseline

    func testBaselineAbsentOnFirstUpgradeSeedsNoScans() async {
        // No persisted baseline → first replace seeds the set as the
        // baseline and enqueues NO scans for either source.
        let cache = KnownIdentifiersCache(consumers: bothSources)
        await cache.replace(with: ["+15550000001", "+15550000002", "+15550000003"])
        let m = await cache.drainNewlyAdded(for: .messages)
        let p = await cache.drainNewlyAdded(for: .phoneCalls)
        XCTAssertTrue(m.isEmpty)
        XCTAssertTrue(p.isEmpty)
        let baseM = await cache.baseline(for: .messages)
        XCTAssertEqual(baseM, ["+15550000001", "+15550000002", "+15550000003"])
    }

    func testBaselinePresentButEmptyDoesScan() async {
        // A REAL empty baseline (empty CRM that later gains a contact)
        // is NOT the same as absent — the new identifier IS scanned.
        let cache = KnownIdentifiersCache(
            baselines: [.messages: [], .phoneCalls: []],
            consumers: bothSources)
        await cache.replace(with: ["+15550000009"])
        let m = await cache.drainNewlyAdded(for: .messages)
        let p = await cache.drainNewlyAdded(for: .phoneCalls)
        XCTAssertEqual(m, ["+15550000009"])
        XCTAssertEqual(p, ["+15550000009"])
    }

    func testPreciseDeltaOnSecondRestart() async {
        let cache = KnownIdentifiersCache(
            baselines: [
                .messages: ["+15550000001", "+15550000002"],
                .phoneCalls: ["+15550000001", "+15550000002"],
            ],
            consumers: bothSources)
        await cache.replace(with: ["+15550000001", "+15550000002", "+15550000003"])
        let m = await cache.drainNewlyAdded(for: .messages)
        let p = await cache.drainNewlyAdded(for: .phoneCalls)
        XCTAssertEqual(m, ["+15550000003"])
        XCTAssertEqual(p, ["+15550000003"])
        let baseM = await cache.baseline(for: .messages)
        XCTAssertEqual(baseM, ["+15550000001", "+15550000002", "+15550000003"])
    }

    func testRemovalDoesNotScan() async {
        let cache = KnownIdentifiersCache(
            baselines: [
                .messages: ["+15550000001", "+15550000002", "+15550000003"],
                .phoneCalls: ["+15550000001", "+15550000002", "+15550000003"],
            ],
            consumers: bothSources)
        await cache.replace(with: ["+15550000001", "+15550000002"])
        let m = await cache.drainNewlyAdded(for: .messages)
        let p = await cache.drainNewlyAdded(for: .phoneCalls)
        XCTAssertTrue(m.isEmpty)
        XCTAssertTrue(p.isEmpty)
        let baseM = await cache.baseline(for: .messages)
        XCTAssertEqual(baseM, ["+15550000001", "+15550000002"])
    }

    // MARK: - per-source independence

    func testPerSourceIndependentDrain() async {
        // After a replace adding {C}, messages drains {C} AND phone_calls
        // STILL drains {C} — neither empties the other.
        let cache = KnownIdentifiersCache(
            baselines: [
                .messages: ["+15550000001"],
                .phoneCalls: ["+15550000001"],
            ],
            consumers: bothSources)
        await cache.replace(with: ["+15550000001", "+15550000003"])
        let m = await cache.drainNewlyAdded(for: .messages)
        XCTAssertEqual(m, ["+15550000003"])
        let p = await cache.drainNewlyAdded(for: .phoneCalls)
        XCTAssertEqual(p, ["+15550000003"], "phone_calls drain not emptied by messages drain")
    }

    func testBaselineTrailsLatestFetchDeleteThenReadd() async {
        // baseline {A,B,C}; replace({A,B}) → baseline {A,B}, drains ∅;
        // later replace({A,B,C}) → drains {C} (re-add detected as new).
        let cache = KnownIdentifiersCache(
            baselines: [.messages: ["+15550000001", "+15550000002", "+15550000003"]],
            consumers: [.messages])
        await cache.replace(with: ["+15550000001", "+15550000002"])
        let drainedAfterRemoval = await cache.drainNewlyAdded(for: .messages)
        XCTAssertTrue(drainedAfterRemoval.isEmpty)
        await cache.replace(with: ["+15550000001", "+15550000002", "+15550000003"])
        let drainedAfterReadd = await cache.drainNewlyAdded(for: .messages)
        XCTAssertEqual(drainedAfterReadd, ["+15550000003"])
    }

    // MARK: - non-destructive scan-queue drain

    func testNonDestructiveDrainRollbackViaReturnInFlight() async {
        let cache = KnownIdentifiersCache(
            baselines: [.messages: ["+15550000001"]],
            consumers: [.messages])
        await cache.replace(with: ["+15550000001", "+15550000003"])
        let drained = await cache.drainNewlyAdded(for: .messages)
        XCTAssertEqual(drained, ["+15550000003"])
        // WITHOUT confirm: a re-drain sees nothing (it's in-flight).
        let reDrainBeforeReturn = await cache.drainNewlyAdded(for: .messages)
        XCTAssertTrue(reDrainBeforeReturn.isEmpty)
        // returnInFlight rolls it back so the next drain re-returns it.
        await cache.returnInFlight(for: .messages)
        let reDrain = await cache.drainNewlyAdded(for: .messages)
        XCTAssertEqual(reDrain, ["+15550000003"])
    }

    func testConfirmDrainedClearsInFlightNoBaselineSideEffect() async {
        let cache = KnownIdentifiersCache(
            baselines: [.messages: ["+15550000001"]],
            consumers: [.messages])
        await cache.replace(with: ["+15550000001", "+15550000003"])
        _ = await cache.drainNewlyAdded(for: .messages)
        await cache.confirmDrained(for: .messages)
        // After confirm, returnInFlight is a no-op and the bucket is
        // empty — the identifier is durably enqueued.
        await cache.returnInFlight(for: .messages)
        let reDrain = await cache.drainNewlyAdded(for: .messages)
        XCTAssertTrue(reDrain.isEmpty)
        // confirmDrained has NO baseline side effect — baseline still
        // trails the latest fetch.
        let base = await cache.baseline(for: .messages)
        XCTAssertEqual(base, ["+15550000001", "+15550000003"])
    }

    func testInFlightExcludedFromReAdd() async {
        // Drain {B} (in-flight, not confirmed); replace({A,B}) again →
        // the bucket does NOT re-contain {B} (excluded via − inFlight).
        let cache = KnownIdentifiersCache(
            baselines: [.messages: ["+15550000001"]],
            consumers: [.messages])
        await cache.replace(with: ["+15550000001", "+15550000002"])
        let drained = await cache.drainNewlyAdded(for: .messages)
        XCTAssertEqual(drained, ["+15550000002"])
        // Same set re-fetched while B is still in-flight.
        await cache.replace(with: ["+15550000001", "+15550000002"])
        let reDrain = await cache.drainNewlyAdded(for: .messages)
        XCTAssertTrue(reDrain.isEmpty, "in-flight B must not re-enter the bucket")
    }

    func testEmptyCRMSeedsPresentEmptyBaseline() async {
        // First replace(∅) marks fetched + seeds baseline ∅; a later
        // replace({X}) drains {X} (NOT a fresh first-upgrade seed).
        let cache = KnownIdentifiersCache(consumers: [.messages])
        await cache.replace(with: [])
        let fetched = await cache.hasFetched
        XCTAssertTrue(fetched)
        let base = await cache.baseline(for: .messages)
        XCTAssertEqual(base, [])
        await cache.replace(with: ["+15550000009"])
        let drained = await cache.drainNewlyAdded(for: .messages)
        XCTAssertEqual(drained, ["+15550000009"])
    }

    // MARK: - persistableBaseline (single-writer baseline)

    func testPersistableBaselineNilWhenNoBaseline() async {
        let cache = KnownIdentifiersCache(consumers: [.messages])
        let pb = await cache.persistableBaseline(for: .messages)
        XCTAssertNil(pb, "noBaseline → nil persistable baseline")
    }

    func testFirstUpgradeSeedPersistsFullSetWithZeroScans() async {
        // First-upgrade replace → pendingNewlyAdded empty →
        // persistableBaseline == fetched (full seed persists, zero
        // scans).
        let cache = KnownIdentifiersCache(consumers: [.messages])
        await cache.replace(with: ["+15550000001", "+15550000002"])
        let pb = await cache.persistableBaseline(for: .messages)
        XCTAssertEqual(pb, ["+15550000001", "+15550000002"])
        let drained = await cache.drainNewlyAdded(for: .messages)
        XCTAssertTrue(drained.isEmpty)
    }

    func testOwedScanExcludedFromPersistableUntilDurablyEnqueued() async {
        // replace({A,X}) against baseline {A} → X in pendingNewlyAdded →
        // persistableBaseline == {A} (X EXCLUDED).
        let cache = KnownIdentifiersCache(
            baselines: [.messages: ["+15550000001"]],
            consumers: [.messages])
        await cache.replace(with: ["+15550000001", "+15550000009"])
        let pbBeforeDrain = await cache.persistableBaseline(for: .messages)
        XCTAssertEqual(pbBeforeDrain, ["+15550000001"], "owed-but-not-drained X excluded")

        // Drain moves X to in-flight: still owed, still excluded.
        let drained = await cache.drainNewlyAdded(for: .messages)
        XCTAssertEqual(drained, ["+15550000009"])
        let pbInFlight = await cache.persistableBaseline(for: .messages)
        XCTAssertEqual(pbInFlight, ["+15550000001"], "in-flight X still excluded")

        // After confirmDrained (scan durably committed), X is in neither
        // owed set → persistableBaseline advances to include it.
        await cache.confirmDrained(for: .messages)
        let pbAfterConfirm = await cache.persistableBaseline(for: .messages)
        XCTAssertEqual(pbAfterConfirm, ["+15550000001", "+15550000009"])
    }

    func testCrashBeforeDurableEnqueueReEnqueuesOnRestart() async {
        // Simulate the P0: refresher persists {A} (X excluded). A crash
        // before the source tick durably commits X. Restart re-seeds
        // baseline {A}; replace({A,X}) re-detects X as new.
        let preCrash = KnownIdentifiersCache(
            baselines: [.messages: ["+15550000001"]],
            consumers: [.messages])
        await preCrash.replace(with: ["+15550000001", "+15550000009"])
        let persisted = await preCrash.persistableBaseline(for: .messages) ?? []
        XCTAssertEqual(persisted, ["+15550000001"])

        // Restart with the persisted baseline {A} (X was NOT persisted).
        let postCrash = KnownIdentifiersCache(
            baselines: [.messages: persisted],
            consumers: [.messages])
        await postCrash.replace(with: ["+15550000001", "+15550000009"])
        let reDetected = await postCrash.drainNewlyAdded(for: .messages)
        XCTAssertEqual(reDetected, ["+15550000009"], "X re-enqueued — scan not lost")
    }

    func testRemovalShrinksPersistableBaseline() async {
        let cache = KnownIdentifiersCache(
            baselines: [.messages: ["+15550000001", "+15550000002"]],
            consumers: [.messages])
        await cache.replace(with: ["+15550000001"])
        let pb = await cache.persistableBaseline(for: .messages)
        XCTAssertEqual(pb, ["+15550000001"], "removal folds into persistable baseline")
    }
}
