import XCTest
import CRMMacCore
@testable import CRMMacLifecycle

final class StatusTests: XCTestCase {
    func testReportsInstalledAndRegistered() throws {
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        try fs.write(Data("binary".utf8), to: paths.binaryPath)
        let config = DaemonConfig(
            piURL: URL(string: "https://pi.example.test")!,
            hostID: UUID(uuidString: "11111111-2222-3333-4444-555555555555")!,
            hostname: "mac-1",
            installedAt: Date(timeIntervalSince1970: 1_700_000_000))
        let encoder = JSONEncoder()
        encoder.dateEncodingStrategy = .iso8601
        try fs.write(try encoder.encode(config), to: paths.configFilePath)
        let state = DaemonState(
            schemaVersion: 1,
            hostID: config.hostID,
            lastHeartbeatAt: Date(timeIntervalSince1970: 1_700_001_000))
        try fs.write(try encoder.encode(state), to: paths.stateFilePath)

        let status = Status(StatusDependencies(
            paths: paths,
            filesystem: fs,
            launchctl: FakeLaunchctlRunner()))
        let report = status.run()
        XCTAssertTrue(report.installed)
        XCTAssertTrue(report.registered)
        XCTAssertEqual(report.configHostname, "mac-1")
        XCTAssertEqual(report.hostID, config.hostID)
        XCTAssertEqual(report.stateSchemaVersion, 1)
        XCTAssertNotNil(report.lastHeartbeatAt)
    }

    func testReportsNotInstalledOnFreshSystem() {
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        var script = FakeLaunchctlRunner.Script()
        script.printService = [1]
        let status = Status(StatusDependencies(
            paths: paths,
            filesystem: fs,
            launchctl: FakeLaunchctlRunner(script: script)))
        let report = status.run()
        XCTAssertFalse(report.installed)
        XCTAssertFalse(report.registered)
        XCTAssertNil(report.configHostname)
    }
}
