import XCTest
import CRMMacCore
@testable import CRMMacLifecycle
@testable import CRMMacPiClient

final class InstallerRegisterOnlyTests: XCTestCase {
    func testRegisterOnlyDoesNotTouchBinaryOrKeychain() async throws {
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        fs.seedFile(at: "/tmp/source/crm-mac")
        let originalBinary = Data("untouched binary".utf8)
        try fs.write(originalBinary, to: paths.binaryPath)
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
        XCTAssertEqual(summary.binaryPath, paths.binaryPath)
        // Binary content unchanged.
        XCTAssertEqual(try fs.read(from: paths.binaryPath), originalBinary)
        // Keychain unchanged.
        XCTAssertEqual(keychain.currentValue, "existing-key")
        // bootstrap was called, NO bootout.
        XCTAssertEqual(launchctl.bootstrapCalls.count, 1)
        XCTAssertEqual(launchctl.bootoutCalls.count, 0)
        XCTAssertEqual(exec.codesignCalls.count, 0)
    }
}
