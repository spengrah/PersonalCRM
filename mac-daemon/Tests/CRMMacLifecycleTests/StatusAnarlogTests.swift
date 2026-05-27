// StatusAnarlogTests cover the anarlog status block rendering. The
// critical surface is `cursorUUIDCount`: an empty cursor renders 0;
// a populated cursor decodes via JSONSerialization and counts keys;
// a malformed cursor returns nil so StatusCommand prints
// "(decode_error)" rather than silently 0.
import XCTest
import CRMMacCore
@testable import CRMMacLifecycle

final class StatusAnarlogTests: XCTestCase {

    func testAnarlogBlocksNilWhenNoConfigAndNoState() throws {
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        try seedConfigOnly(paths: paths, fs: fs, anarlog: nil)
        try seedStateOnly(paths: paths, fs: fs)
        let status = makeStatus(paths: paths, fs: fs)
        let report = status.run()
        XCTAssertNil(report.anarlogHumans)
        XCTAssertNil(report.anarlogSessions)
    }

    func testAnarlogConfiguredButNoStateYieldsEnabledBlocks() throws {
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        try seedConfigOnly(paths: paths, fs: fs, anarlog: AnarlogConfig(
            rootPath: "/tmp/anarlog",
            humansEnabled: true,
            sessionsEnabled: false))
        try seedStateOnly(paths: paths, fs: fs)
        let status = makeStatus(paths: paths, fs: fs)
        let report = status.run()
        // Humans is enabled — even without state, we show the block.
        let humans = try XCTUnwrap(report.anarlogHumans)
        XCTAssertTrue(humans.enabled)
        XCTAssertNil(humans.lastScheduledAt)
        XCTAssertNil(humans.cursorUUIDCount)
        // Sessions is disabled AND has no state → nil.
        XCTAssertNil(report.anarlogSessions)
    }

    func testEmptyCursorYieldsCursorCount0() throws {
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        try seedConfigOnly(paths: paths, fs: fs, anarlog: AnarlogConfig(
            rootPath: "/tmp/anarlog",
            humansEnabled: true,
            sessionsEnabled: true))
        let state = makeState(humansCursor: "", sessionsCursor: "")
        try writeState(state: state, paths: paths, fs: fs)
        let status = makeStatus(paths: paths, fs: fs)
        let report = status.run()
        XCTAssertEqual(report.anarlogHumans?.cursorUUIDCount, 0)
        XCTAssertEqual(report.anarlogSessions?.cursorUUIDCount, 0)
    }

    func testPopulatedCursorCountedCorrectly() throws {
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        try seedConfigOnly(paths: paths, fs: fs, anarlog: AnarlogConfig(
            rootPath: "/tmp/anarlog",
            humansEnabled: true))
        let cursor = """
        {"u1":{"content_hash":"a","payload_hash":"b"},
         "u2":{"content_hash":"c","payload_hash":"d"},
         "u3":{"content_hash":"e","payload_hash":"f"}}
        """
        let state = makeState(humansCursor: cursor)
        try writeState(state: state, paths: paths, fs: fs)
        let status = makeStatus(paths: paths, fs: fs)
        let report = status.run()
        XCTAssertEqual(report.anarlogHumans?.cursorUUIDCount, 3)
    }

    func testMalformedCursorReturnsNilCount() throws {
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        try seedConfigOnly(paths: paths, fs: fs, anarlog: AnarlogConfig(
            rootPath: "/tmp/anarlog",
            humansEnabled: true))
        let state = makeState(humansCursor: "not json")
        try writeState(state: state, paths: paths, fs: fs)
        let status = makeStatus(paths: paths, fs: fs)
        let report = status.run()
        XCTAssertNil(report.anarlogHumans?.cursorUUIDCount,
                     "malformed cursor must yield nil so renderer can show (decode_error)")
    }

    func testRecoveryFlagDetected() throws {
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        try seedConfigOnly(paths: paths, fs: fs, anarlog: AnarlogConfig(
            rootPath: "/tmp/anarlog",
            humansEnabled: true))
        let state = makeState(humansCursor: "{}",
                              humansLastError: "recovery_requested:hash_mismatch")
        try writeState(state: state, paths: paths, fs: fs)
        let status = makeStatus(paths: paths, fs: fs)
        let report = status.run()
        XCTAssertEqual(report.anarlogHumans?.recoveryRequested, true)
    }

    func testNonRecoveryErrorRendered() throws {
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        try seedConfigOnly(paths: paths, fs: fs, anarlog: AnarlogConfig(
            rootPath: "/tmp/anarlog",
            humansEnabled: true))
        let state = makeState(humansCursor: "{}",
                              humansLastError: "publish_held_due_to_rejections (123 rejected)")
        try writeState(state: state, paths: paths, fs: fs)
        let status = makeStatus(paths: paths, fs: fs)
        let report = status.run()
        XCTAssertEqual(report.anarlogHumans?.recoveryRequested, false)
        XCTAssertEqual(report.anarlogHumans?.lastError,
                       "publish_held_due_to_rejections (123 rejected)")
    }

    func testCursorCountStaticHelpers() {
        XCTAssertEqual(AnarlogSourceStatus.humansCursorUUIDCount(""), 0)
        XCTAssertEqual(AnarlogSourceStatus.humansCursorUUIDCount("{}"), 0)
        XCTAssertEqual(AnarlogSourceStatus.humansCursorUUIDCount(
            #"{"a":{"x":1},"b":{"x":1}}"#), 2)
        XCTAssertNil(AnarlogSourceStatus.humansCursorUUIDCount("not json"))
        XCTAssertNil(AnarlogSourceStatus.humansCursorUUIDCount("[1,2,3]"))
    }

    // MARK: - helpers

    private func makeStatus(paths: LifecyclePaths, fs: InMemoryFilesystem) -> Status {
        var script = FakeAgentService.Script()
        script.statusSequence = [.enabled]
        return Status(StatusDependencies(
            paths: paths,
            filesystem: fs,
            agentService: FakeAgentService(script: script)))
    }

    private func seedConfigOnly(
        paths: LifecyclePaths,
        fs: InMemoryFilesystem,
        anarlog: AnarlogConfig?
    ) throws {
        let cfg = DaemonConfig(
            piURL: URL(string: "https://pi.example.test")!,
            hostID: UUID(uuidString: "11111111-2222-3333-4444-555555555555")!,
            hostname: "mac-1",
            installedAt: Date(timeIntervalSince1970: 1_700_000_000),
            sources: anarlog.map { DaemonSourcesConfig(anarlog: $0) })
        let encoder = JSONEncoder()
        encoder.dateEncodingStrategy = .iso8601
        try fs.write(try encoder.encode(cfg), to: paths.configFilePath)
    }

    private func seedStateOnly(
        paths: LifecyclePaths,
        fs: InMemoryFilesystem
    ) throws {
        let state = DaemonState(schemaVersion: 1)
        let encoder = JSONEncoder()
        encoder.dateEncodingStrategy = .iso8601
        try fs.write(try encoder.encode(state), to: paths.stateFilePath)
    }

    private func makeState(
        humansCursor: String? = nil,
        humansLastError: String? = nil,
        sessionsCursor: String? = nil
    ) -> DaemonState {
        var state = DaemonState(schemaVersion: 1)
        if let cursor = humansCursor {
            state.sources["anarlog_humans"] = SourceState(
                cursor: cursor,
                lastScheduledAt: Date(timeIntervalSince1970: 2_000_000_000),
                lastError: humansLastError)
        }
        if let cursor = sessionsCursor {
            state.sources["anarlog_sessions"] = SourceState(
                cursor: cursor,
                lastScheduledAt: Date(timeIntervalSince1970: 2_000_000_000))
        }
        return state
    }

    private func writeState(
        state: DaemonState,
        paths: LifecyclePaths,
        fs: InMemoryFilesystem
    ) throws {
        let encoder = JSONEncoder()
        encoder.dateEncodingStrategy = .iso8601
        try fs.write(try encoder.encode(state), to: paths.stateFilePath)
    }
}
