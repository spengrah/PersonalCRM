// LegacyMigrationTests exercise the runLegacyMigrationIfNeeded()
// path on the Installer's upgrade + register-only flows.
//
// The migration runs ONLY when all three signals hold:
//   1. legacy binary at paths.legacyBinaryPath
//   2. NO bundle at paths.bundleAppPath
//   3. user-data present (config or state or api-key)
//
// Step order (legacy stop + bootout BEFORE bundle assembly because
// legacy + SMAppService share the launchd label):
//   - SIGTERM legacy daemon + pidfile-poll
//   - legacyLaunchctl.bootout
//   - printService probe after grace period (fail if still loaded)
//   - Assemble bundle at tmp + atomic-rename
//   - Substitute __INSTALL_PREFIX__ placeholder
//   - SMAppService.register
//   - Delete legacy plist + legacy binary (best-effort, post-register).
import XCTest
import CRMMacCore
@testable import CRMMacLifecycle
@testable import CRMMacPiClient

final class LegacyMigrationTests: XCTestCase {

    func testUpgradeFromBareBinaryStopsDaemonUnloadsLegacyAssemblesBundleRegisters() async throws {
        let (installer, fs, agentService, signaller, launchctl, paths, _) =
            try makeLegacyInstall()
        // Pidfile present so the migration's SIGTERM path is exercised.
        try fs.write(Data("12345".utf8), to: paths.pidfilePath)

        _ = try await installer.run(InstallRequest(
            piURL: URL(string: "https://x")!,
            pairingToken: "ignored",
            hostname: "ignored",
            upgrade: true))

        // 2a — SIGTERM called on the pidfile pid.
        XCTAssertEqual(signaller.sigtermCalls, [12345])
        // 2b/2c — legacyLaunchctl bootout + printService called.
        XCTAssertGreaterThanOrEqual(launchctl.bootoutCalls.count, 1)
        XCTAssertGreaterThanOrEqual(launchctl.printServiceCalls.count, 1)
        // 3-4 — bundle exists at final path.
        XCTAssertTrue(fs.fileExists(at: paths.bundleAppPath))
        // 6 — register called.
        XCTAssertGreaterThanOrEqual(agentService.registerCalls, 1)
        // 7 — legacy plist + binary gone.
        XCTAssertFalse(fs.fileExists(at: paths.legacyPlistPath))
        XCTAssertFalse(fs.fileExists(at: paths.legacyBinaryPath))
        // Config, state, api-key untouched.
        XCTAssertTrue(fs.fileExists(at: paths.configFilePath))
    }

    func testRegisterOnlyAlsoRunsLegacyMigration() async throws {
        let (installer, fs, agentService, _, _, paths, _) = try makeLegacyInstall()
        _ = try await installer.run(InstallRequest(
            piURL: URL(string: "https://x")!,
            pairingToken: "ignored",
            hostname: "ignored",
            registerOnly: true))
        XCTAssertTrue(fs.fileExists(at: paths.bundleAppPath))
        XCTAssertGreaterThanOrEqual(agentService.registerCalls, 1)
        XCTAssertFalse(fs.fileExists(at: paths.legacyBinaryPath))
    }

    func testMigrationDoesNotFireOnFreshInstall() async throws {
        // No legacy artifacts seeded; run fresh install — migration
        // path must not execute.
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        fs.seedFile(at: "/tmp/source/crm-mac")
        let agentService = FakeAgentService()
        let signaller = FakeProcessSignaller()
        let launchctl = FakeLaunchctlRunner()
        let exec = FakeExecutableAdapter(currentExecutablePath: "/tmp/source/crm-mac")
        let installer = Installer(InstallerDependencies(
            paths: paths,
            filesystem: fs,
            executable: exec,
            keychain: InMemoryKeychainStore(),
            agentService: agentService,
            processSignaller: signaller,
            bundleAssembler: BundleAssembler(filesystem: fs, executable: exec),
            piClientFactory: { url in
                PiClient(baseURL: url,
                         transport: LifecycleMockTransport([.respond(status: 200, data: pair200JSON)]).asTransport(),
                         sleep: noopSleep)
            },
            clock: FixedClock(),
            logger: NoopLogger(),
            legacyLaunchctl: launchctl))
        _ = try await installer.run(InstallRequest(
            piURL: URL(string: "https://x")!,
            pairingToken: "tk",
            hostname: "mac-1"))
        // Migration path NOT entered: no SIGTERM, no legacy bootout.
        XCTAssertEqual(signaller.sigtermCalls.count, 0)
        XCTAssertEqual(launchctl.bootoutCalls.count, 0)
    }

    func testMigrationLegacyBootoutFailureSurfacesTypedError() async throws {
        // Legacy launchctl bootout exit-code is not by itself decisive
        // (the bootout step tolerates non-zero); the printService probe in
        // step 2c is what fails the migration. Script:
        //   bootout = [99]    (non-zero, ignored)
        //   printService = [0] (service still loaded — fatal)
        let (installer, fs, _, _, launchctl, paths, _) = try makeLegacyInstall()
        var script = FakeLaunchctlRunner.Script()
        script.bootout = [99]
        script.printService = [0]  // still loaded after grace period
        launchctl.script = script
        do {
            _ = try await installer.run(InstallRequest(
                piURL: URL(string: "https://x")!,
                pairingToken: "ignored",
                hostname: "ignored",
                upgrade: true))
            XCTFail("expected legacyBootoutFailed")
        } catch InstallError.legacyBootoutFailed {
            // ok
        } catch {
            XCTFail("got \(error)")
        }
        // Bundle NOT assembled — legacy artifacts still on disk.
        XCTAssertFalse(fs.fileExists(at: paths.bundleAppPath))
        XCTAssertTrue(fs.fileExists(at: paths.legacyBinaryPath))
        XCTAssertTrue(fs.fileExists(at: paths.legacyPlistPath))
    }

    func testMigrationMalformedPidfileStillRequiresFlockProbe() async throws {
        // Malformed pidfile on the migration path must surface
        // daemonStillRunning when the flock probe says held — same
        // contract as the upgrade and uninstall paths. A malformed
        // pidfile must NOT short-circuit to "daemon not running".
        let (installer, fs, _, signaller, _, paths, _) = try makeLegacyInstall()
        try fs.write(Data("not-a-pid".utf8), to: paths.pidfilePath)
        signaller.nextPidfileReleaseResult = false
        do {
            _ = try await installer.run(InstallRequest(
                piURL: URL(string: "https://x")!,
                pairingToken: "ignored",
                hostname: "ignored",
                upgrade: true))
            XCTFail("expected daemonStillRunning")
        } catch InstallError.daemonStillRunning(let pid) {
            XCTAssertEqual(pid, 0, "unparseable pidfile → pid=0")
        } catch {
            XCTFail("got \(error)")
        }
        XCTAssertEqual(signaller.sigtermCalls.count, 0)
        // Legacy install intact (migration didn't proceed).
        XCTAssertTrue(fs.fileExists(at: paths.legacyBinaryPath))
        XCTAssertFalse(fs.fileExists(at: paths.bundleAppPath))
    }

    func testMigrationDaemonRefusesToStopFailsCleanly() async throws {
        // Pidfile present + ProcessSignaller's wait returns false ->
        // migration aborts with daemonStillRunning.
        let (installer, fs, _, signaller, _, paths, _) = try makeLegacyInstall()
        try fs.write(Data("12345".utf8), to: paths.pidfilePath)
        signaller.nextPidfileReleaseResult = false
        do {
            _ = try await installer.run(InstallRequest(
                piURL: URL(string: "https://x")!,
                pairingToken: "ignored",
                hostname: "ignored",
                upgrade: true))
            XCTFail("expected daemonStillRunning")
        } catch InstallError.daemonStillRunning(let pid) {
            XCTAssertEqual(pid, 12345)
        } catch {
            XCTFail("got \(error)")
        }
        // Legacy install fully intact.
        XCTAssertTrue(fs.fileExists(at: paths.legacyBinaryPath))
        XCTAssertTrue(fs.fileExists(at: paths.legacyPlistPath))
        XCTAssertFalse(fs.fileExists(at: paths.bundleAppPath))
    }

    func testUpgradeRetryAfterPartialMigrationCleansLegacyArtifacts() async throws {
        // A partial migration left the bundle in place + the legacy
        // plist + binary on disk. The operator reruns with
        // `--upgrade` (rather than `--register-only`). The upgrade
        // path's normal swap completes; the post-swap cleanup hook
        // must still sweep the legacy artifacts so the abandoned
        // ~/Library/LaunchAgents/<label>.plist doesn't get re-loaded
        // on next login.
        let (installer, fs, agentService, _, _, paths, _) = try makeLegacyInstall()
        // First attempt: register throws.
        agentService.script.registerThrows = .registrationFailed(
            message: "denied", requiresApproval: true)
        do {
            _ = try await installer.run(InstallRequest(
                piURL: URL(string: "https://x")!,
                pairingToken: "ignored",
                hostname: "ignored",
                upgrade: true))
            XCTFail("expected first attempt to fail")
        } catch InstallError.agentRegistrationFailed {
            // ok — partial migration state: bundle assembled, register
            // failed, legacy artifacts still on disk.
        } catch {
            XCTFail("first attempt: got \(error)")
        }
        XCTAssertTrue(fs.fileExists(at: paths.bundleAppPath))
        XCTAssertTrue(fs.fileExists(at: paths.legacyBinaryPath))
        XCTAssertTrue(fs.fileExists(at: paths.legacyPlistPath))

        // Second attempt: clear the register error and retry via
        // `--upgrade` (the operator's natural choice if they forgot
        // about the register-only flag). The normal upgrade path
        // runs (migration short-circuit doesn't fire because the
        // bundle is already in place); the cleanup hook at the end
        // of runUpgrade still sweeps the legacy files.
        agentService.script.registerThrows = nil
        _ = try await installer.run(InstallRequest(
            piURL: URL(string: "https://x")!,
            pairingToken: "ignored",
            hostname: "ignored",
            upgrade: true))
        XCTAssertFalse(fs.fileExists(at: paths.legacyBinaryPath),
            "upgrade retry must delete the legacy binary")
        XCTAssertFalse(fs.fileExists(at: paths.legacyPlistPath),
            "upgrade retry must delete the legacy plist — otherwise launchd can auto-load it on next login")
    }

    func testMigrationRegisterFailureRetryCleansLegacyArtifacts() async throws {
        // If a migration assembles the bundle but register fails, a
        // subsequent --register-only retry must complete the cleanup
        // (delete legacy plist + binary).
        // Otherwise the legacy launchd plist survives in
        // ~/Library/LaunchAgents/ and can be auto-loaded on the
        // next login, resurrecting the bare-binary service the
        // migration was meant to retire.
        let (installer, fs, agentService, _, _, paths, _) = try makeLegacyInstall()
        // First attempt: register throws.
        agentService.script.registerThrows = .registrationFailed(
            message: "denied", requiresApproval: true)
        do {
            _ = try await installer.run(InstallRequest(
                piURL: URL(string: "https://x")!,
                pairingToken: "ignored",
                hostname: "ignored",
                upgrade: true))
            XCTFail("expected agentRegistrationFailed on first attempt")
        } catch InstallError.agentRegistrationFailed {
            // ok — bundle assembled, register failed.
        } catch {
            XCTFail("first attempt: got \(error)")
        }
        // Bundle IS in place; legacy artifacts still on disk because
        // step 7 didn't run.
        XCTAssertTrue(fs.fileExists(at: paths.bundleAppPath))
        XCTAssertTrue(fs.fileExists(at: paths.legacyBinaryPath))
        XCTAssertTrue(fs.fileExists(at: paths.legacyPlistPath))

        // Second attempt: clear the script error and retry via
        // --register-only. The migration's step 7 cleanup must run
        // even though the build-the-new half is skipped.
        agentService.script.registerThrows = nil
        _ = try await installer.run(InstallRequest(
            piURL: URL(string: "https://x")!,
            pairingToken: "ignored",
            hostname: "ignored",
            registerOnly: true))
        XCTAssertFalse(fs.fileExists(at: paths.legacyBinaryPath),
            "register-only retry must delete the legacy binary")
        XCTAssertFalse(fs.fileExists(at: paths.legacyPlistPath),
            "register-only retry must delete the legacy plist — otherwise launchd can auto-load it on next login")
    }

    func testMigrationBundleAssemblyFailureLeavesLegacyUnloadedForRetry() async throws {
        let (installer, fs, _, _, _, paths, exec) = try makeLegacyInstall()
        // Inject codesign failure during the migration's bundle
        // assembly step.
        exec.failBundleCodesignWith = "injected codesign failure"
        do {
            _ = try await installer.run(InstallRequest(
                piURL: URL(string: "https://x")!,
                pairingToken: "ignored",
                hostname: "ignored",
                upgrade: true))
            XCTFail("expected codesignFailed")
        } catch InstallError.codesignFailed {
            // ok
        } catch {
            XCTFail("got \(error)")
        }
        // Legacy artifacts still on disk (step 7 never ran). No
        // partial bundle.
        XCTAssertFalse(fs.fileExists(at: paths.bundleAppPath))
        XCTAssertTrue(fs.fileExists(at: paths.legacyBinaryPath))
        XCTAssertTrue(fs.fileExists(at: paths.legacyPlistPath))
    }

    // MARK: - helpers

    private typealias LegacyHarness = (
        installer: Installer,
        fs: InMemoryFilesystem,
        agentService: FakeAgentService,
        signaller: FakeProcessSignaller,
        launchctl: FakeLaunchctlRunner,
        paths: LifecyclePaths,
        exec: FakeExecutableAdapter
    )

    private func makeLegacyInstall() throws -> LegacyHarness {
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        fs.seedFile(at: "/tmp/source/crm-mac")
        // Legacy artifacts.
        try fs.write(Data("legacy bin".utf8), to: paths.legacyBinaryPath)
        try fs.write(Data("legacy plist".utf8), to: paths.legacyPlistPath)
        // User data (signal 3).
        let cfg = DaemonConfig(
            piURL: URL(string: "https://pi.example.test")!,
            hostID: UUID(uuidString: "11111111-2222-3333-4444-555555555555")!,
            hostname: "mac-1",
            installedAt: Date())
        let encoder = JSONEncoder()
        encoder.dateEncodingStrategy = .iso8601
        try fs.write(try encoder.encode(cfg), to: paths.configFilePath)

        let agentService = FakeAgentService()
        let signaller = FakeProcessSignaller()
        let launchctl = FakeLaunchctlRunner()
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
            legacyLaunchctl: launchctl,
            // Shrink the bootout-grace + stop-daemon timeouts so the
            // tests run fast.
            stopDaemonTimeoutSeconds: 1,
            legacyBootoutGraceSeconds: 0.05)
        )
        return (installer, fs, agentService, signaller, launchctl, paths, exec)
    }
}
