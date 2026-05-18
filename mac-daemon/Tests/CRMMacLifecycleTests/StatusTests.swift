import XCTest
import CRMMacCore
@testable import CRMMacLifecycle

final class StatusTests: XCTestCase {
    func testReportsInstalledAndRegistered() throws {
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        try fs.createDirectory(at: paths.bundleAppPath)
        try fs.write(Data("bin".utf8), to: paths.bundleBinaryPath)
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

        var script = FakeAgentService.Script()
        script.statusSequence = [.enabled]
        let status = Status(StatusDependencies(
            paths: paths,
            filesystem: fs,
            agentService: FakeAgentService(script: script)))
        let report = status.run()
        XCTAssertTrue(report.installed)
        XCTAssertTrue(report.registered)
        XCTAssertEqual(report.registrationStatus, .enabled)
        XCTAssertEqual(report.configHostname, "mac-1")
        XCTAssertEqual(report.hostID, config.hostID)
        XCTAssertEqual(report.stateSchemaVersion, 1)
        XCTAssertNotNil(report.lastHeartbeatAt)
    }

    func testReportsNotInstalledOnFreshSystem() {
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        var script = FakeAgentService.Script()
        script.statusSequence = [.notRegistered]
        let status = Status(StatusDependencies(
            paths: paths,
            filesystem: fs,
            agentService: FakeAgentService(script: script)))
        let report = status.run()
        XCTAssertFalse(report.installed)
        XCTAssertFalse(report.registered)
        XCTAssertEqual(report.registrationStatus, .notRegistered)
        XCTAssertNil(report.configHostname)
    }

    func testReportsRequiresApprovalDistinctFromNotRegistered() throws {
        // Bundle present but agent registration requires approval.
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        try fs.createDirectory(at: paths.bundleAppPath)
        try fs.write(Data("bin".utf8), to: paths.bundleBinaryPath)
        var script = FakeAgentService.Script()
        script.statusSequence = [.requiresApproval]
        let status = Status(StatusDependencies(
            paths: paths,
            filesystem: fs,
            agentService: FakeAgentService(script: script)))
        let report = status.run()
        XCTAssertTrue(report.installed)
        XCTAssertFalse(report.registered)
        XCTAssertEqual(report.registrationStatus, .requiresApproval)
    }

    // MARK: - icloud_contacts row

    func testIcloudContactsRowOmittedWhenNoConfigOrState() throws {
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        try seedConfigOnly(paths: paths, fs: fs)
        let report = Status(StatusDependencies(
            paths: paths,
            filesystem: fs,
            agentService: FakeAgentService())).run()
        XCTAssertNil(report.icloudContacts,
                     "no containers configured + no source state → omit row")
    }

    func testIcloudContactsRowShowsContainerCountFromConfig() throws {
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        try seedConfig(paths: paths, fs: fs, containers: ["c1", "c2"])
        try seedState(paths: paths, fs: fs, sourceState: nil)
        let report = Status(StatusDependencies(
            paths: paths,
            filesystem: fs,
            agentService: FakeAgentService())).run()
        let icloud = report.icloudContacts!
        XCTAssertEqual(icloud.containerCount, 2)
        XCTAssertNil(icloud.lastScheduledAt)
        XCTAssertFalse(icloud.recoveryRequested)
    }

    func testIcloudContactsRowSurfacesRecoveryFlag() throws {
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        try seedConfig(paths: paths, fs: fs, containers: ["c1"])
        let src = SourceState(
            lastScheduledAt: Date(timeIntervalSince1970: 1_700_000_000),
            lastError: "recovery_requested:allowlist_changed")
        try seedState(paths: paths, fs: fs, sourceState: src)
        let report = Status(StatusDependencies(
            paths: paths,
            filesystem: fs,
            agentService: FakeAgentService())).run()
        let icloud = report.icloudContacts!
        XCTAssertTrue(icloud.recoveryRequested)
        XCTAssertEqual(icloud.lastError, "recovery_requested:allowlist_changed")
    }

    func testIcloudContactsRowLastError() throws {
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        try seedConfig(paths: paths, fs: fs, containers: ["c1"])
        let src = SourceState(
            lastScheduledAt: Date(timeIntervalSince1970: 1_700_000_000),
            lastError: "contacts_permission:denied")
        try seedState(paths: paths, fs: fs, sourceState: src)
        let report = Status(StatusDependencies(
            paths: paths,
            filesystem: fs,
            agentService: FakeAgentService())).run()
        let icloud = report.icloudContacts!
        XCTAssertFalse(icloud.recoveryRequested)
        XCTAssertEqual(icloud.lastError, "contacts_permission:denied")
    }

    // MARK: - helpers

    private func seedConfigOnly(paths: LifecyclePaths, fs: InMemoryFilesystem) throws {
        try fs.createDirectory(at: paths.bundleAppPath)
        try fs.write(Data("bin".utf8), to: paths.bundleBinaryPath)
    }

    private func seedConfig(
        paths: LifecyclePaths,
        fs: InMemoryFilesystem,
        containers: [String]
    ) throws {
        let cfg = DaemonConfig(
            piURL: URL(string: "https://pi.example.test")!,
            hostID: UUID(uuidString: "11111111-2222-3333-4444-555555555555")!,
            hostname: "mac-1",
            installedAt: Date(),
            sources: DaemonSourcesConfig(
                icloudContacts: ICloudContactsConfig(containers: containers)))
        let encoder = JSONEncoder()
        encoder.dateEncodingStrategy = .iso8601
        try fs.write(try encoder.encode(cfg), to: paths.configFilePath)
    }

    private func seedState(
        paths: LifecyclePaths,
        fs: InMemoryFilesystem,
        sourceState: SourceState?
    ) throws {
        var state = DaemonState(schemaVersion: 1)
        if let s = sourceState {
            state.sources["icloud_contacts"] = s
        }
        let encoder = JSONEncoder()
        encoder.dateEncodingStrategy = .iso8601
        try fs.write(try encoder.encode(state), to: paths.stateFilePath)
    }
}
