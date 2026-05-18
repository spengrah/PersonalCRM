import XCTest
import CRMMacCore
@testable import CRMMacLifecycle
@testable import CRMMacPiClient

final class InstallerRegisterOnlyTests: XCTestCase {
    func testRegisterOnlyDoesNotTouchBundleOrKeychain() async throws {
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        fs.seedFile(at: "/tmp/source/crm-mac")
        // Existing bundle on disk.
        try fs.createDirectory(at: paths.bundleAppPath)
        let originalBinary = Data("untouched binary".utf8)
        try fs.write(originalBinary, to: paths.bundleBinaryPath)
        // Existing embedded plist with placeholder already substituted
        // (re-running register-only on a healthy install is the
        // common case).
        try fs.write(
            Data("<plist>installed</plist>".utf8),
            to: paths.bundlePlistPath)
        let config = DaemonConfig(
            piURL: URL(string: "https://pi.example.test")!,
            hostID: UUID(uuidString: "11111111-2222-3333-4444-555555555555")!,
            hostname: "mac-1",
            installedAt: Date())
        let encoder = JSONEncoder()
        encoder.dateEncodingStrategy = .iso8601
        try fs.write(try encoder.encode(config), to: paths.configFilePath)
        let keychain = InMemoryKeychainStore(initial: "existing-key")
        let agentService = FakeAgentService()
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
                    transport: LifecycleMockTransport([]).asTransport(),
                    sleep: noopSleep)
            },
            clock: FixedClock(),
            logger: NoopLogger()))

        let summary = try await installer.run(InstallRequest(
            piURL: URL(string: "https://pi.example.test")!,
            pairingToken: "ignored",
            hostname: "ignored",
            registerOnly: true))
        XCTAssertEqual(summary.bundleBinaryPath, paths.bundleBinaryPath)
        // Binary content unchanged.
        XCTAssertEqual(try fs.read(from: paths.bundleBinaryPath), originalBinary)
        // Keychain unchanged.
        XCTAssertEqual(keychain.currentValue, "existing-key")
        // register was called; unregister was NOT.
        XCTAssertEqual(agentService.registerCalls, 1)
        XCTAssertEqual(agentService.unregisterCalls, 0)
        // No re-assembly: bundle codesign was NOT called.
        XCTAssertEqual(exec.bundleCodesignCalls.count, 0)
    }

    func testRegisterOnlyRequiresExistingBundle() async {
        // Config present + api-key present, but bundle missing — the
        // operator wants `--upgrade`, not register-only.
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        fs.seedFile(at: "/tmp/source/crm-mac")
        let config = DaemonConfig(
            piURL: URL(string: "https://pi.example.test")!,
            hostID: UUID(),
            hostname: "mac-1",
            installedAt: Date())
        let encoder = JSONEncoder()
        encoder.dateEncodingStrategy = .iso8601
        try? fs.write(try? encoder.encode(config), to: paths.configFilePath)
        let exec = FakeExecutableAdapter(currentExecutablePath: "/tmp/source/crm-mac")
        let installer = Installer(InstallerDependencies(
            paths: paths,
            filesystem: fs,
            executable: exec,
            keychain: InMemoryKeychainStore(initial: "existing-key"),
            agentService: FakeAgentService(),
            processSignaller: FakeProcessSignaller(),
            bundleAssembler: BundleAssembler(filesystem: fs, executable: exec),
            piClientFactory: { url in
                PiClient(baseURL: url, transport: LifecycleMockTransport([]).asTransport(), sleep: noopSleep)
            },
            clock: FixedClock(),
            logger: NoopLogger()))
        do {
            _ = try await installer.run(InstallRequest(
                piURL: URL(string: "https://x")!,
                pairingToken: "x", hostname: "x", registerOnly: true))
            XCTFail("expected noExistingInstall")
        } catch InstallError.noExistingInstall {
            // ok
        } catch {
            XCTFail("got \(error)")
        }
    }

    func testRegisterOnlyToleratesAlreadyRegistered() async throws {
        // Existing bundle + script the FakeAgentService to return
        // .alreadyRegistered. Register-only should succeed.
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        fs.seedFile(at: "/tmp/source/crm-mac")
        try fs.createDirectory(at: paths.bundleAppPath)
        try fs.write(Data("bin".utf8), to: paths.bundleBinaryPath)
        try fs.write(Data("<plist/>".utf8), to: paths.bundlePlistPath)
        let config = DaemonConfig(
            piURL: URL(string: "https://pi.example.test")!,
            hostID: UUID(),
            hostname: "mac-1",
            installedAt: Date())
        let encoder = JSONEncoder()
        encoder.dateEncodingStrategy = .iso8601
        try fs.write(try encoder.encode(config), to: paths.configFilePath)
        let keychain = InMemoryKeychainStore(initial: "k")
        var script = FakeAgentService.Script()
        script.nextRegisterOutcome = .alreadyRegistered
        let agent = FakeAgentService(script: script)
        let exec = FakeExecutableAdapter(currentExecutablePath: "/tmp/source/crm-mac")
        let installer = Installer(InstallerDependencies(
            paths: paths,
            filesystem: fs,
            executable: exec,
            keychain: keychain,
            agentService: agent,
            processSignaller: FakeProcessSignaller(),
            bundleAssembler: BundleAssembler(filesystem: fs, executable: exec),
            piClientFactory: { url in
                PiClient(baseURL: url, transport: LifecycleMockTransport([]).asTransport(), sleep: noopSleep)
            },
            clock: FixedClock(),
            logger: NoopLogger()))
        _ = try await installer.run(InstallRequest(
            piURL: URL(string: "https://x")!,
            pairingToken: "x",
            hostname: "x",
            registerOnly: true))
        XCTAssertEqual(agent.registerCalls, 1)
        XCTAssertEqual(agent.registerAlreadyRegisteredCount, 1)
    }
}
