// InstallerUpgradeStopsRunningDaemonTests cover the stop-the-running-
// daemon step on the upgrade path. The Installer
// reads the pidfile, sends SIGTERM via ProcessSignaller, and polls
// for release. Failure during the poll surfaces as
// InstallError.daemonStillRunning(pid:).
import XCTest
import CRMMacCore
@testable import CRMMacLifecycle
@testable import CRMMacPiClient

final class InstallerUpgradeStopsRunningDaemonTests: XCTestCase {

    func testUpgradeStopsRunningDaemonBeforeReplacement() async throws {
        let (installer, fs, agentService, signaller, paths) = try makeUpgradeHarness(
            pidfileContents: "98765")

        _ = try await installer.run(InstallRequest(
            piURL: URL(string: "https://x")!,
            pairingToken: "ignored",
            hostname: "ignored",
            upgrade: true))

        // SIGTERM was sent to the recorded pid.
        XCTAssertEqual(signaller.sigtermCalls, [98765])
        // Wait was called on the pidfile path.
        XCTAssertEqual(signaller.waitForPidfileReleaseCalls, [paths.pidfilePath])
        // SMAppService.unregister was called as part of the
        // stop-the-running-daemon step.
        XCTAssertGreaterThanOrEqual(agentService.unregisterCalls, 1)
        // Bundle in place after upgrade.
        XCTAssertTrue(fs.fileExists(at: paths.bundleAppPath))
    }

    func testUpgradeFailsCleanlyWhenDaemonRefusesToStop() async throws {
        let (installer, fs, _, signaller, paths) = try makeUpgradeHarness(
            pidfileContents: "98765")
        signaller.nextPidfileReleaseResult = false

        do {
            _ = try await installer.run(InstallRequest(
                piURL: URL(string: "https://x")!,
                pairingToken: "ignored",
                hostname: "ignored",
                upgrade: true))
            XCTFail("expected daemonStillRunning")
        } catch InstallError.daemonStillRunning(let pid) {
            XCTAssertEqual(pid, 98765)
        } catch {
            XCTFail("got \(error)")
        }
        // Bundle UNCHANGED — no backup-then-swap started because the
        // stop step refused before the backup-rename.
        let unchanged = try fs.read(from: paths.bundleBinaryPath)
        XCTAssertEqual(unchanged, Data("old binary".utf8))
    }

    func testUpgradeMalformedPidfileStillRequiresFlockProbe() async throws {
        // A present-but-unparseable pidfile during upgrade is NOT
        // treated as "daemon not running". The flock probe is
        // authoritative; if it reports the lock is still held the
        // upgrade fails with daemonStillRunning(pid: 0) rather than
        // stomping on a still-running daemon.
        let (installer, _, _, signaller, _) = try makeUpgradeHarness(
            pidfileContents: "garbage-not-a-pid")
        signaller.nextPidfileReleaseResult = false
        do {
            _ = try await installer.run(InstallRequest(
                piURL: URL(string: "https://x")!,
                pairingToken: "ignored",
                hostname: "ignored",
                upgrade: true))
            XCTFail("expected daemonStillRunning")
        } catch InstallError.daemonStillRunning(let pid) {
            XCTAssertEqual(pid, 0, "unparseable pidfile → pid surfaces as 0")
        } catch {
            XCTFail("got \(error)")
        }
        // Critically: no SIGTERM was sent (we couldn't parse a pid).
        XCTAssertEqual(signaller.sigtermCalls.count, 0)
    }

    func testUpgradeSkipsSIGTERMWhenNoPidfile() async throws {
        // No pidfile -> no SIGTERM call. The unregister is still
        // invoked because that's a separate SMAppService call.
        let (installer, _, agentService, signaller, _) = try makeUpgradeHarness(
            pidfileContents: nil)
        _ = try await installer.run(InstallRequest(
            piURL: URL(string: "https://x")!,
            pairingToken: "ignored",
            hostname: "ignored",
            upgrade: true))
        XCTAssertEqual(signaller.sigtermCalls.count, 0)
        XCTAssertGreaterThanOrEqual(agentService.unregisterCalls, 1)
    }

    // MARK: - helper

    private func makeUpgradeHarness(
        pidfileContents: String?
    ) throws -> (Installer, InMemoryFilesystem, FakeAgentService, FakeProcessSignaller, LifecyclePaths) {
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        fs.seedFile(at: "/tmp/source/crm-mac")
        try fs.createDirectory(at: paths.bundleAppPath)
        try fs.write(Data("old binary".utf8), to: paths.bundleBinaryPath)
        if let pid = pidfileContents {
            try fs.write(Data(pid.utf8), to: paths.pidfilePath)
        }
        let config = DaemonConfig(
            piURL: URL(string: "https://pi.example.test")!,
            hostID: UUID(uuidString: "11111111-2222-3333-4444-555555555555")!,
            hostname: "mac-1",
            installedAt: Date())
        let encoder = JSONEncoder()
        encoder.dateEncodingStrategy = .iso8601
        try fs.write(try encoder.encode(config), to: paths.configFilePath)

        let agentService = FakeAgentService()
        let signaller = FakeProcessSignaller()
        let exec = FakeExecutableAdapter(currentExecutablePath: "/tmp/source/crm-mac")
        let installer = Installer(InstallerDependencies(
            paths: paths,
            filesystem: fs,
            executable: exec,
            keychain: InMemoryKeychainStore(initial: "k"),
            agentService: agentService,
            processSignaller: signaller,
            bundleAssembler: BundleAssembler(filesystem: fs, executable: exec),
            piClientFactory: { url in
                PiClient(baseURL: url, transport: LifecycleMockTransport([]).asTransport(), sleep: noopSleep)
            },
            clock: FixedClock(),
            logger: NoopLogger(),
            stopDaemonTimeoutSeconds: 1))
        return (installer, fs, agentService, signaller, paths)
    }
}
