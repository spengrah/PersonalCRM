// Tests for the `crm-mac configure anarlog` mutation contract.
//
// The AnarlogSubcommand lives in the `crm-mac` executable target
// (no test target by design); these tests exercise the same
// ConfigStore code paths the command runs to prove the config-write
// behavior: enable/disable flag persistence and
// top-level key preservation across mutations.
//
// The cursor-reset handshake is covered by AnarlogCursorResetTests
// against the testable AnarlogCursorReset helper. Daemon-running
// rejection is enforced by `requireDaemonNotRunning` at the CLI
// entry point; the predicate is the pidfile-exists check shared
// with the containers subcommand.
import XCTest
import Foundation
import CRMMacCore

final class ConfigureCommandAnarlogTests: XCTestCase {
    private var tempDir: URL!
    private var configURL: URL!
    private var store: ConfigStore!

    override func setUpWithError() throws {
        try super.setUpWithError()
        tempDir = FileManager.default.temporaryDirectory
            .appendingPathComponent("configure-anarlog-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: tempDir, withIntermediateDirectories: true)
        configURL = tempDir.appendingPathComponent("config.json")
        store = ConfigStore(fileURL: configURL)
        try seedBaseConfig()
    }

    override func tearDownWithError() throws {
        try? FileManager.default.removeItem(at: tempDir)
        try super.tearDownWithError()
    }

    private func seedBaseConfig() throws {
        try store.save(DaemonConfig(
            piURL: URL(string: "https://test.invalid")!,
            hostID: UUID(uuidString: "11111111-2222-3333-4444-555555555555")!,
            hostname: "host",
            installedAt: Date(timeIntervalSince1970: 1_700_000_000)))
    }

    // TC-CFG1: --enable both persists with both flags true.
    func testEnableBothPersistsToConfig() throws {
        var cfg = try store.loadAnarlogConfig() ??
            AnarlogConfig()
        XCTAssertNil(try store.loadAnarlogConfig(), "precondition: no anarlog config yet")
        cfg.humansEnabled = true
        cfg.sessionsEnabled = true
        try store.saveAnarlogConfig(cfg)
        let loaded = try XCTUnwrap(try store.loadAnarlogConfig())
        XCTAssertTrue(loaded.humansEnabled)
        XCTAssertTrue(loaded.sessionsEnabled)
    }

    // TC-CFG2: --enable humans only flips humans; sessions stays as-is.
    func testEnableHumansOnlyLeavesSessionsUntouched() throws {
        try store.saveAnarlogConfig(AnarlogConfig(
            humansEnabled: false,
            sessionsEnabled: true))
        var cfg = try XCTUnwrap(try store.loadAnarlogConfig())
        cfg.humansEnabled = true
        try store.saveAnarlogConfig(cfg)
        let loaded = try XCTUnwrap(try store.loadAnarlogConfig())
        XCTAssertTrue(loaded.humansEnabled)
        XCTAssertTrue(loaded.sessionsEnabled, "sessions must remain unchanged")
    }

    // TC-CFG3: --disable both flips both flags.
    func testDisableBothFlipsBothFlags() throws {
        try store.saveAnarlogConfig(AnarlogConfig(
            humansEnabled: true,
            sessionsEnabled: true))
        var cfg = try XCTUnwrap(try store.loadAnarlogConfig())
        cfg.humansEnabled = false
        cfg.sessionsEnabled = false
        try store.saveAnarlogConfig(cfg)
        let loaded = try XCTUnwrap(try store.loadAnarlogConfig())
        XCTAssertFalse(loaded.humansEnabled)
        XCTAssertFalse(loaded.sessionsEnabled)
    }

    // Round-trip: persist + reload + persist again preserves everything.
    func testRoundTripPreservesTopLevelKeys() throws {
        let original = try store.load()
        try store.saveAnarlogConfig(AnarlogConfig(
            humansEnabled: true,
            sessionsEnabled: false))
        let updated = try store.load()
        XCTAssertEqual(updated.piURL, original.piURL)
        XCTAssertEqual(updated.hostID, original.hostID)
        XCTAssertEqual(updated.hostname, original.hostname)
        XCTAssertEqual(updated.installedAt, original.installedAt)
    }
}
