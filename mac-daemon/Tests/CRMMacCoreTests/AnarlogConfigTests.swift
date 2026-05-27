// AnarlogConfigTests cover the ConfigStore extension that loads +
// saves the anarlog reader config alongside the rest of the daemon's
// config. Backward-compat with older config.json files (no `sources`
// key OR `sources` carrying only `icloud_contacts`) is the critical
// invariant — operators who haven't yet enabled anarlog readers must
// continue to load + save correctly.
import XCTest
@testable import CRMMacCore

final class AnarlogConfigTests: XCTestCase {

    private var tmpDir: URL!
    private var fileURL: URL!
    private var store: ConfigStore!

    override func setUp() {
        super.setUp()
        tmpDir = URL(fileURLWithPath: NSTemporaryDirectory())
            .appendingPathComponent(UUID().uuidString)
        try? FileManager.default.createDirectory(
            at: tmpDir, withIntermediateDirectories: true)
        fileURL = tmpDir.appendingPathComponent("config.json")
        store = ConfigStore(fileURL: fileURL)
    }

    override func tearDown() {
        try? FileManager.default.removeItem(at: tmpDir)
        super.tearDown()
    }

    func testLoadReturnsNilWhenSourcesKeyMissing() throws {
        try seedBackwardCompatibleConfig()
        let result = try store.loadAnarlogConfig()
        XCTAssertNil(result)
    }

    func testLoadReturnsNilWhenSourcesHasOnlyIcloud() throws {
        // Existing operator with icloud_contacts configured but no
        // anarlog block — adding anarlog must not corrupt that path.
        try seedBackwardCompatibleConfig()
        try store.saveICloudContactsConfig(ICloudContactsConfig(containers: ["container-1"]))
        let result = try store.loadAnarlogConfig()
        XCTAssertNil(result)
    }

    func testSaveAndReload() throws {
        try seedBackwardCompatibleConfig()
        let cfg = AnarlogConfig(
            rootPath: "/Users/test/Documents/notes/meetings",
            humansEnabled: true,
            sessionsEnabled: false)
        try store.saveAnarlogConfig(cfg)
        let loaded = try store.loadAnarlogConfig()
        XCTAssertEqual(loaded, cfg)
    }

    func testSavePreservesTopLevelKeys() throws {
        try seedBackwardCompatibleConfig()
        let originalDaemon = try store.load()
        let cfg = AnarlogConfig(
            rootPath: "/tmp/notes",
            humansEnabled: true,
            sessionsEnabled: true)
        try store.saveAnarlogConfig(cfg)
        let updated = try store.load()
        XCTAssertEqual(updated.piURL, originalDaemon.piURL)
        XCTAssertEqual(updated.hostID, originalDaemon.hostID)
        XCTAssertEqual(updated.hostname, originalDaemon.hostname)
        XCTAssertEqual(updated.installedAt, originalDaemon.installedAt)
        XCTAssertEqual(updated.sources?.anarlog, cfg)
    }

    func testSavePreservesIcloudContactsAlongside() throws {
        try seedBackwardCompatibleConfig()
        let icloud = ICloudContactsConfig(containers: ["container-1"])
        try store.saveICloudContactsConfig(icloud)
        let anarlog = AnarlogConfig(
            rootPath: "/tmp/notes",
            humansEnabled: true,
            sessionsEnabled: false)
        try store.saveAnarlogConfig(anarlog)
        let updated = try store.load()
        XCTAssertEqual(updated.sources?.icloudContacts, icloud)
        XCTAssertEqual(updated.sources?.anarlog, anarlog)
    }

    func testDecodeFromWireSnakeCaseKeys() throws {
        // The on-disk JSON uses snake_case keys; verify the explicit
        // CodingKeys map correctly.
        let body = """
        {
          "host_id": "00000000-0000-0000-0000-000000000001",
          "hostname": "test-host",
          "installed_at": "2026-01-01T00:00:00Z",
          "pi_url": "https://pi.example.invalid",
          "sources": {
            "anarlog": {
              "root_path": "/tmp/notes/meetings",
              "humans_enabled": true,
              "sessions_enabled": false
            }
          }
        }
        """
        try Data(body.utf8).write(to: fileURL)
        let loaded = try store.loadAnarlogConfig()
        XCTAssertEqual(loaded?.rootPath, "/tmp/notes/meetings")
        XCTAssertEqual(loaded?.humansEnabled, true)
        XCTAssertEqual(loaded?.sessionsEnabled, false)
    }

    func testDefaultEnableFlagsAreFalse() {
        let cfg = AnarlogConfig(rootPath: "/tmp/notes")
        XCTAssertFalse(cfg.humansEnabled)
        XCTAssertFalse(cfg.sessionsEnabled)
    }

    func testSourceIDConstants() {
        XCTAssertEqual(SourceID.anarlogHumans.rawValue, "anarlog_humans")
        XCTAssertEqual(SourceID.anarlogSessions.rawValue, "anarlog_sessions")
    }

    // MARK: - helpers

    private func seedBackwardCompatibleConfig() throws {
        let body = """
        {
          "host_id": "00000000-0000-0000-0000-000000000001",
          "hostname": "test-host",
          "installed_at": "2026-01-01T00:00:00Z",
          "pi_url": "https://pi.example.invalid"
        }
        """
        try Data(body.utf8).write(to: fileURL)
    }
}
