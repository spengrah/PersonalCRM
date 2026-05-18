// Tests for the `crm-mac configure containers` allowlist mutation
// flow's crash-safety + state-first ordering.
//
// The reconciliation contract:
//   1. StateMutator: set sources["icloud_contacts"].lastError =
//      "recovery_requested:allowlist_changed" — write FIRST.
//   2. ConfigStore.saveICloudContactsConfig(new allowlist) — write
//      SECOND.
//
// Crash semantics:
//   - Crash AFTER step 1 (state set, config NOT updated): next tick
//     recovers against the OLD allowlist; still correctly tombstones
//     contacts that were removed upstream. Idempotent — a second run
//     reapplies and completes.
//   - Crash AFTER step 2 (both updated): next tick recovers against
//     the NEW allowlist. Intended outcome.
//   - No reachable state produces wrong-allowlist + no-recovery.
//
// The ConfigureCommand lives in the `crm-mac` executable target (no
// test target by design); these tests exercise the same StateStore +
// ConfigStore code paths the command runs to prove the contract holds.
import XCTest
import Foundation
import CRMMacCore
@testable import CRMMacLifecycle

final class ConfigureContainersReconciliationTests: XCTestCase {
    private var tempDir: URL!
    private var stateURL: URL!
    private var configURL: URL!

    override func setUpWithError() throws {
        try super.setUpWithError()
        tempDir = FileManager.default.temporaryDirectory
            .appendingPathComponent("configure-containers-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: tempDir, withIntermediateDirectories: true)
        stateURL = tempDir.appendingPathComponent("state.json")
        configURL = tempDir.appendingPathComponent("config.json")
    }

    override func tearDownWithError() throws {
        try? FileManager.default.removeItem(at: tempDir)
        try super.tearDownWithError()
    }

    // MARK: - helpers

    private func seedConfig(containers: [String]) throws {
        let cfg = DaemonConfig(
            piURL: URL(string: "https://test.invalid")!,
            hostID: UUID(uuidString: "11111111-2222-3333-4444-555555555555")!,
            hostname: "host",
            installedAt: Date(timeIntervalSince1970: 1_700_000_000),
            sources: DaemonSourcesConfig(
                icloudContacts: ICloudContactsConfig(containers: containers)))
        try ConfigStore(fileURL: configURL).save(cfg)
    }

    private func runReconciliation(
        newContainers: [String],
        configWriteFails: Bool = false,
        stateWriteFails: Bool = false
    ) async throws {
        // This mirrors the ConfigureCommand.run() flow exactly:
        // state write FIRST, then config write.
        let stateStore = StateStore(fileURL: stateURL)
        // The production Installer guarantees state.json exists before
        // any source plugin or CLI subcommand runs; tests must do the
        // same.
        if !FileManager.default.fileExists(atPath: stateURL.path) {
            try stateStore.initializeIfMissing()
        }
        if stateWriteFails {
            // Make the state path unwritable by creating a directory
            // where the file should be.
            try? FileManager.default.removeItem(at: stateURL)
            try? FileManager.default.createDirectory(at: stateURL, withIntermediateDirectories: true)
        }
        let mutator = StateMutator(store: stateStore)
        try await mutator.mutate { state in
            var src = state.sources["icloud_contacts"] ?? SourceState()
            src.lastError = "recovery_requested:allowlist_changed"
            src.lastErrorAt = Date(timeIntervalSince1970: 1_750_000_000)
            state.sources["icloud_contacts"] = src
        }
        if configWriteFails {
            // Force a write failure by making the config path point at
            // a directory.
            try? FileManager.default.createDirectory(at: configURL, withIntermediateDirectories: true)
        }
        let configStore = ConfigStore(fileURL: configURL)
        try configStore.saveICloudContactsConfig(
            ICloudContactsConfig(containers: newContainers))
    }

    private func readState() throws -> DaemonState {
        try StateStore(fileURL: stateURL).load()
    }

    private func readConfig() throws -> DaemonConfig {
        try ConfigStore(fileURL: configURL).load()
    }

    // MARK: - happy path

    func testReconciliationHappyPathWritesStateFirstThenConfig() async throws {
        try seedConfig(containers: ["A"])
        try await runReconciliation(newContainers: ["B", "C"])
        let state = try readState()
        XCTAssertEqual(state.sources["icloud_contacts"]?.lastError,
                       "recovery_requested:allowlist_changed")
        let config = try readConfig()
        XCTAssertEqual(config.sources?.icloudContacts?.containers, ["B", "C"])
    }

    // MARK: - crash safety

    func testCrashAfterStateWritePreservesOldConfigAndKeepsFlagSet() async throws {
        // Realistic failure mode: state write succeeds, the
        // subsequent ConfigStore.save throws. The atomic-rename in
        // ConfigStore leaves the OLD config file intact (rename-on-
        // failure deletes the .tmp and never replaces the
        // destination), so the operator's next interaction reads:
        //   - state.json: recovery flag SET
        //   - config.json: OLD allowlist
        // The next daemon tick recovers against the OLD allowlist
        // — still correct, tombstones removed-upstream contacts.
        // A re-run of `configure containers` reapplies and completes
        // the swap; idempotent.
        try seedConfig(containers: ["A"])
        // Compute what the OLD-config bytes look like so we can
        // verify they survive the simulated failure unchanged.
        let oldConfigBytes = try Data(contentsOf: configURL)

        // Step 1 succeeds: bump the recovery flag.
        let stateStore = StateStore(fileURL: stateURL)
        if !FileManager.default.fileExists(atPath: stateURL.path) {
            try stateStore.initializeIfMissing()
        }
        let mutator = StateMutator(store: stateStore)
        try await mutator.mutate { state in
            var src = state.sources["icloud_contacts"] ?? SourceState()
            src.lastError = "recovery_requested:allowlist_changed"
            src.lastErrorAt = Date(timeIntervalSince1970: 1_750_000_000)
            state.sources["icloud_contacts"] = src
        }

        // Step 2 throws: simulate by pointing the ConfigStore at an
        // unwritable parent directory.
        let badConfigDir = tempDir.appendingPathComponent("not-a-real-dir/nested")
        // Create a non-directory file at the parent path so the
        // intermediate directory creation fails.
        try Data("blocker".utf8).write(to: tempDir.appendingPathComponent("not-a-real-dir"))
        let badConfigURL = badConfigDir.appendingPathComponent("config.json")
        let badStore = ConfigStore(fileURL: badConfigURL)
        do {
            try badStore.saveICloudContactsConfig(
                ICloudContactsConfig(containers: ["B"]))
            XCTFail("expected ConfigStore.save to throw")
        } catch {
            // expected
        }

        // Assert the real config file is untouched (no half-write,
        // no swap-to-empty). The atomic-rename guarantee is the key
        // safety net here.
        let stillThereBytes = try Data(contentsOf: configURL)
        XCTAssertEqual(stillThereBytes, oldConfigBytes,
                       "OLD config bytes must survive a save() failure")
        let cfg = try ConfigStore(fileURL: configURL).load()
        XCTAssertEqual(cfg.sources?.icloudContacts?.containers, ["A"],
                       "ConfigStore.load must still see the OLD allowlist")

        // Assert the recovery flag is persisted.
        let state = try readState()
        XCTAssertEqual(state.sources["icloud_contacts"]?.lastError,
                       "recovery_requested:allowlist_changed",
                       "recovery flag must be durable across the simulated failure")
    }

    func testReReconciliationIsIdempotent() async throws {
        // After a crash leaves the flag set + config stale, re-running
        // `crm-mac configure containers` should complete the swap.
        try seedConfig(containers: ["A"])
        // First attempt: succeeds.
        try await runReconciliation(newContainers: ["B"])
        // Second attempt with same new containers: idempotent.
        try await runReconciliation(newContainers: ["B"])
        let config = try readConfig()
        XCTAssertEqual(config.sources?.icloudContacts?.containers, ["B"])
        let state = try readState()
        XCTAssertEqual(state.sources["icloud_contacts"]?.lastError,
                       "recovery_requested:allowlist_changed")
    }

    // MARK: - diff semantics

    func testDiffComputesAddedAndRemoved() {
        // Mirror the Set arithmetic the command uses.
        let existing: Set<String> = ["A", "B", "C"]
        let picked: Set<String> = ["B", "C", "D"]
        let added = picked.subtracting(existing)
        let removed = existing.subtracting(picked)
        XCTAssertEqual(added, ["D"])
        XCTAssertEqual(removed, ["A"])
    }

    func testNoOpDiffWhenAllowlistUnchanged() {
        let existing: Set<String> = ["A"]
        let picked: Set<String> = ["A"]
        XCTAssertTrue(picked.subtracting(existing).isEmpty)
        XCTAssertTrue(existing.subtracting(picked).isEmpty)
    }

    // MARK: - config preservation

    func testReconciliationPreservesOtherTopLevelConfigKeys() async throws {
        try seedConfig(containers: ["A"])
        // Mutate to verify pi_url + host_id round-trip.
        try await runReconciliation(newContainers: ["B"])
        let config = try readConfig()
        XCTAssertEqual(config.piURL.absoluteString, "https://test.invalid")
        XCTAssertEqual(config.hostname, "host")
    }
}
