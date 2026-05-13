import XCTest
@testable import CRMMacCore

final class ConfigTests: XCTestCase {
    private var tempDir: URL!
    private var fileURL: URL!

    override func setUpWithError() throws {
        try super.setUpWithError()
        tempDir = FileManager.default.temporaryDirectory
            .appendingPathComponent("crm-mac-config-tests-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: tempDir, withIntermediateDirectories: true)
        fileURL = tempDir.appendingPathComponent("config.json")
    }

    override func tearDownWithError() throws {
        try? FileManager.default.removeItem(at: tempDir)
        try super.tearDownWithError()
    }

    private func makeConfig() -> DaemonConfig {
        DaemonConfig(
            piURL: URL(string: "https://pi.example.ts.net")!,
            hostID: UUID(uuidString: "11111111-2222-3333-4444-555555555555")!,
            hostname: "mac-1",
            installedAt: Date(timeIntervalSince1970: 1_700_000_000))
    }

    func testRoundtrip() throws {
        let store = ConfigStore(fileURL: fileURL)
        let cfg = makeConfig()
        try store.save(cfg)
        XCTAssertEqual(try store.load(), cfg)
    }

    func testLoadMissingThrowsFileNotFound() {
        let store = ConfigStore(fileURL: fileURL)
        XCTAssertThrowsError(try store.load()) { error in
            guard case ConfigStoreError.fileNotFound = error else {
                XCTFail("expected fileNotFound, got \(error)")
                return
            }
        }
    }

    func testValidatePiURLRejectsFileScheme() {
        XCTAssertThrowsError(try ConfigStore.validatePiURL(URL(string: "file:///tmp")!))
    }

    func testValidatePiURLRejectsRelative() {
        XCTAssertThrowsError(try ConfigStore.validatePiURL(URL(string: "/relative")!))
    }

    func testValidatePiURLAcceptsHTTPS() throws {
        try ConfigStore.validatePiURL(URL(string: "https://pi.example.ts.net")!)
    }

    func testValidatePiURLAcceptsHTTPWithPort() throws {
        try ConfigStore.validatePiURL(URL(string: "http://localhost:8080")!)
    }

    func testSaveValidatesPiURL() {
        let store = ConfigStore(fileURL: fileURL)
        let bad = DaemonConfig(
            piURL: URL(string: "file:///tmp")!,
            hostID: UUID(),
            hostname: "mac-1",
            installedAt: Date())
        XCTAssertThrowsError(try store.save(bad)) { error in
            guard case ConfigStoreError.invalidPiURL = error else {
                XCTFail("expected invalidPiURL, got \(error)")
                return
            }
        }
    }

    func testJSONHasSnakeCaseKeys() throws {
        let store = ConfigStore(fileURL: fileURL)
        try store.save(makeConfig())
        let raw = String(data: try Data(contentsOf: fileURL), encoding: .utf8) ?? ""
        XCTAssertTrue(raw.contains("\"pi_url\""))
        XCTAssertTrue(raw.contains("\"host_id\""))
        XCTAssertTrue(raw.contains("\"installed_at\""))
    }

    func testExists() throws {
        let store = ConfigStore(fileURL: fileURL)
        XCTAssertFalse(store.exists())
        try store.save(makeConfig())
        XCTAssertTrue(store.exists())
    }
}
