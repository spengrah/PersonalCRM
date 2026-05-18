// StopStartTests cover the StopOps + StartOps logic that powers
// `crm-mac stop` and `crm-mac start` (plan D26). The ArgumentParser
// shells in mac-daemon/Sources/crm-mac/Commands/{Stop,Start}Command.swift
// are thin — they only build the dependency struct, delegate to
// StopOps/StartOps, and print the result lines. The logic-under-test
// lives in CRMMacLifecycle.
import XCTest
@testable import CRMMacLifecycle

final class StopStartTests: XCTestCase {

    // MARK: - stop

    func testStopUnregistersAndSignals() async throws {
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        try fs.write(Data("12345".utf8), to: paths.pidfilePath)
        let agent = FakeAgentService()
        let signaller = FakeProcessSignaller()
        let result = await StopOps.run(
            StopOpsDependencies(
                paths: paths,
                filesystem: fs,
                agentService: agent,
                processSignaller: signaller,
                logger: NoopLogger()),
            timeoutSeconds: 1)
        XCTAssertEqual(agent.unregisterCalls, 1)
        XCTAssertEqual(signaller.sigtermCalls, [12345])
        XCTAssertTrue(result.stopped)
        XCTAssertEqual(result.pid, 12345)
        XCTAssertTrue(result.unregisterInvoked)
    }

    func testStopReturnsExitCode1OnTimeout() async {
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        try? fs.write(Data("12345".utf8), to: paths.pidfilePath)
        let signaller = FakeProcessSignaller()
        signaller.nextPidfileReleaseResult = false
        let result = await StopOps.run(
            StopOpsDependencies(
                paths: paths,
                filesystem: fs,
                agentService: FakeAgentService(),
                processSignaller: signaller,
                logger: NoopLogger()),
            timeoutSeconds: 1)
        XCTAssertFalse(result.stopped,
            "release returned false → stop result must reflect timeout")
    }

    func testStopMalformedPidfileRequiresFlockProbe() async throws {
        // Per Codex r6 #3: a present-but-unparseable pidfile must
        // NOT be treated as "daemon not running". The flock probe is
        // the authoritative running check; if it reports the lock
        // is still held (release returns false), stop reports
        // stopped=false even though we never sent SIGTERM.
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        try fs.write(Data("garbage-not-a-pid".utf8), to: paths.pidfilePath)
        let signaller = FakeProcessSignaller()
        signaller.nextPidfileReleaseResult = false  // daemon still alive
        let result = await StopOps.run(
            StopOpsDependencies(
                paths: paths,
                filesystem: fs,
                agentService: FakeAgentService(),
                processSignaller: signaller,
                logger: NoopLogger()),
            timeoutSeconds: 1)
        XCTAssertEqual(signaller.sigtermCalls.count, 0,
            "no SIGTERM when pid is unparseable")
        XCTAssertFalse(result.stopped,
            "malformed pidfile + flock probe says held → must report stopped=false")
    }

    func testStopHandlesAbsentPidfile() async {
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        let signaller = FakeProcessSignaller()
        let result = await StopOps.run(
            StopOpsDependencies(
                paths: paths,
                filesystem: fs,
                agentService: FakeAgentService(),
                processSignaller: signaller,
                logger: NoopLogger()),
            timeoutSeconds: 1)
        XCTAssertEqual(signaller.sigtermCalls.count, 0)
        XCTAssertTrue(result.stopped)
        XCTAssertEqual(result.pid, 0)
    }

    func testStopToleratesUnregisterThrow() async {
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        var script = FakeAgentService.Script()
        script.unregisterThrows = .unregistrationFailed("not registered")
        let agent = FakeAgentService(script: script)
        let result = await StopOps.run(
            StopOpsDependencies(
                paths: paths,
                filesystem: fs,
                agentService: agent,
                processSignaller: FakeProcessSignaller(),
                logger: NoopLogger()),
            timeoutSeconds: 1)
        // The throw is logged but stop continues; flag is true
        // because we DID invoke unregister.
        XCTAssertTrue(result.unregisterInvoked)
        XCTAssertTrue(result.stopped)
    }

    // MARK: - start

    func testStartRegistersAndVerifiesEnabled() async throws {
        var script = FakeAgentService.Script()
        script.statusSequence = [.enabled]
        let agent = FakeAgentService(script: script)
        let result = try await StartOps.run(
            StartOpsDependencies(agentService: agent, logger: NoopLogger()),
            statusPollTimeoutSeconds: 1,
            statusPollIntervalNs: 50_000_000)
        XCTAssertEqual(agent.registerCalls, 1)
        XCTAssertEqual(result.outcome, .registered)
        XCTAssertEqual(result.finalStatus, .enabled)
        XCTAssertTrue(result.started)
    }

    func testStartReportsAlreadyRegistered() async throws {
        var script = FakeAgentService.Script()
        script.nextRegisterOutcome = .alreadyRegistered
        script.statusSequence = [.enabled]
        let agent = FakeAgentService(script: script)
        let result = try await StartOps.run(
            StartOpsDependencies(agentService: agent, logger: NoopLogger()),
            statusPollTimeoutSeconds: 1,
            statusPollIntervalNs: 50_000_000)
        XCTAssertEqual(result.outcome, .alreadyRegistered)
        XCTAssertEqual(result.finalStatus, .enabled)
        XCTAssertTrue(result.started)
    }

    func testStartExitCode1OnRequiresApproval() async throws {
        // register succeeds, but currentStatus stays .requiresApproval
        // throughout the poll window.
        var script = FakeAgentService.Script()
        script.statusSequence = [.requiresApproval]
        let agent = FakeAgentService(script: script)
        let result = try await StartOps.run(
            StartOpsDependencies(agentService: agent, logger: NoopLogger()),
            statusPollTimeoutSeconds: 0.3,
            statusPollIntervalNs: 50_000_000)
        XCTAssertFalse(result.started)
        XCTAssertEqual(result.finalStatus, .requiresApproval)
    }

    func testStartExitCode1OnPostRegisterNotEnabled() async throws {
        var script = FakeAgentService.Script()
        script.statusSequence = [.notRegistered]
        let agent = FakeAgentService(script: script)
        let result = try await StartOps.run(
            StartOpsDependencies(agentService: agent, logger: NoopLogger()),
            statusPollTimeoutSeconds: 0.3,
            statusPollIntervalNs: 50_000_000)
        XCTAssertFalse(result.started)
        XCTAssertEqual(result.finalStatus, .notRegistered)
    }

    func testStartSurfacesRegisterFailure() async {
        var script = FakeAgentService.Script()
        script.registerThrows = .registrationFailed(
            message: "denied", requiresApproval: true)
        let agent = FakeAgentService(script: script)
        do {
            _ = try await StartOps.run(
                StartOpsDependencies(agentService: agent, logger: NoopLogger()),
                statusPollTimeoutSeconds: 1,
                statusPollIntervalNs: 50_000_000)
            XCTFail("expected throw")
        } catch let err as StartOpsError {
            if case .registerFailed = err {
                // ok
            } else {
                XCTFail("expected registerFailed, got \(err)")
            }
        } catch {
            XCTFail("expected StartOpsError, got \(error)")
        }
    }
}
