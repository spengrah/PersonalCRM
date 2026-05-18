import XCTest
import CRMMacCore
@testable import CRMMacLifecycle
@testable import CRMMacPiClient

final class InstallerFreshInstallTests: XCTestCase {
    func testHappyPathPersistsAndRegisters() async throws {
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        fs.seedFile(at: "/tmp/source/crm-mac")
        let keychain = InMemoryKeychainStore()
        let agentService = FakeAgentService()
        let signaller = FakeProcessSignaller()
        let transport = LifecycleMockTransport([.respond(status: 200, data: pair200JSON)])
        let exec = FakeExecutableAdapter(currentExecutablePath: "/tmp/source/crm-mac")
        let assembler = BundleAssembler(filesystem: fs, executable: exec)
        let installer = Installer(InstallerDependencies(
            paths: paths,
            filesystem: fs,
            executable: exec,
            keychain: keychain,
            agentService: agentService,
            processSignaller: signaller,
            bundleAssembler: assembler,
            piClientFactory: { url in
                PiClient(
                    baseURL: url,
                    transport: transport.asTransport(),
                    sleep: noopSleep)
            },
            clock: FixedClock(),
            logger: NoopLogger()))

        let summary = try await installer.run(InstallRequest(
            piURL: URL(string: "https://pi.example.test")!,
            pairingToken: "tk",
            hostname: "mac-1"))

        XCTAssertEqual(summary.bundleBinaryPath, paths.bundleBinaryPath)
        XCTAssertEqual(summary.bundleAppPath, paths.bundleAppPath)
        // Bundle structure is in place.
        XCTAssertTrue(fs.fileExists(at: paths.bundleAppPath))
        XCTAssertTrue(fs.fileExists(at: paths.bundleBinaryPath))
        XCTAssertTrue(fs.fileExists(at: paths.bundlePlistPath))
        XCTAssertTrue(fs.fileExists(at: paths.bundleInfoPlistPath))
        // No leftover tmp.
        XCTAssertFalse(fs.allPaths.contains(where: { $0.contains(".tmp.") }))
        XCTAssertFalse(fs.allDirs.contains(where: { $0.contains(".tmp.") }))
        // Bundle codesign was invoked once with the right identifier.
        XCTAssertEqual(exec.bundleCodesignCalls.count, 1)
        XCTAssertEqual(exec.bundleCodesignCalls.first?.identifier, Daemon.label)
        // Single-Mach-O codesign (legacy path) was NOT used.
        XCTAssertEqual(exec.codesignCalls.count, 0)
        // Config exists + parses.
        let decoder = JSONDecoder()
        decoder.dateDecodingStrategy = .iso8601
        let configData = try fs.read(from: paths.configFilePath)
        let cfg = try decoder.decode(DaemonConfig.self, from: configData)
        XCTAssertEqual(cfg.hostname, "mac-1")
        // State exists + parses with schema version 1.
        let stateData = try fs.read(from: paths.stateFilePath)
        let state = try decoder.decode(DaemonState.self, from: stateData)
        XCTAssertEqual(state.schemaVersion, 1)
        // Api-key persisted.
        XCTAssertEqual(keychain.currentValue, "k")
        // SMAppService.register was called.
        XCTAssertEqual(agentService.registerCalls, 1)
        // Placeholder substitution happened — the embedded plist no
        // longer contains __INSTALL_PREFIX__.
        let plistContent = try fs.read(from: paths.bundlePlistPath)
        let plistStr = String(data: plistContent, encoding: .utf8) ?? ""
        XCTAssertFalse(plistStr.contains("__INSTALL_PREFIX__"),
            "installer must substitute the placeholder")
        XCTAssertTrue(plistStr.contains(paths.bundleAppPath),
            "installer must substitute with the real bundle path")
    }

    func testDirectoriesCreated() async throws {
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        fs.seedFile(at: "/tmp/source/crm-mac")
        let exec = FakeExecutableAdapter(currentExecutablePath: "/tmp/source/crm-mac")
        let installer = makeInstaller(paths: paths, fs: fs, exec: exec)
        _ = try await installer.run(InstallRequest(
            piURL: URL(string: "https://pi.example.test")!,
            pairingToken: "tk",
            hostname: "mac-1"))
        XCTAssertTrue(fs.allDirs.contains(paths.configDirPath))
        XCTAssertTrue(fs.allDirs.contains(paths.logsDirPath))
        XCTAssertTrue(fs.allDirs.contains(paths.bundleAppPath))
        // Legacy bin/ is NOT created on fresh install.
        XCTAssertFalse(fs.allDirs.contains(paths.binDirPath),
            "fresh install must NOT create legacy bin/ directory")
    }

    func testRequiresHostname() async {
        let installer = makeInstaller()
        do {
            _ = try await installer.run(InstallRequest(
                piURL: URL(string: "https://pi.example.test")!,
                pairingToken: "tk",
                hostname: ""))
            XCTFail("expected throw")
        } catch InstallError.missingHostnameFlag {
            // ok
        } catch {
            XCTFail("got \(error)")
        }
    }

    func testEmptyPairingTokenRejected() async {
        let installer = makeInstaller()
        do {
            _ = try await installer.run(InstallRequest(
                piURL: URL(string: "https://pi.example.test")!,
                pairingToken: "",
                hostname: "mac-1"))
            XCTFail("expected throw")
        } catch InstallError.invalidPairingToken {
            // ok
        } catch {
            XCTFail("got \(error)")
        }
    }

    // MARK: - helpers

    private func makeInstaller(
        paths: LifecyclePaths? = nil,
        fs: InMemoryFilesystem? = nil,
        exec: FakeExecutableAdapter? = nil
    ) -> Installer {
        let p = paths ?? TestPaths.make()
        let f = fs ?? {
            let x = InMemoryFilesystem()
            x.seedFile(at: "/tmp/source/crm-mac")
            return x
        }()
        let e = exec ?? FakeExecutableAdapter(currentExecutablePath: "/tmp/source/crm-mac")
        return Installer(InstallerDependencies(
            paths: p,
            filesystem: f,
            executable: e,
            keychain: InMemoryKeychainStore(),
            agentService: FakeAgentService(),
            processSignaller: FakeProcessSignaller(),
            bundleAssembler: BundleAssembler(filesystem: f, executable: e),
            piClientFactory: { url in
                PiClient(
                    baseURL: url,
                    transport: LifecycleMockTransport([.respond(status: 200, data: pair200JSON)]).asTransport(),
                    sleep: noopSleep)
            },
            clock: FixedClock(),
            logger: NoopLogger()))
    }
}
