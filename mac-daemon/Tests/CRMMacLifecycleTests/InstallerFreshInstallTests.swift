import XCTest
import CRMMacCore
@testable import CRMMacLifecycle
@testable import CRMMacPiClient

final class InstallerFreshInstallTests: XCTestCase {
    func testHappyPathPersistsAndBootstraps() async throws {
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        fs.seedFile(at: "/tmp/source/crm-mac")
        let keychain = InMemoryKeychainStore()
        let launchctl = FakeLaunchctlRunner()
        let transport = LifecycleMockTransport([.respond(status: 200, data: pair200JSON)])
        let exec = FakeExecutableAdapter(currentExecutablePath: "/tmp/source/crm-mac")
        let installer = Installer(InstallerDependencies(
            paths: paths,
            filesystem: fs,
            executable: exec,
            keychain: keychain,
            launchctl: launchctl,
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

        XCTAssertEqual(summary.binaryPath, paths.binaryPath)
        XCTAssertEqual(summary.plistPath, paths.plistPath)

        // Binary is at its final location.
        XCTAssertTrue(fs.fileExists(at: paths.binaryPath))
        // No leftover tmp.
        XCTAssertFalse(fs.allPaths.contains(where: { $0.contains(".tmp.") }))
        // Codesign was invoked.
        XCTAssertEqual(exec.codesignCalls.count, 1)
        // Config exists and parses with the expected hostname.
        let configData = try fs.read(from: paths.configFilePath)
        let cfg = try JSONDecoder().decode(DaemonConfig.self, from: configData)
        XCTAssertEqual(cfg.hostname, "mac-1")
        // State exists and parses with schema version 1.
        let stateData = try fs.read(from: paths.stateFilePath)
        let state = try JSONDecoder().decode(DaemonState.self, from: stateData)
        XCTAssertEqual(state.schemaVersion, 1)
        // Keychain holds the api key.
        XCTAssertEqual(keychain.currentValue, "k")
        // Plist + bootstrap.
        XCTAssertTrue(fs.fileExists(at: paths.plistPath))
        XCTAssertEqual(launchctl.bootstrapCalls.count, 1)
    }

    func testDirectoriesCreated() async throws {
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        fs.seedFile(at: "/tmp/source/crm-mac")
        let installer = Installer(InstallerDependencies(
            paths: paths,
            filesystem: fs,
            executable: FakeExecutableAdapter(currentExecutablePath: "/tmp/source/crm-mac"),
            keychain: InMemoryKeychainStore(),
            launchctl: FakeLaunchctlRunner(),
            piClientFactory: { url in
                PiClient(
                    baseURL: url,
                    transport: LifecycleMockTransport([.respond(status: 200, data: pair200JSON)]).asTransport(),
                    sleep: noopSleep)
            },
            clock: FixedClock(),
            logger: NoopLogger()))
        _ = try await installer.run(InstallRequest(
            piURL: URL(string: "https://pi.example.test")!,
            pairingToken: "tk",
            hostname: "mac-1"))
        XCTAssertTrue(fs.allDirs.contains(paths.configDirPath))
        XCTAssertTrue(fs.allDirs.contains(paths.binDirPath))
        XCTAssertTrue(fs.allDirs.contains(paths.logsDirPath))
        XCTAssertTrue(fs.allDirs.contains(paths.launchAgentsDirPath))
    }

    func testRequiresHostname() async {
        let installer = makeInstaller(transportSteps: [])
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
        let installer = makeInstaller(transportSteps: [])
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

    private func makeInstaller(transportSteps: [LifecycleMockTransport.Step]) -> Installer {
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        fs.seedFile(at: "/tmp/source/crm-mac")
        return Installer(InstallerDependencies(
            paths: paths,
            filesystem: fs,
            executable: FakeExecutableAdapter(currentExecutablePath: "/tmp/source/crm-mac"),
            keychain: InMemoryKeychainStore(),
            launchctl: FakeLaunchctlRunner(),
            piClientFactory: { url in
                PiClient(
                    baseURL: url,
                    transport: LifecycleMockTransport(transportSteps).asTransport(),
                    sleep: noopSleep)
            },
            clock: FixedClock(),
            logger: NoopLogger()))
    }
}
