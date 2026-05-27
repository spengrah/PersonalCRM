// DoctorAnarlogTests cover the anarlog reader source checks Doctor
// runs. Same harness shape as DoctorIcloudContactsTests.
import XCTest
import CRMMacCore
@testable import CRMMacLifecycle
@testable import CRMMacPiClient

final class DoctorAnarlogTests: XCTestCase {

    func testNoAnarlogConfigEmitsTwoNotConfiguredRows() async {
        let r = await runDoctor()
        let humans = r.results.first { $0.name == "anarlog_humans" }
        let sessions = r.results.first { $0.name == "anarlog_sessions" }
        XCTAssertNotNil(humans)
        XCTAssertNotNil(sessions)
        XCTAssertEqual(humans?.status, .warn)
        XCTAssertEqual(sessions?.status, .warn)
        XCTAssertTrue(humans?.details.contains("not_configured") ?? false)
    }

    func testHumansEnabledShowsActive() async {
        let r = await runDoctor(anarlog: AnarlogConfig(
            rootPath: "/tmp/anarlog",
            humansEnabled: true,
            sessionsEnabled: false))
        let humans = r.results.first { $0.name == "anarlog_humans" }
        let sessions = r.results.first { $0.name == "anarlog_sessions" }
        XCTAssertEqual(humans?.status, .pass)
        XCTAssertTrue(humans?.details.contains("enabled") ?? false)
        XCTAssertEqual(sessions?.status, .warn)
        XCTAssertTrue(sessions?.details.contains("not_configured") ?? false)
    }

    func testPathMissingFails() async {
        let r = await runDoctor(anarlog: AnarlogConfig(
            rootPath: "/nonexistent-path-for-test",
            humansEnabled: true))
        let pathCheck = r.results.first { $0.name == "anarlog:path_missing" }
        XCTAssertNotNil(pathCheck)
        XCTAssertEqual(pathCheck?.status, .fail)
    }

    func testHumansSubdirMissingWarns() async {
        let path = "/tmp/anarlog-test-\(UUID().uuidString)"
        let r = await runDoctor(
            anarlog: AnarlogConfig(rootPath: path, humansEnabled: true),
            // Mark the root path as existing but DON'T mark humans/.
            extraDirs: [path])
        let subdir = r.results.first { $0.name == "anarlog:humans_subdir_missing" }
        XCTAssertNotNil(subdir)
        XCTAssertEqual(subdir?.status, .warn)
    }

    func testSessionsSubdirMissingWarns() async {
        let path = "/tmp/anarlog-test-\(UUID().uuidString)"
        let r = await runDoctor(
            anarlog: AnarlogConfig(rootPath: path, sessionsEnabled: true),
            extraDirs: [path])
        let subdir = r.results.first { $0.name == "anarlog:sessions_subdir_missing" }
        XCTAssertNotNil(subdir)
        XCTAssertEqual(subdir?.status, .warn)
    }

    func testHappyPathEmitsLastTickRow() async {
        let path = "/tmp/anarlog-test-\(UUID().uuidString)"
        let now = Date(timeIntervalSince1970: 2_000_000_000)
        let recent = now.addingTimeInterval(-60)
        let r = await runDoctor(
            anarlog: AnarlogConfig(rootPath: path, humansEnabled: true),
            humansState: SourceState(
                lastScheduledAt: recent,
                lastPushedAt: recent),
            extraDirs: [path, "\(path)/humans"],
            clock: FixedClock(now))
        let lastTick = r.results.first { $0.name == "anarlog_humans.last_tick" }
        XCTAssertNotNil(lastTick)
        XCTAssertEqual(lastTick?.status, .pass)
    }

    func testStaleLastTickWarns() async {
        let path = "/tmp/anarlog-test-\(UUID().uuidString)"
        let now = Date(timeIntervalSince1970: 2_000_000_000)
        // 30 minutes > 2x 5 min humans interval.
        let stale = now.addingTimeInterval(-30 * 60)
        let r = await runDoctor(
            anarlog: AnarlogConfig(rootPath: path, humansEnabled: true),
            humansState: SourceState(lastScheduledAt: stale),
            extraDirs: [path, "\(path)/humans"],
            clock: FixedClock(now))
        let lastTick = r.results.first { $0.name == "anarlog_humans.last_tick" }
        XCTAssertEqual(lastTick?.status, .warn)
    }

    func testNeitherEnabledSkipsFilesystemProbes() async {
        // path is bogus but neither source is enabled — Doctor should
        // NOT probe the filesystem and NOT emit path_missing.
        let r = await runDoctor(anarlog: AnarlogConfig(
            rootPath: "/totally/bogus/path",
            humansEnabled: false,
            sessionsEnabled: false))
        XCTAssertNil(r.results.first { $0.name == "anarlog:path_missing" })
    }

    // MARK: - test rig

    private func runDoctor(
        anarlog: AnarlogConfig? = nil,
        humansState: SourceState? = nil,
        sessionsState: SourceState? = nil,
        extraDirs: [String] = [],
        clock: ClockAdapter? = nil
    ) async -> DoctorReport {
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        let sources: DaemonSourcesConfig?
        if let anarlog {
            sources = DaemonSourcesConfig(anarlog: anarlog)
        } else {
            sources = nil
        }
        let config = DaemonConfig(
            piURL: URL(string: "https://pi.example.test")!,
            hostID: UUID(uuidString: "11111111-2222-3333-4444-555555555555")!,
            hostname: "mac-1",
            installedAt: Date(timeIntervalSince1970: 1_700_000_000),
            sources: sources)
        var state = DaemonState(schemaVersion: 1, hostID: config.hostID)
        if let s = humansState { state.sources["anarlog_humans"] = s }
        if let s = sessionsState { state.sources["anarlog_sessions"] = s }
        let encoder = JSONEncoder()
        encoder.dateEncodingStrategy = .iso8601
        try! fs.write(try! encoder.encode(config), to: paths.configFilePath)
        try! fs.write(try! encoder.encode(state), to: paths.stateFilePath)
        for dir in extraDirs {
            try! fs.createDirectory(at: dir)
        }
        var script = FakeAgentService.Script()
        script.statusSequence = [.enabled]
        let deps = DoctorDependencies(
            paths: paths,
            filesystem: fs,
            keychain: InMemoryKeychainStore(initial: "key"),
            agentService: FakeAgentService(script: script),
            piClientFactory: { url in
                PiClient(
                    baseURL: url,
                    transport: LifecycleMockTransport([.respond(status: 200, data: known200JSON)]).asTransport(),
                    sleep: noopSleep)
            },
            contactsAuth: StubContactsAuthorizationAdapter(status: .authorized),
            containerEnumerator: StubContactContainerEnumerator(),
            tickInterval: 60,
            clock: clock ?? FixedClock(),
            logger: NoopLogger())
        return await Doctor(deps).run()
    }
}
