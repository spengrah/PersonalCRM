import XCTest
import CRMMacCore
@testable import CRMMacLifecycle
@testable import CRMMacPiClient

final class InstallerUpgradeTests: XCTestCase {
    func testUpgradeDoesNotPostHost() async throws {
        let (installer, fs, agentService, _, paths, transport, _) = try prepareExistingInstall()
        let summary = try await installer.run(InstallRequest(
            piURL: URL(string: "https://pi.example.test")!,
            pairingToken: "ignored",
            hostname: "ignored",
            upgrade: true))
        XCTAssertEqual(transport.invocations.count, 0, "upgrade must NOT call POST /host")
        XCTAssertTrue(fs.fileExists(at: paths.bundleAppPath))
        XCTAssertTrue(fs.fileExists(at: paths.bundleBinaryPath))
        XCTAssertGreaterThanOrEqual(agentService.unregisterCalls, 1)
        XCTAssertGreaterThanOrEqual(agentService.registerCalls, 1)
        XCTAssertEqual(summary.bundleBinaryPath, paths.bundleBinaryPath)
    }

    func testUpgradeRefusesWithNoExistingInstall() async {
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        fs.seedFile(at: "/tmp/source/crm-mac")
        let exec = FakeExecutableAdapter(currentExecutablePath: "/tmp/source/crm-mac")
        let installer = Installer(InstallerDependencies(
            paths: paths,
            filesystem: fs,
            executable: exec,
            keychain: InMemoryKeychainStore(),
            agentService: FakeAgentService(),
            processSignaller: FakeProcessSignaller(),
            bundleAssembler: BundleAssembler(filesystem: fs, executable: exec),
            piClientFactory: { url in
                PiClient(
                    baseURL: url,
                    transport: LifecycleMockTransport([]).asTransport(),
                    sleep: noopSleep)
            },
            clock: FixedClock(),
            logger: NoopLogger()))
        do {
            _ = try await installer.run(InstallRequest(
                piURL: URL(string: "https://pi.example.test")!,
                pairingToken: "x",
                hostname: "x",
                upgrade: true))
            XCTFail("expected noExistingInstall")
        } catch InstallError.noExistingInstall {
            // ok
        } catch {
            XCTFail("got \(error)")
        }
    }

    func testUpgradeMigratesLegacyKeychainToPrimary() async throws {
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        fs.seedFile(at: "/tmp/source/crm-mac")
        // Pre-existing bundle from a prior install.
        try fs.createDirectory(at: paths.bundleAppPath)
        try fs.write(Data("old binary".utf8), to: paths.bundleBinaryPath)
        let config = DaemonConfig(
            piURL: URL(string: "https://pi.example.test")!,
            hostID: UUID(uuidString: "11111111-2222-3333-4444-555555555555")!,
            hostname: "mac-1",
            installedAt: Date(timeIntervalSince1970: 1_700_000_000))
        let encoder = JSONEncoder()
        encoder.dateEncodingStrategy = .iso8601
        try fs.write(try encoder.encode(config), to: paths.configFilePath)
        // Primary (file-store stand-in) starts empty; legacy holds the key.
        let primary = InMemoryKeychainStore()
        let legacy = InMemoryKeychainStore(initial: "migrating-key")
        let exec = FakeExecutableAdapter(currentExecutablePath: "/tmp/source/crm-mac")
        let installer = Installer(InstallerDependencies(
            paths: paths,
            filesystem: fs,
            executable: exec,
            keychain: primary,
            agentService: FakeAgentService(),
            processSignaller: FakeProcessSignaller(),
            bundleAssembler: BundleAssembler(filesystem: fs, executable: exec),
            piClientFactory: { url in
                PiClient(
                    baseURL: url,
                    transport: LifecycleMockTransport([]).asTransport(),
                    sleep: noopSleep)
            },
            clock: FixedClock(),
            logger: NoopLogger(),
            legacyKeychain: legacy))
        _ = try await installer.run(InstallRequest(
            piURL: URL(string: "https://pi.example.test")!,
            pairingToken: "ignored",
            hostname: "ignored",
            upgrade: true))
        XCTAssertEqual(try primary.readAPIKey(), "migrating-key",
            "migration must copy legacy key into primary store")
        XCTAssertThrowsError(try legacy.readAPIKey(),
            "migration must delete legacy entry post-copy") { error in
            guard let e = error as? KeychainStoreError, e == .notFound else {
                XCTFail("expected .notFound, got \(error)"); return
            }
        }
    }

    func testUpgradeAtomicallyReplacesExistingBundle() async throws {
        let (installer, fs, _, _, paths, _, _) = try prepareExistingInstall(
            bundleFiles: [
                ("Contents/Info.plist", Data("old-info".utf8)),
                ("Contents/MacOS/crm-mac", Data("old-binary".utf8)),
                ("Contents/Library/LaunchAgents/\(Daemon.label).plist", Data("old-plist".utf8)),
            ])
        _ = try await installer.run(InstallRequest(
            piURL: URL(string: "https://pi.example.test")!,
            pairingToken: "ignored",
            hostname: "ignored",
            upgrade: true))
        XCTAssertTrue(fs.fileExists(at: paths.bundleAppPath))
        // No leftover .tmp. or .backup. dirs.
        XCTAssertFalse(fs.allDirs.contains(where: { $0.contains(".tmp.") }))
        XCTAssertFalse(fs.allDirs.contains(where: { $0.contains(".backup.") }))
        XCTAssertFalse(fs.allPaths.contains(where: { $0.contains(".tmp.") }))
        XCTAssertFalse(fs.allPaths.contains(where: { $0.contains(".backup.") }))
        // New binary content replaces old.
        let newBinary = try fs.read(from: paths.bundleBinaryPath)
        XCTAssertNotEqual(newBinary, Data("old-binary".utf8),
            "upgrade must replace the old bundle content with the new")
    }

    func testUpgradeRegistrationFailureRollsBackToOldBundle() async throws {
        // A register failure during upgrade must restore the
        // previous install (rollback the backup-rename-swap) so the
        // operator isn't left with a stopped daemon + new bundle
        // they can't run.
        let oldInfoBytes = Data("old-info".utf8)
        let (installer, fs, agent, _, paths, _, _) = try prepareExistingInstall(
            bundleFiles: [
                ("Contents/Info.plist", oldInfoBytes),
                ("Contents/MacOS/crm-mac", Data("old-binary".utf8)),
            ])
        // Make register throw — the swap should have completed first
        // (so the new bundle is briefly at the final path) then roll
        // back.
        agent.script.registerThrows = .registrationFailed(
            message: "requires approval", requiresApproval: true)
        do {
            _ = try await installer.run(InstallRequest(
                piURL: URL(string: "https://x")!,
                pairingToken: "ignored",
                hostname: "ignored",
                upgrade: true))
            XCTFail("expected agentRegistrationFailed")
        } catch InstallError.agentRegistrationFailed {
            // ok
        } catch {
            XCTFail("got \(error)")
        }
        // Old bundle restored at the final path.
        let restored = try fs.read(from: "\(paths.bundleAppPath)/Contents/Info.plist")
        XCTAssertEqual(restored, oldInfoBytes,
            "register-failure rollback must restore the original bundle content")
        // No tmp or backup left behind.
        XCTAssertFalse(fs.allDirs.contains(where: { $0.contains(".tmp.") }))
        XCTAssertFalse(fs.allDirs.contains(where: { $0.contains(".backup.") }))
        // The rollback must attempt to re-register the restored bundle
        // (best-effort) — otherwise launchd has no record of the agent
        // after the upgrade-time unregister, and the daemon stays
        // stopped despite the file-level rollback succeeding. Total
        // register calls: 1 (initial, which threw) + 1 (rollback
        // re-register, which also throws because the fake still has
        // registerThrows set) = 2.
        XCTAssertGreaterThanOrEqual(agent.registerCalls, 2,
            "rollback must attempt to re-register the restored backup")
    }

    func testUpgradeAssemblyFailureRollsBackToOldBundle() async throws {
        let oldInfoBytes = Data("old-info".utf8)
        let (installer, fs, _, _, paths, _, exec) = try prepareExistingInstall(
            bundleFiles: [
                ("Contents/Info.plist", oldInfoBytes),
                ("Contents/MacOS/crm-mac", Data("old-binary".utf8)),
            ])
        exec.failBundleCodesignWith = "injected codesign failure"
        do {
            _ = try await installer.run(InstallRequest(
                piURL: URL(string: "https://pi.example.test")!,
                pairingToken: "ignored",
                hostname: "ignored",
                upgrade: true))
            XCTFail("expected upgrade to fail")
        } catch InstallError.codesignFailed {
            // ok
        } catch {
            XCTFail("got \(error)")
        }
        // Old bundle restored.
        let restored = try fs.read(from: "\(paths.bundleAppPath)/Contents/Info.plist")
        XCTAssertEqual(restored, oldInfoBytes,
            "rollback must restore the original bundle content")
        XCTAssertFalse(fs.allDirs.contains(where: { $0.contains(".tmp.") }))
        XCTAssertFalse(fs.allDirs.contains(where: { $0.contains(".backup.") }))
    }

    private func prepareExistingInstall(
        bundleFiles: [(String, Data)] = []
    ) throws -> (Installer, InMemoryFilesystem, FakeAgentService, InMemoryKeychainStore, LifecyclePaths, LifecycleMockTransport, FakeExecutableAdapter) {
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        fs.seedFile(at: "/tmp/source/crm-mac")
        try fs.createDirectory(at: paths.bundleAppPath)
        try fs.write(Data("old binary".utf8), to: paths.bundleBinaryPath)
        for (rel, data) in bundleFiles {
            let path = "\(paths.bundleAppPath)/\(rel)"
            try fs.write(data, to: path)
        }
        let config = DaemonConfig(
            piURL: URL(string: "https://pi.example.test")!,
            hostID: UUID(uuidString: "11111111-2222-3333-4444-555555555555")!,
            hostname: "mac-1",
            installedAt: Date(timeIntervalSince1970: 1_700_000_000))
        let encoder = JSONEncoder()
        encoder.dateEncodingStrategy = .iso8601
        try fs.write(try encoder.encode(config), to: paths.configFilePath)

        let keychain = InMemoryKeychainStore(initial: "existing-key")
        let agentService = FakeAgentService()
        let transport = LifecycleMockTransport([])
        let exec = FakeExecutableAdapter(currentExecutablePath: "/tmp/source/crm-mac")
        let installer = Installer(InstallerDependencies(
            paths: paths,
            filesystem: fs,
            executable: exec,
            keychain: keychain,
            agentService: agentService,
            processSignaller: FakeProcessSignaller(),
            bundleAssembler: BundleAssembler(filesystem: fs, executable: exec),
            piClientFactory: { url in
                PiClient(
                    baseURL: url,
                    transport: transport.asTransport(),
                    sleep: noopSleep)
            },
            clock: FixedClock(),
            logger: NoopLogger()))
        return (installer, fs, agentService, keychain, paths, transport, exec)
    }
}
