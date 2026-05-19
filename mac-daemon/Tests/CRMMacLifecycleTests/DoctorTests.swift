import XCTest
import CRMMacCore
@testable import CRMMacLifecycle
@testable import CRMMacPiClient

final class DoctorTests: XCTestCase {

    /// Convenience constructor: minimal DoctorDependencies with all
    /// adapter dependencies stubbed so existing tests don't need to
    /// thread every adapter through their setup.
    private func makeDeps(
        paths: LifecyclePaths,
        fs: FilesystemAdapter,
        keychain: KeychainStore,
        agentService: AgentService,
        piClient: @escaping (URL) -> PiClient,
        contactsAuth: ContactsAuthorizationAdapter? = nil,
        containerEnumerator: ContactContainerEnumerator? = nil,
        clock: ClockAdapter? = nil
    ) -> DoctorDependencies {
        DoctorDependencies(
            paths: paths,
            filesystem: fs,
            keychain: keychain,
            agentService: agentService,
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
        var script = FakeAgentService.Script()
        script.statusSequence = [.enabled]
        let doctor = Doctor(makeDeps(
            paths: paths,
            fs: fs,
            keychain: keychain,
            agentService: FakeAgentService(script: script),
            piClient: { url in
                PiClient(
                    baseURL: url,
                    transport: LifecycleMockTransport([.respond(status: 200, data: known200JSON)]).asTransport(),
                    sleep: noopSleep)
            }))
        let report = await doctor.run()
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
            agentService: FakeAgentService(),
            piClient: { _ in PiClient(baseURL: URL(string: "https://x")!, transport: LifecycleMockTransport([]).asTransport(), sleep: noopSleep) }))
        let report = await doctor.run()
        let keychain = report.results.first(where: { $0.name == "api-key" })!
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
            agentService: FakeAgentService(),
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
        let stateJSON = Data("""
        {"schemaVersion": 99, "sources": {}}
        """.utf8)
        try fs.write(stateJSON, to: paths.stateFilePath)
        let doctor = Doctor(makeDeps(
            paths: paths,
            fs: fs,
            keychain: InMemoryKeychainStore(initial: "key"),
            agentService: FakeAgentService(),
            piClient: { _ in PiClient(baseURL: URL(string: "https://x")!, transport: LifecycleMockTransport([]).asTransport(), sleep: noopSleep) }))
        let report = await doctor.run()
        let configCheck = report.results.first(where: { $0.name == "config_state" })!
        XCTAssertEqual(configCheck.status, .fail)
    }

    func testAgentServiceNotRegisteredWarns() async {
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        var script = FakeAgentService.Script()
        script.statusSequence = [.notRegistered]
        let doctor = Doctor(makeDeps(
            paths: paths,
            fs: fs,
            keychain: InMemoryKeychainStore(),
            agentService: FakeAgentService(script: script),
            piClient: { _ in PiClient(baseURL: URL(string: "https://x")!, transport: LifecycleMockTransport([]).asTransport(), sleep: noopSleep) }))
        let report = await doctor.run()
        let check = report.results.first(where: { $0.name == "agent_service" })!
        XCTAssertEqual(check.status, .warn)
    }

    func testAgentServiceRequiresApprovalWarns() async {
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        var script = FakeAgentService.Script()
        script.statusSequence = [.requiresApproval]
        let doctor = Doctor(makeDeps(
            paths: paths,
            fs: fs,
            keychain: InMemoryKeychainStore(),
            agentService: FakeAgentService(script: script),
            piClient: { _ in PiClient(baseURL: URL(string: "https://x")!, transport: LifecycleMockTransport([]).asTransport(), sleep: noopSleep) }))
        let report = await doctor.run()
        let check = report.results.first(where: { $0.name == "agent_service" })!
        XCTAssertEqual(check.status, .warn)
        XCTAssertTrue(check.details.lowercased().contains("approv"),
            "requires-approval WARN must mention 'approv*' for operator guidance")
    }

    func testAgentServiceNotFoundFails() async {
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        var script = FakeAgentService.Script()
        script.statusSequence = [.notFound]
        let doctor = Doctor(makeDeps(
            paths: paths,
            fs: fs,
            keychain: InMemoryKeychainStore(),
            agentService: FakeAgentService(script: script),
            piClient: { _ in PiClient(baseURL: URL(string: "https://x")!, transport: LifecycleMockTransport([]).asTransport(), sleep: noopSleep) }))
        let report = await doctor.run()
        let check = report.results.first(where: { $0.name == "agent_service" })!
        XCTAssertEqual(check.status, .fail)
        XCTAssertTrue(check.details.contains(paths.bundleAppPath))
    }
}
