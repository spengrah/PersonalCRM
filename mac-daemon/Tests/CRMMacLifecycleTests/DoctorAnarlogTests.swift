// DoctorAnarlogTests cover the Anarlog source checks Doctor runs.
// Same harness shape as DoctorIcloudContactsTests.
import Foundation
import XCTest
import CRMMacCore
@testable import CRMMacLifecycle
@testable import CRMMacPiClient

private final class ResolverRecorder: @unchecked Sendable {
    private let lock = NSLock()
    private var values: [String?] = []

    func append(_ value: String?) {
        lock.lock()
        values.append(value)
        lock.unlock()
    }

    func snapshot() -> [String?] {
        lock.lock()
        defer { lock.unlock() }
        return values
    }
}

final class DoctorAnarlogTests: XCTestCase {

    func testNoAnarlogConfigEmitsTwoNotConfiguredRows() async {
        let recorder = ResolverRecorder()
        let report = await runDoctor(resolver: { path in
            recorder.append(path)
            return "/synthetic/bin/anarlog"
        })
        let humans = report.results.first { $0.name == "anarlog_humans" }
        let sessions = report.results.first { $0.name == "anarlog_sessions" }
        XCTAssertNotNil(humans)
        XCTAssertNotNil(sessions)
        XCTAssertEqual(humans?.status, .warn)
        XCTAssertEqual(sessions?.status, .warn)
        XCTAssertEqual(humans?.details, "not_configured (run `crm-mac configure anarlog --operator-person-id <uuid> --enable both`)")
        XCTAssertEqual(sessions?.details, "not_configured (run `crm-mac configure anarlog --operator-person-id <uuid> --enable both`)")
        XCTAssertTrue(recorder.snapshot().isEmpty)
    }

    func testHumansEnabledShowsActive() async throws {
        let report = await runDoctor(anarlog: try anarlogConfig(humans: true))
        let humans = report.results.first { $0.name == "anarlog_humans" }
        let sessions = report.results.first { $0.name == "anarlog_sessions" }
        XCTAssertEqual(humans?.status, .pass)
        XCTAssertEqual(humans?.details, "enabled")
        XCTAssertEqual(sessions?.status, .warn)
        XCTAssertEqual(sessions?.details, "not_configured (disabled)")
    }

    func testHappyPathEmitsLastTickRow() async throws {
        let now = Date(timeIntervalSince1970: 2_000_000_000)
        let recent = now.addingTimeInterval(-60)
        let report = await runDoctor(
            anarlog: try anarlogConfig(humans: true),
            humansState: SourceState(lastScheduledAt: recent, lastPushedAt: recent),
            clock: FixedClock(now))
        let lastTick = report.results.first { $0.name == "anarlog_humans.last_tick" }
        XCTAssertNotNil(lastTick)
        XCTAssertEqual(lastTick?.status, .pass)
    }

    func testStaleLastTickWarns() async throws {
        let now = Date(timeIntervalSince1970: 2_000_000_000)
        let stale = now.addingTimeInterval(-(60 * 60 + 1))
        let report = await runDoctor(
            anarlog: try anarlogConfig(humans: true),
            humansState: SourceState(lastScheduledAt: stale),
            clock: FixedClock(now))
        let lastTick = report.results.first { $0.name == "anarlog_humans.last_tick" }
        XCTAssertEqual(lastTick?.status, .warn)
    }

    func testLastTickWithinTwoHumansIntervalsPasses() async throws {
        let now = Date(timeIntervalSince1970: 2_000_000_000)
        let report = await runDoctor(
            anarlog: try anarlogConfig(humans: true),
            humansState: SourceState(lastScheduledAt: now.addingTimeInterval(-(60 * 60))),
            clock: FixedClock(now))
        let lastTick = report.results.first { $0.name == "anarlog_humans.last_tick" }
        XCTAssertEqual(lastTick?.status, .pass)
    }

    func testNeitherEnabledRunsNoAnarlogProbes() async throws {
        let recorder = ResolverRecorder()
        let report = await runDoctor(
            anarlog: try anarlogConfig(),
            resolver: { path in
                recorder.append(path)
                return "/synthetic/bin/anarlog"
            })
        XCTAssertNil(report.results.first { $0.name == "anarlog:cli" })
        XCTAssertNil(report.results.first { $0.name == "anarlog:operator_person_id" })
        XCTAssertTrue(recorder.snapshot().isEmpty)
    }

    func testCLIResolvedPasses() async throws {
        let report = await runDoctor(
            anarlog: try anarlogConfig(humans: true),
            resolver: { _ in "/synthetic/bin/anarlog" })
        let check = report.results.first { $0.name == "anarlog:cli" }
        XCTAssertEqual(check?.status, .pass)
        XCTAssertEqual(check?.details, "resolved: /synthetic/bin/anarlog")
    }

    func testCLINotFoundByDefaultSearchFails() async throws {
        let report = await runDoctor(
            anarlog: try anarlogConfig(humans: true),
            resolver: { _ in nil })
        let check = report.results.first { $0.name == "anarlog:cli" }
        XCTAssertEqual(check?.status, .fail)
        XCTAssertEqual(check?.details, "not found at the default location; set it with `crm-mac configure anarlog --cli-path <abs>`")
    }

    func testConfiguredCLIPathNotExecutableFails() async throws {
        let cliPath = "/opt/synthetic/anarlog"
        let report = await runDoctor(
            anarlog: try anarlogConfig(humans: true, cliPath: cliPath),
            resolver: { _ in nil })
        let check = report.results.first { $0.name == "anarlog:cli" }
        XCTAssertEqual(check?.status, .fail)
        XCTAssertEqual(check?.details, "cli_path is not an executable file: \(cliPath)")
    }

    func testResolverReceivesConfiguredCLIPath() async throws {
        let configured = ResolverRecorder()
        _ = await runDoctor(
            anarlog: try anarlogConfig(humans: true, cliPath: "/opt/synthetic/anarlog"),
            resolver: { path in
                configured.append(path)
                return "/synthetic/bin/anarlog"
            })
        XCTAssertEqual(configured.snapshot().map { $0 ?? "<nil>" }, ["/opt/synthetic/anarlog"])

        let defaultSearch = ResolverRecorder()
        _ = await runDoctor(
            anarlog: try anarlogConfig(humans: true),
            resolver: { path in
                defaultSearch.append(path)
                return "/synthetic/bin/anarlog"
            })
        let defaultValues = defaultSearch.snapshot()
        XCTAssertEqual(defaultValues.count, 1)
        XCTAssertNil(defaultValues[0])
    }

    func testChecksRunOncePerDoctorRun() async throws {
        let recorder = ResolverRecorder()
        let report = await runDoctor(
            anarlog: try anarlogConfig(humans: true, sessions: true),
            resolver: { path in
                recorder.append(path)
                return "/synthetic/bin/anarlog"
            })
        XCTAssertEqual(report.results.filter { $0.name == "anarlog:cli" }.count, 1)
        XCTAssertEqual(report.results.filter { $0.name == "anarlog:operator_person_id" }.count, 1)
        XCTAssertEqual(recorder.snapshot().count, 1)
    }

    func testOperatorPersonIDSetPasses() async throws {
        let report = await runDoctor(
            anarlog: try anarlogConfig(humans: true),
            resolver: { _ in "/synthetic/bin/anarlog" })
        let check = report.results.first { $0.name == "anarlog:operator_person_id" }
        XCTAssertEqual(check?.status, .pass)
        XCTAssertEqual(check?.details, "set")
    }

    func testOperatorPersonIDUnsetFails() async throws {
        let report = await runDoctor(
            anarlog: try anarlogConfig(humans: true, operatorSet: false),
            resolver: { _ in "/synthetic/bin/anarlog" })
        let check = report.results.first { $0.name == "anarlog:operator_person_id" }
        XCTAssertEqual(check?.status, .fail)
        XCTAssertEqual(check?.details, "not set; set it with `crm-mac configure anarlog --operator-person-id <uuid>`")
    }

    func testSessionsLastTickUsesThirtyMinuteInterval() async throws {
        let now = Date(timeIntervalSince1970: 2_000_000_000)
        let config = try anarlogConfig(sessions: true)
        let atThreshold = await runDoctor(
            anarlog: config,
            sessionsState: SourceState(lastScheduledAt: now.addingTimeInterval(-3600)),
            clock: FixedClock(now))
        XCTAssertEqual(atThreshold.results.first { $0.name == "anarlog_sessions.last_tick" }?.status, .pass)

        let beyondThreshold = await runDoctor(
            anarlog: config,
            sessionsState: SourceState(lastScheduledAt: now.addingTimeInterval(-3601)),
            clock: FixedClock(now))
        XCTAssertEqual(beyondThreshold.results.first { $0.name == "anarlog_sessions.last_tick" }?.status, .warn)
    }

    func testDefaultResolverResolvesConfiguredCLIPath() async throws {
        let directory = FileManager.default.temporaryDirectory
            .appendingPathComponent("doctor-anarlog-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }

        let executable = directory.appendingPathComponent("anarlog-executable")
        let nonExecutable = directory.appendingPathComponent("anarlog-non-executable")
        try Data("synthetic".utf8).write(to: executable)
        try Data("synthetic".utf8).write(to: nonExecutable)
        try FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: executable.path)
        try FileManager.default.setAttributes([.posixPermissions: 0o644], ofItemAtPath: nonExecutable.path)

        let executableReport = await runDoctor(
            anarlog: try anarlogConfig(humans: true, cliPath: executable.path),
            useDefaultResolver: true)
        let executableCheck = executableReport.results.first { $0.name == "anarlog:cli" }
        XCTAssertEqual(executableCheck?.status, .pass)
        XCTAssertEqual(executableCheck?.details, "resolved: \(executable.path)")

        let nonExecutableReport = await runDoctor(
            anarlog: try anarlogConfig(humans: true, cliPath: nonExecutable.path),
            useDefaultResolver: true)
        let nonExecutableCheck = nonExecutableReport.results.first { $0.name == "anarlog:cli" }
        XCTAssertEqual(nonExecutableCheck?.status, .fail)
        XCTAssertEqual(nonExecutableCheck?.details, "cli_path is not an executable file: \(nonExecutable.path)")
    }

    private func anarlogConfig(
        humans: Bool = false,
        sessions: Bool = false,
        operatorSet: Bool = true,
        cliPath: String? = nil
    ) throws -> AnarlogConfig {
        var config = AnarlogConfig(humansEnabled: humans, sessionsEnabled: sessions)
        if operatorSet {
            try config.setOperatorPersonID("aaaaaaaa-0000-4000-8000-000000000001")
        }
        if let cliPath {
            try config.setCLIPath(cliPath)
        }
        return config
    }

    private func runDoctor(
        anarlog: AnarlogConfig? = nil,
        humansState: SourceState? = nil,
        sessionsState: SourceState? = nil,
        resolver: @escaping (String?) -> String? = { _ in "/synthetic/bin/anarlog" },
        useDefaultResolver: Bool = false,
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
        if let sourceState = humansState { state.sources["anarlog_humans"] = sourceState }
        if let sourceState = sessionsState { state.sources["anarlog_sessions"] = sourceState }
        let encoder = JSONEncoder()
        encoder.dateEncodingStrategy = .iso8601
        try! fs.write(try! encoder.encode(config), to: paths.configFilePath)
        try! fs.write(try! encoder.encode(state), to: paths.stateFilePath)
        var script = FakeAgentService.Script()
        script.statusSequence = [.enabled]
        let common = (
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
            tickInterval: 60.0,
            clock: clock ?? FixedClock(),
            logger: NoopLogger())
        let dependencies: DoctorDependencies
        if useDefaultResolver {
            dependencies = DoctorDependencies(
                paths: common.paths,
                filesystem: common.filesystem,
                keychain: common.keychain,
                agentService: common.agentService,
                piClientFactory: common.piClientFactory,
                contactsAuth: common.contactsAuth,
                containerEnumerator: common.containerEnumerator,
                tickInterval: common.tickInterval,
                clock: common.clock,
                logger: common.logger)
        } else {
            dependencies = DoctorDependencies(
                paths: common.paths,
                filesystem: common.filesystem,
                keychain: common.keychain,
                agentService: common.agentService,
                piClientFactory: common.piClientFactory,
                contactsAuth: common.contactsAuth,
                containerEnumerator: common.containerEnumerator,
                anarlogCLIResolver: resolver,
                tickInterval: common.tickInterval,
                clock: common.clock,
                logger: common.logger)
        }
        return await Doctor(dependencies).run()
    }
}
