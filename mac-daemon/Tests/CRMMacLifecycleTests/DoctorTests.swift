import XCTest
import CRMMacCore
@testable import CRMMacLifecycle
@testable import CRMMacPiClient

final class DoctorTests: XCTestCase {

    /// Convenience constructor: minimal DoctorDependencies with all
    /// PR8b dependencies stubbed so existing tests don't need to
    /// thread every adapter through their setup.
    private func makeDeps(
        paths: LifecyclePaths,
        fs: FilesystemAdapter,
        keychain: KeychainStore,
        launchctl: LaunchctlRunner,
        piClient: @escaping (URL) -> PiClient,
        contactsAuth: ContactsAuthorizationAdapter? = nil,
        containerEnumerator: ContactContainerEnumerator? = nil,
        clock: ClockAdapter? = nil
    ) -> DoctorDependencies {
        DoctorDependencies(
            paths: paths,
            filesystem: fs,
            keychain: keychain,
            launchctl: launchctl,
            piClientFactory: piClient,
            contactsAuth: contactsAuth ?? StubContactsAuthorizationAdapter(),
            containerEnumerator: containerEnumerator ?? StubContactContainerEnumerator(),
            tickInterval: 60,
            clock: clock ?? FixedClock(),
            logger: NoopLogger())
    }

    func testAllPass() async throws {
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        let config = DaemonConfig(
            piURL: URL(string: "https://pi.example.test")!,
            hostID: UUID(uuidString: "11111111-2222-3333-4444-555555555555")!,
            hostname: "mac-1",
            installedAt: Date(timeIntervalSince1970: 1_700_000_000))
        let state = DaemonState(schemaVersion: 1, hostID: config.hostID)
        let encoder = JSONEncoder()
        encoder.dateEncodingStrategy = .iso8601
        try fs.write(try encoder.encode(config), to: paths.configFilePath)
        try fs.write(try encoder.encode(state), to: paths.stateFilePath)
        let keychain = InMemoryKeychainStore(initial: "key")
        // The fake's default printService is exit 1 (unregistered);
        // this test wants the four core checks to PASS, so override
        // the launchctl probe to "registered".
        var script = FakeLaunchctlRunner.Script()
        script.printService = [0]
        let doctor = Doctor(makeDeps(
            paths: paths,
            fs: fs,
            keychain: keychain,
            launchctl: FakeLaunchctlRunner(script: script),
            piClient: { url in
                PiClient(
                    baseURL: url,
                    transport: LifecycleMockTransport([.respond(status: 200, data: known200JSON)]).asTransport(),
                    sleep: noopSleep)
            }))
        let report = await doctor.run()
        // 4 core checks (keychain, launchctl, config_state, pi_reachability)
        // + 3 icloud_contacts checks (permission, allowlist, last_tick).
        // All four core pass; icloud permission passes (default
        // .authorized stub) but allowlist + last_tick are non-PASS
        // because the test config has no icloud allowlist + no
        // SourceState for icloud_contacts.
        XCTAssertEqual(report.failCount, 0,
                       "no FAILs expected (icloud checks are WARN at worst)")
        let names = report.results.map(\.name)
        XCTAssertTrue(names.contains("icloud_contacts.permission"))
        XCTAssertTrue(names.contains("icloud_contacts.allowlist"))
        XCTAssertTrue(names.contains("icloud_contacts.last_tick"))
    }

    func testKeychainMissingFails() async {
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        let doctor = Doctor(makeDeps(
            paths: paths,
            fs: fs,
            keychain: InMemoryKeychainStore(),
            launchctl: FakeLaunchctlRunner(),
            piClient: { _ in PiClient(baseURL: URL(string: "https://x")!, transport: LifecycleMockTransport([]).asTransport(), sleep: noopSleep) }))
        let report = await doctor.run()
        let keychain = report.results.first(where: { $0.name == "keychain" })!
        XCTAssertEqual(keychain.status, .fail)
    }

    func testPi401Fails() async throws {
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        let config = DaemonConfig(
            piURL: URL(string: "https://pi.example.test")!,
            hostID: UUID(uuidString: "11111111-2222-3333-4444-555555555555")!,
            hostname: "mac-1",
            installedAt: Date())
        let state = DaemonState(schemaVersion: 1)
        let encoder = JSONEncoder()
        encoder.dateEncodingStrategy = .iso8601
        try fs.write(try encoder.encode(config), to: paths.configFilePath)
        try fs.write(try encoder.encode(state), to: paths.stateFilePath)
        let doctor = Doctor(makeDeps(
            paths: paths,
            fs: fs,
            keychain: InMemoryKeychainStore(initial: "key"),
            launchctl: FakeLaunchctlRunner(),
            piClient: { _ in
                PiClient(
                    baseURL: URL(string: "https://pi.example.test")!,
                    transport: LifecycleMockTransport([.respond(status: 401, data: heartbeat401JSON)]).asTransport(),
                    sleep: noopSleep)
            }))
        let report = await doctor.run()
        let pi = report.results.first(where: { $0.name == "pi_reachability" })!
        XCTAssertEqual(pi.status, .fail)
    }

    func testSchemaMismatchFails() async throws {
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        let config = DaemonConfig(
            piURL: URL(string: "https://pi.example.test")!,
            hostID: UUID(),
            hostname: "mac-1",
            installedAt: Date())
        let encoder = JSONEncoder()
        encoder.dateEncodingStrategy = .iso8601
        try fs.write(try encoder.encode(config), to: paths.configFilePath)
        // Schema version 99
        let stateJSON = Data("""
        {"schemaVersion": 99, "sources": {}}
        """.utf8)
        try fs.write(stateJSON, to: paths.stateFilePath)
        let doctor = Doctor(makeDeps(
            paths: paths,
            fs: fs,
            keychain: InMemoryKeychainStore(initial: "key"),
            launchctl: FakeLaunchctlRunner(),
            piClient: { _ in PiClient(baseURL: URL(string: "https://x")!, transport: LifecycleMockTransport([]).asTransport(), sleep: noopSleep) }))
        let report = await doctor.run()
        let configCheck = report.results.first(where: { $0.name == "config_state" })!
        XCTAssertEqual(configCheck.status, .fail)
    }

    func testLaunchctlNotRegisteredWarns() async {
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        var script = FakeLaunchctlRunner.Script()
        script.printService = [1]
        let doctor = Doctor(makeDeps(
            paths: paths,
            fs: fs,
            keychain: InMemoryKeychainStore(),
            launchctl: FakeLaunchctlRunner(script: script),
            piClient: { _ in PiClient(baseURL: URL(string: "https://x")!, transport: LifecycleMockTransport([]).asTransport(), sleep: noopSleep) }))
        let report = await doctor.run()
        let lc = report.results.first(where: { $0.name == "launchctl" })!
        XCTAssertEqual(lc.status, .warn)
    }
}
