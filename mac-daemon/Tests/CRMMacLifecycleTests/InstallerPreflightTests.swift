import XCTest
import CRMMacCore
@testable import CRMMacLifecycle
@testable import CRMMacPiClient

final class InstallerPreflightTests: XCTestCase {
    func testRefusesOnExistingBinary() async {
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        fs.seedFile(at: "/tmp/source/crm-mac")
        try? fs.write(Data(), to: paths.binaryPath)
        let installer = makeInstaller(paths: paths, fs: fs)
        await assertThrowsAlreadyInstalled(installer)
    }

    func testRefusesOnExistingConfig() async {
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        fs.seedFile(at: "/tmp/source/crm-mac")
        try? fs.write(Data("{}".utf8), to: paths.configFilePath)
        let installer = makeInstaller(paths: paths, fs: fs)
        await assertThrowsAlreadyInstalled(installer)
    }

    func testRefusesWhenKeychainHasAPIKey() async {
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        fs.seedFile(at: "/tmp/source/crm-mac")
        let keychain = InMemoryKeychainStore(initial: "leftover")
        let installer = makeInstaller(paths: paths, fs: fs, keychain: keychain)
        await assertThrowsAlreadyInstalled(installer)
    }

    func testRefusesWhenLaunchctlReportsRegistered() async {
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        fs.seedFile(at: "/tmp/source/crm-mac")
        var script = FakeLaunchctlRunner.Script()
        // bootstrap default 0 is fine; printService 0 means "registered"
        script.printService = [0]
        let launchctl = FakeLaunchctlRunner(script: script)
        let installer = makeInstaller(paths: paths, fs: fs, launchctl: launchctl)
        await assertThrowsAlreadyInstalled(installer)
    }

    func testUpgradeBypassesPreflight() async throws {
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        fs.seedFile(at: "/tmp/source/crm-mac")
        // Pretend an install exists.
        let config = DaemonConfig(
            piURL: URL(string: "https://pi.example.test")!,
            hostID: UUID(uuidString: "11111111-2222-3333-4444-555555555555")!,
            hostname: "mac-1",
            installedAt: Date())
        let encoder = JSONEncoder()
        encoder.dateEncodingStrategy = .iso8601
        try fs.write(try encoder.encode(config), to: paths.configFilePath)
        let keychain = InMemoryKeychainStore(initial: "existing-key")
        let launchctl = FakeLaunchctlRunner()
        let installer = Installer(InstallerDependencies(
            paths: paths,
            filesystem: fs,
            executable: FakeExecutableAdapter(currentExecutablePath: "/tmp/source/crm-mac"),
            keychain: keychain,
            launchctl: launchctl,
            piClientFactory: { url in
                PiClient(
                    baseURL: url,
                    transport: LifecycleMockTransport([]).asTransport(),
                    sleep: noopSleep)
            },
            clock: FixedClock(),
            logger: NoopLogger()))

        _ = try await installer.run(InstallRequest(
            piURL: URL(string: "https://pi.example.test")!,
            pairingToken: "ignored",
            hostname: "ignored",
            upgrade: true))
        // Upgrade does NOT POST /host — assert no transport activity.
        // (LifecycleMockTransport throws on use; reaching here means it
        // wasn't called.)
    }

    // MARK: - helpers

    private func makeInstaller(
        paths: LifecyclePaths,
        fs: InMemoryFilesystem,
        keychain: InMemoryKeychainStore = InMemoryKeychainStore(),
        launchctl: FakeLaunchctlRunner = FakeLaunchctlRunner()
    ) -> Installer {
        Installer(InstallerDependencies(
            paths: paths,
            filesystem: fs,
            executable: FakeExecutableAdapter(currentExecutablePath: "/tmp/source/crm-mac"),
            keychain: keychain,
            launchctl: launchctl,
            piClientFactory: { url in
                PiClient(
                    baseURL: url,
                    transport: LifecycleMockTransport([.respond(status: 200, data: pair200JSON)]).asTransport(),
                    sleep: noopSleep)
            },
            clock: FixedClock(),
            logger: NoopLogger()))
    }

    private func assertThrowsAlreadyInstalled(_ installer: Installer) async {
        do {
            _ = try await installer.run(InstallRequest(
                piURL: URL(string: "https://pi.example.test")!,
                pairingToken: "tk",
                hostname: "mac-1"))
            XCTFail("expected alreadyInstalled")
        } catch InstallError.alreadyInstalled {
            // ok
        } catch {
            XCTFail("got \(error)")
        }
    }
}
