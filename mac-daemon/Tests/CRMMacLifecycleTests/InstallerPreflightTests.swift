import XCTest
import CRMMacCore
@testable import CRMMacLifecycle
@testable import CRMMacPiClient

final class InstallerPreflightTests: XCTestCase {
    func testRefusesOnExistingBundle() async {
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        fs.seedFile(at: "/tmp/source/crm-mac")
        try? fs.createDirectory(at: paths.bundleAppPath)
        let installer = makeInstaller(paths: paths, fs: fs)
        await assertThrowsAlreadyInstalled(installer)
    }

    func testRefusesOnExistingLegacyBinary() async {
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        fs.seedFile(at: "/tmp/source/crm-mac")
        try? fs.write(Data("old".utf8), to: paths.legacyBinaryPath)
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

    func testRefusesWhenAgentServiceReportsEnabled() async {
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        fs.seedFile(at: "/tmp/source/crm-mac")
        var script = FakeAgentService.Script()
        script.statusSequence = [.enabled]
        let agent = FakeAgentService(script: script)
        let installer = makeInstaller(paths: paths, fs: fs, agentService: agent)
        await assertThrowsAlreadyInstalled(installer)
    }

    func testUpgradeBypassesPreflight() async throws {
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        fs.seedFile(at: "/tmp/source/crm-mac")
        try fs.createDirectory(at: paths.bundleAppPath)
        try fs.write(Data("old".utf8), to: paths.bundleBinaryPath)
        let config = DaemonConfig(
            piURL: URL(string: "https://pi.example.test")!,
            hostID: UUID(uuidString: "11111111-2222-3333-4444-555555555555")!,
            hostname: "mac-1",
            installedAt: Date())
        let encoder = JSONEncoder()
        encoder.dateEncodingStrategy = .iso8601
        try fs.write(try encoder.encode(config), to: paths.configFilePath)
        let keychain = InMemoryKeychainStore(initial: "existing-key")
        let exec = FakeExecutableAdapter(currentExecutablePath: "/tmp/source/crm-mac")
        let installer = Installer(InstallerDependencies(
            paths: paths,
            filesystem: fs,
            executable: exec,
            keychain: keychain,
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

        _ = try await installer.run(InstallRequest(
            piURL: URL(string: "https://pi.example.test")!,
            pairingToken: "ignored",
            hostname: "ignored",
            upgrade: true))
    }

    // MARK: - helpers

    private func makeInstaller(
        paths: LifecyclePaths,
        fs: InMemoryFilesystem,
        keychain: InMemoryKeychainStore = InMemoryKeychainStore(),
        agentService: FakeAgentService = FakeAgentService()
    ) -> Installer {
        let exec = FakeExecutableAdapter(currentExecutablePath: "/tmp/source/crm-mac")
        return Installer(InstallerDependencies(
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
