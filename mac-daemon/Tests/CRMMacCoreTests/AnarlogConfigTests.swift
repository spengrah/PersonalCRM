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
              "humans_enabled": true,
              "sessions_enabled": false
            }
          }
        }
        """
        try Data(body.utf8).write(to: fileURL)
        let loaded = try store.loadAnarlogConfig()
        XCTAssertEqual(loaded?.humansEnabled, true)
        XCTAssertEqual(loaded?.sessionsEnabled, false)
    }

    func testDefaultEnableFlagsAreFalse() {
        let cfg = AnarlogConfig()
        XCTAssertFalse(cfg.humansEnabled)
        XCTAssertFalse(cfg.sessionsEnabled)
    }

    func testSourceIDConstants() {
        XCTAssertEqual(SourceID.anarlogHumans.rawValue, "anarlog_humans")
        XCTAssertEqual(SourceID.anarlogSessions.rawValue, "anarlog_sessions")
    }

    func testLegacyAnarlogJSONDefaultsNewFields() throws {
        let body = """
        {
          "host_id": "00000000-0000-0000-0000-000000000001",
          "hostname": "test-host",
          "installed_at": "2026-01-01T00:00:00Z",
          "pi_url": "https://pi.example.invalid",
          "sources": {
            "anarlog": {
              "root_path": "/tmp/notes",
              "humans_enabled": true,
              "sessions_enabled": false
            }
          }
        }
        """
        try Data(body.utf8).write(to: fileURL)

        let loaded = try XCTUnwrap(store.loadAnarlogConfig())

        XCTAssertTrue(loaded.humansEnabled)
        XCTAssertFalse(loaded.sessionsEnabled)
        XCTAssertNil(loaded.operatorPersonID)
        XCTAssertNil(loaded.cliPath)
        XCTAssertEqual(loaded.deletionCap, 5)
    }

    func testLegacyRootPathIsIgnoredAndDroppedOnSave() throws {
        try seedBackwardCompatibleConfig()
        let body = """
        {
          "host_id": "00000000-0000-0000-0000-000000000001",
          "hostname": "test-host",
          "installed_at": "2026-01-01T00:00:00Z",
          "pi_url": "https://pi.example.invalid",
          "sources": {
            "anarlog": {
              "root_path": "/tmp/notes",
              "humans_enabled": true,
              "sessions_enabled": true,
              "operator_person_id": "aaaaaaaa-0000-4000-8000-000000000001"
            }
          }
        }
        """
        try Data(body.utf8).write(to: fileURL)

        let loaded = try XCTUnwrap(store.loadAnarlogConfig())
        XCTAssertTrue(loaded.humansEnabled)
        XCTAssertTrue(loaded.sessionsEnabled)
        XCTAssertEqual(loaded.operatorPersonID, "aaaaaaaa-0000-4000-8000-000000000001")

        try store.saveAnarlogConfig(loaded)

        let saved = try XCTUnwrap(
            JSONSerialization.jsonObject(with: Data(contentsOf: fileURL)) as? [String: Any])
        let sources = try XCTUnwrap(saved["sources"] as? [String: Any])
        let anarlog = try XCTUnwrap(sources["anarlog"] as? [String: Any])
        XCTAssertNil(anarlog["root_path"])
        XCTAssertEqual(anarlog["humans_enabled"] as? Bool, true)
        XCTAssertEqual(anarlog["sessions_enabled"] as? Bool, true)
    }

    func testRoundTripPreservesAllAnarlogFields() throws {
        try seedBackwardCompatibleConfig()
        var cfg = AnarlogConfig(
            humansEnabled: true,
            sessionsEnabled: false)
        try cfg.setOperatorPersonID("AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE")
        try cfg.setCLIPath("/opt/anarlog/bin/anarlog")
        try cfg.setDeletionCap(7)

        try store.saveAnarlogConfig(cfg)
        let loaded = try store.loadAnarlogConfig()

        XCTAssertEqual(loaded, cfg)
    }

    func testSettersAcceptValidValues() throws {
        var cfg = AnarlogConfig()

        try cfg.setOperatorPersonID("AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE")
        try cfg.setCLIPath("/opt/anarlog/bin/anarlog")
        try cfg.setDeletionCap(0)

        XCTAssertEqual(cfg.operatorPersonID, "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
        XCTAssertEqual(cfg.cliPath, "/opt/anarlog/bin/anarlog")
        XCTAssertEqual(cfg.deletionCap, 0)
    }

    func testSettersRejectInvalidValuesWithoutChangingProperties() throws {
        var cfg = AnarlogConfig()
        try cfg.setOperatorPersonID("11111111-2222-3333-4444-555555555555")
        try cfg.setCLIPath("/opt/anarlog/bin/anarlog")
        try cfg.setDeletionCap(7)

        XCTAssertThrowsError(try cfg.setOperatorPersonID("not-a-uuid")) { error in
            XCTAssertEqual(error as? AnarlogConfigError, .operatorPersonIDNotUUID("not-a-uuid"))
        }
        XCTAssertThrowsError(try cfg.setCLIPath("relative/anarlog")) { error in
            XCTAssertEqual(error as? AnarlogConfigError, .cliPathNotAbsolute("relative/anarlog"))
        }
        XCTAssertThrowsError(try cfg.setCLIPath("~/bin/anarlog")) { error in
            XCTAssertEqual(error as? AnarlogConfigError, .cliPathNotAbsolute("~/bin/anarlog"))
        }
        XCTAssertThrowsError(try cfg.setDeletionCap(-1)) { error in
            XCTAssertEqual(error as? AnarlogConfigError, .negativeDeletionCap(-1))
        }

        XCTAssertEqual(cfg.operatorPersonID, "11111111-2222-3333-4444-555555555555")
        XCTAssertEqual(cfg.cliPath, "/opt/anarlog/bin/anarlog")
        XCTAssertEqual(cfg.deletionCap, 7)
    }

    func testDecodeDoesNotValidatePersistedValues() throws {
        let body = """
        {
          "humans_enabled": false,
          "sessions_enabled": false,
          "operator_person_id": "not-a-uuid",
          "cli_path": "relative/anarlog",
          "deletion_cap": -1
        }
        """

        let loaded = try JSONDecoder().decode(AnarlogConfig.self, from: Data(body.utf8))

        XCTAssertEqual(loaded.operatorPersonID, "not-a-uuid")
        XCTAssertEqual(loaded.cliPath, "relative/anarlog")
        XCTAssertEqual(loaded.deletionCap, -1)
    }

    func testExplicitDeletionCapDecodes() throws {
        let body = """
        {
          "humans_enabled": false,
          "sessions_enabled": false,
          "deletion_cap": 12
        }
        """

        let loaded = try JSONDecoder().decode(AnarlogConfig.self, from: Data(body.utf8))

        XCTAssertEqual(loaded.deletionCap, 12)
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
