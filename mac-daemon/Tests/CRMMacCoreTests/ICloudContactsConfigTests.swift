// ICloudContactsConfigTests cover the ConfigStore extension that
// loads + saves the icloud_contacts allowlist alongside the rest of
// the daemon's config. Backward compatibility with pre-PR8b
// config.json files (no `sources` key) is the critical invariant.
import XCTest
@testable import CRMMacCore

final class ICloudContactsConfigTests: XCTestCase {

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
        let result = try store.loadICloudContactsConfig()
        XCTAssertNil(result)
    }

    func testSaveAndReload() throws {
        try seedBackwardCompatibleConfig()
        let cfg = ICloudContactsConfig(containers: ["container-1", "container-2"])
        try store.saveICloudContactsConfig(cfg)
        let loaded = try store.loadICloudContactsConfig()
        XCTAssertEqual(loaded, cfg)
    }

    func testSavePreservesTopLevelKeys() throws {
        try seedBackwardCompatibleConfig()
        let originalDaemon = try store.load()
        let cfg = ICloudContactsConfig(containers: ["container-1"])
        try store.saveICloudContactsConfig(cfg)
        let updated = try store.load()
        XCTAssertEqual(updated.piURL, originalDaemon.piURL)
        XCTAssertEqual(updated.hostID, originalDaemon.hostID)
        XCTAssertEqual(updated.hostname, originalDaemon.hostname)
        XCTAssertEqual(updated.installedAt, originalDaemon.installedAt)
        XCTAssertEqual(updated.sources?.icloudContacts, cfg)
    }

    func testEmptyAllowlistRoundTrip() throws {
        try seedBackwardCompatibleConfig()
        let cfg = ICloudContactsConfig(containers: [])
        try store.saveICloudContactsConfig(cfg)
        let loaded = try store.loadICloudContactsConfig()
        XCTAssertEqual(loaded, cfg)
        XCTAssertEqual(loaded?.containers, [])
    }

    // MARK: - helpers

    private func seedBackwardCompatibleConfig() throws {
        // Hand-write a config file matching the pre-PR8b shape (no
        // `sources` key) to exercise the additive-decode path.
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
