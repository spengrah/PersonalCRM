import XCTest
import CRMMacCore
@testable import CRMMacLifecycle
@testable import CRMMacPiClient

final class InstallerUpgradeTests: XCTestCase {
    func testUpgradeDoesNotPostHost() async throws {
        let (installer, fs, launchctl, _, paths, transport) = try prepareExistingInstall()
        let summary = try await installer.run(InstallRequest(
            piURL: URL(string: "https://pi.example.test")!,
            pairingToken: "ignored",
            hostname: "ignored",
            upgrade: true))
        XCTAssertEqual(transport.invocations.count, 0, "upgrade must NOT call POST /host")
        XCTAssertTrue(fs.fileExists(at: paths.binaryPath))
        // bootout then bootstrap.
        XCTAssertEqual(launchctl.bootoutCalls.count, 1)
        XCTAssertEqual(launchctl.bootstrapCalls.count, 1)
        XCTAssertEqual(summary.binaryPath, paths.binaryPath)
    }

    func testUpgradeRefusesWithNoExistingInstall() async {
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
        try fs.write(Data("old binary".utf8), to: paths.binaryPath)
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
        let launchctl = FakeLaunchctlRunner()
        let installer = Installer(InstallerDependencies(
            paths: paths,
            filesystem: fs,
            executable: FakeExecutableAdapter(currentExecutablePath: "/tmp/source/crm-mac"),
            keychain: primary,
            launchctl: launchctl,
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

    private func prepareExistingInstall() throws -> (Installer, InMemoryFilesystem, FakeLaunchctlRunner, InMemoryKeychainStore, LifecyclePaths, LifecycleMockTransport) {
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        fs.seedFile(at: "/tmp/source/crm-mac")
        // Pretend an install exists.
        try fs.write(Data("old binary".utf8), to: paths.binaryPath)
        let config = DaemonConfig(
            piURL: URL(string: "https://pi.example.test")!,
            hostID: UUID(uuidString: "11111111-2222-3333-4444-555555555555")!,
            hostname: "mac-1",
            installedAt: Date(timeIntervalSince1970: 1_700_000_000))
        let encoder = JSONEncoder()
        encoder.dateEncodingStrategy = .iso8601
        try fs.write(try encoder.encode(config), to: paths.configFilePath)

        let keychain = InMemoryKeychainStore(initial: "existing-key")
        let launchctl = FakeLaunchctlRunner()
        let transport = LifecycleMockTransport([])
        let installer = Installer(InstallerDependencies(
            paths: paths,
            filesystem: fs,
            executable: FakeExecutableAdapter(currentExecutablePath: "/tmp/source/crm-mac"),
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
        return (installer, fs, launchctl, keychain, paths, transport)
    }
}
