// Tests for `NonInteractiveAllowlistWriter` — the testable writer
// that lives between the CLI non-interactive `--containers` flag
// and the ConfigStore + StateStore. The writer's type signature
// deliberately excludes any ContactsAuthorizationAdapter or
// ContactContainerEnumerator parameter; this file documents and
// locks in that contract (test 6 below).
//
// Coverage:
//   1. Writing a new allowlist that differs from the existing one
//      bumps the recovery flag (when mutatingExistingConfig is true).
//   2. No-op short-circuit: picked == existing returns .noOp and
//      does NOT bump the recovery flag — the deliberate behaviour
//      improvement over the prior InstallCommand.runContactsPermissionFlow
//      which bumped unconditionally on --re-request-permission.
//   3. Fresh-install (mutatingExistingConfig: false, existing
//      allowlist absent) does NOT bump the recovery flag.
//   4. Defensive case: mutatingExistingConfig: false but existing
//      allowlist non-empty — bump the flag anyway because the
//      daemon may already be running with the old allowlist.
//   5. Crash-safety: a forced ConfigStore.save failure leaves the
//      state file's recovery flag set, proving state-write ran
//      first.
//   6. Type-signature regression guard: a no-body test that
//      references the writer's initializer to break the build if
//      a Contacts adapter parameter is ever added.
import XCTest
import Foundation
import CRMMacCore
@testable import CRMMacLifecycle

final class NonInteractiveAllowlistWriterTests: XCTestCase {
    private var tempDir: URL!
    private var stateURL: URL!
    private var configURL: URL!

    override func setUpWithError() throws {
        try super.setUpWithError()
        tempDir = FileManager.default.temporaryDirectory
            .appendingPathComponent("non-interactive-writer-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: tempDir, withIntermediateDirectories: true)
        stateURL = tempDir.appendingPathComponent("state.json")
        configURL = tempDir.appendingPathComponent("config.json")
    }

    override func tearDownWithError() throws {
        try? FileManager.default.removeItem(at: tempDir)
        try super.tearDownWithError()
    }

    // MARK: - helpers

    private func seedConfig(containers: [String]?) throws {
        let sources: DaemonSourcesConfig? = containers.map {
            DaemonSourcesConfig(icloudContacts: ICloudContactsConfig(containers: $0))
        }
        let cfg = DaemonConfig(
            piURL: URL(string: "https://test.invalid")!,
            hostID: UUID(uuidString: "11111111-2222-3333-4444-555555555555")!,
            hostname: "host",
            installedAt: Date(timeIntervalSince1970: 1_700_000_000),
            sources: sources)
        try ConfigStore(fileURL: configURL).save(cfg)
    }

    private func seedEmptyState() throws {
        try StateStore(fileURL: stateURL).initializeIfMissing()
    }

    private func readState() throws -> DaemonState {
        try StateStore(fileURL: stateURL).load()
    }

    private func readConfig() throws -> DaemonConfig {
        try ConfigStore(fileURL: configURL).load()
    }

    private func makeWriter(mutatingExistingConfig: Bool) -> NonInteractiveAllowlistWriter {
        NonInteractiveAllowlistWriter(
            configStore: ConfigStore(fileURL: configURL),
            stateStore: StateStore(fileURL: stateURL),
            mutatingExistingConfig: mutatingExistingConfig)
    }

    // MARK: - 1. mutating-existing path

    func testWriteWritesConfigAndBumpsRecoveryFlagWhenMutatingExisting() async throws {
        try seedConfig(containers: ["A"])
        try seedEmptyState()

        let writer = makeWriter(mutatingExistingConfig: true)
        let outcome = try await writer.write(pickedIDs: ["B"])

        XCTAssertEqual(outcome, .wrote(pickedIDs: ["B"]))
        let cfg = try readConfig()
        XCTAssertEqual(cfg.sources?.icloudContacts?.containers, ["B"])
        let state = try readState()
        XCTAssertEqual(
            state.sources["icloud_contacts"]?.lastError,
            "recovery_requested:allowlist_changed",
            "recovery flag must be set when mutating an existing config")
    }

    // MARK: - 2. no-op short-circuit

    func testWriteReturnsNoOpWhenSetsMatch() async throws {
        // Seed existing config ["A", "B"]; pick same set in
        // different order. Writer should yield .noOp WITHOUT
        // bumping the recovery flag. This locks in the deliberate
        // semantic improvement over the prior install flow which
        // bumped unconditionally on --re-request-permission.
        try seedConfig(containers: ["A", "B"])
        try seedEmptyState()
        let configBytesBefore = try Data(contentsOf: configURL)

        let writer = makeWriter(mutatingExistingConfig: true)
        let outcome = try await writer.write(pickedIDs: ["B", "A"])

        XCTAssertEqual(outcome, .noOp)
        let state = try readState()
        XCTAssertNil(
            state.sources["icloud_contacts"]?.lastError,
            "no-op must NOT trigger a recovery flag bump")
        XCTAssertEqual(
            try Data(contentsOf: configURL),
            configBytesBefore,
            "no-op must NOT touch config.json on disk")
    }

    // MARK: - 3. fresh-install path

    func testWriteOnFreshInstallSkipsRecoveryFlag() async throws {
        // Preconditions match the production installer's invariant:
        // a config.json exists (installer.run writes it before
        // runContactsPermissionFlow is called) but the
        // sources.icloud_contacts sub-key is absent. State file
        // exists but has no icloud_contacts entry.
        try seedConfig(containers: nil)
        try seedEmptyState()

        let writer = makeWriter(mutatingExistingConfig: false)
        let outcome = try await writer.write(pickedIDs: ["X"])

        XCTAssertEqual(outcome, .wrote(pickedIDs: ["X"]))
        let cfg = try readConfig()
        XCTAssertEqual(cfg.sources?.icloudContacts?.containers, ["X"])
        let state = try readState()
        XCTAssertNil(
            state.sources["icloud_contacts"]?.lastError,
            "fresh install must NOT trigger a recovery flag bump")
    }

    // MARK: - 4. defensive: non-empty existing + mutatingExistingConfig=false

    func testWriteOnExistingConfigBumpsRecoveryFlagEvenWhenMutatingExistingFalse() async throws {
        // Hypothetical caller passes mutatingExistingConfig=false
        // even though the existing allowlist is non-empty (a
        // corrupted-state or programmer-error scenario). The writer
        // is defensively safer than today: bump the flag because
        // the daemon may already be running with the old allowlist.
        try seedConfig(containers: ["A"])
        try seedEmptyState()

        let writer = makeWriter(mutatingExistingConfig: false)
        let outcome = try await writer.write(pickedIDs: ["B"])

        XCTAssertEqual(outcome, .wrote(pickedIDs: ["B"]))
        let state = try readState()
        XCTAssertEqual(
            state.sources["icloud_contacts"]?.lastError,
            "recovery_requested:allowlist_changed",
            "non-empty existing allowlist must trigger a recovery flag bump regardless of mutatingExistingConfig")
    }

    // MARK: - 5. crash-safety: state-first then config

    func testWriteCrashSafetyStateFirstThenConfig() async throws {
        // Seed valid state but point the writer at a configStore
        // whose save path is unwritable. The StateMutator runs
        // first against the real state file and succeeds; the
        // configStore.save throws second. After the failure the
        // state file MUST still have the recovery flag set —
        // proving state-write ran before config-write.
        try seedEmptyState()
        // The "config store" the writer holds points at a directory,
        // so `loadICloudContactsConfig()` throws and is folded to
        // [] by the writer's `(try? …) ?? []`. The subsequent
        // `save(_:)` then also fails — the path is a directory.
        // `mutatingExistingConfig: true` keeps the state-write
        // branch active regardless of `existing` being empty.
        let badConfigURL = tempDir.appendingPathComponent("config-as-dir")
        try FileManager.default.createDirectory(at: badConfigURL, withIntermediateDirectories: true)
        let configStore = ConfigStore(fileURL: badConfigURL)
        let stateStore = StateStore(fileURL: stateURL)

        let writer = NonInteractiveAllowlistWriter(
            configStore: configStore,
            stateStore: stateStore,
            mutatingExistingConfig: true)

        do {
            _ = try await writer.write(pickedIDs: ["B"])
            XCTFail("expected write to throw when config path is unwritable")
        } catch NonInteractiveAllowlistWriteError.configWriteFailedAfterStateWrite {
            // expected: state-write succeeded, config-write failed
            // → callers should surface the "recovery flag is set;
            // re-run to retry" guidance.
        } catch {
            XCTFail("expected configWriteFailedAfterStateWrite, got: \(error)")
        }

        // State write ran first and persisted.
        let state = try readState()
        XCTAssertEqual(
            state.sources["icloud_contacts"]?.lastError,
            "recovery_requested:allowlist_changed",
            "state-write must persist even when the subsequent config-write fails")
    }

    // MARK: - 5b. error-classification for fresh-install config-fail

    func testWriteOnFreshInstallSurfacesPlainConfigWriteFailedError() async throws {
        // Fresh-install + no existing allowlist + sabotaged config
        // path: no state-write should run, and the resulting error
        // should be the plain `.configWriteFailed` variant (NOT the
        // partial-write variant). This drives the CLI wrapper to
        // emit the simpler "Failed to write config.json" message
        // without the "recovery flag is set; re-run" guidance.
        try seedEmptyState()
        let badConfigURL = tempDir.appendingPathComponent("config-as-dir")
        try FileManager.default.createDirectory(at: badConfigURL, withIntermediateDirectories: true)
        let writer = NonInteractiveAllowlistWriter(
            configStore: ConfigStore(fileURL: badConfigURL),
            stateStore: StateStore(fileURL: stateURL),
            mutatingExistingConfig: false)

        do {
            _ = try await writer.write(pickedIDs: ["X"])
            XCTFail("expected configWriteFailed")
        } catch NonInteractiveAllowlistWriteError.configWriteFailed {
            // expected
        } catch {
            XCTFail("expected configWriteFailed, got: \(error)")
        }

        // No state-write fired.
        let state = try readState()
        XCTAssertNil(
            state.sources["icloud_contacts"]?.lastError,
            "fresh install with no existing allowlist must not bump the recovery flag")
    }

    // MARK: - 6. type-signature regression guard

    func testWriterTypeSignatureExcludesContactsAdapters() {
        // The point of `NonInteractiveAllowlistWriter` is that its
        // initializer accepts ONLY ConfigStore + StateStore + Bool.
        // If a future contributor adds a ContactsAuthorizationAdapter
        // or ContactContainerEnumerator parameter, this file (and
        // every production call site) breaks the build until the
        // change is reverted. The reference below to the
        // metatype proves the type is still in scope; the real
        // assertion is structural at compile time.
        let writerType: NonInteractiveAllowlistWriter.Type = NonInteractiveAllowlistWriter.self
        XCTAssertNotNil(writerType)

        // Belt-and-suspenders: grep the writer source file's
        // executable code (after stripping line and block comments)
        // for any re-introduced Contacts adapter symbols. Comments
        // are allowed to mention these symbols by name as part of
        // the contract documentation; what matters is that no
        // production code path imports or invokes them.
        let testFile = URL(fileURLWithPath: #filePath)
        let writerSource = testFile
            .deletingLastPathComponent()                 // CRMMacLifecycleTests
            .deletingLastPathComponent()                 // Tests
            .deletingLastPathComponent()                 // mac-daemon
            .appendingPathComponent("Sources/CRMMacLifecycle/NonInteractiveAllowlistWriter.swift")
        guard let bytes = try? Data(contentsOf: writerSource),
              let text = String(data: bytes, encoding: .utf8) else {
            XCTFail("could not read writer source at \(writerSource.path)")
            return
        }
        let codeOnly = Self.stripSwiftComments(text)
        let forbiddenSymbols = [
            "ContactsAuthorizationAdapter",
            "ContactContainerEnumerator",
            "requestAccess",
            "listContainers",
        ]
        for symbol in forbiddenSymbols {
            XCTAssertFalse(
                codeOnly.contains(symbol),
                "NonInteractiveAllowlistWriter must not reference \(symbol) in executable code. See \(writerSource.path).")
        }
    }

    /// Remove `//` line comments and `/* … */` block comments from
    /// a Swift source string. Naive (no string-literal awareness)
    /// but sufficient for the writer file which has no string
    /// literals containing the forbidden symbols.
    private static func stripSwiftComments(_ source: String) -> String {
        var result = ""
        var i = source.startIndex
        var inBlockComment = false
        var inLineComment = false
        while i < source.endIndex {
            let c = source[i]
            let next = source.index(after: i) < source.endIndex
                ? source[source.index(after: i)]
                : Character("\0")
            if inBlockComment {
                if c == "*" && next == "/" {
                    inBlockComment = false
                    i = source.index(i, offsetBy: 2)
                    continue
                }
                i = source.index(after: i)
                continue
            }
            if inLineComment {
                if c == "\n" {
                    inLineComment = false
                    result.append(c)
                }
                i = source.index(after: i)
                continue
            }
            if c == "/" && next == "/" {
                inLineComment = true
                i = source.index(i, offsetBy: 2)
                continue
            }
            if c == "/" && next == "*" {
                inBlockComment = true
                i = source.index(i, offsetBy: 2)
                continue
            }
            result.append(c)
            i = source.index(after: i)
        }
        return result
    }
}
