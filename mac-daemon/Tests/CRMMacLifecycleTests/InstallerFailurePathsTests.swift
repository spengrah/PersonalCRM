import XCTest
import CRMMacCore
@testable import CRMMacLifecycle
@testable import CRMMacPiClient

final class InstallerFailurePathsTests: XCTestCase {
    private func makeInstaller(
        transport: LifecycleMockTransport,
        launchctl: FakeLaunchctlRunner = FakeLaunchctlRunner(),
        keychain: InMemoryKeychainStore = InMemoryKeychainStore(),
        executable: FakeExecutableAdapter? = nil
    ) -> (Installer, InMemoryFilesystem, FakeLaunchctlRunner, InMemoryKeychainStore, LifecyclePaths) {
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        fs.seedFile(at: "/tmp/source/crm-mac")
        let exec = executable ?? FakeExecutableAdapter(currentExecutablePath: "/tmp/source/crm-mac")
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
        return (installer, fs, launchctl, keychain, paths)
    }

    func test410CleansTempBinaryAndDoesNotPersist() async {
        let transport = LifecycleMockTransport([.respond(status: 410, data: pair410JSON)])
        let (installer, fs, launchctl, keychain, paths) = makeInstaller(transport: transport)

        do {
            _ = try await installer.run(InstallRequest(
                piURL: URL(string: "https://pi.example.test")!,
                pairingToken: "bad",
                hostname: "mac-1"))
            XCTFail("expected throw")
        } catch InstallError.pairFailed {
            // ok
        } catch {
            XCTFail("got \(error)")
        }
        // No artifact at the install location.
        XCTAssertFalse(fs.fileExists(at: paths.binaryPath))
        // No tmp file remaining.
        XCTAssertFalse(fs.allPaths.contains(where: { $0.contains(".tmp.") }))
        // No config / state / Keychain side effects.
        XCTAssertFalse(fs.fileExists(at: paths.configFilePath))
        XCTAssertFalse(fs.fileExists(at: paths.stateFilePath))
        XCTAssertNil(keychain.currentValue)
        // No launchctl bootstrap attempted.
        XCTAssertEqual(launchctl.bootstrapCalls.count, 0)
    }

    func test409CleansTempBinary() async {
        let transport = LifecycleMockTransport([.respond(status: 409, data: pair409JSON)])
        let (installer, fs, _, _, paths) = makeInstaller(transport: transport)
        do {
            _ = try await installer.run(InstallRequest(
                piURL: URL(string: "https://pi.example.test")!,
                pairingToken: "tk",
                hostname: "mac-1"))
            XCTFail("expected throw")
        } catch InstallError.pairFailed {
            // ok
        } catch {
            XCTFail("got \(error)")
        }
        XCTAssertFalse(fs.fileExists(at: paths.binaryPath))
        XCTAssertFalse(fs.allPaths.contains(where: { $0.contains(".tmp.") }))
    }

    func test5xxCleansTempBinaryAndSurfacesAmbiguous() async {
        // Pair is no-retry so we expect only 1 attempt; 5xx now
        // surfaces as ambiguousPair so the operator gets the
        // list-hosts recovery guidance.
        let transport = LifecycleMockTransport([
            .respond(status: 502, data: Data("{}".utf8)),
        ])
        let (installer, fs, _, _, paths) = makeInstaller(transport: transport)
        do {
            _ = try await installer.run(InstallRequest(
                piURL: URL(string: "https://pi.example.test")!,
                pairingToken: "tk",
                hostname: "mac-1"))
            XCTFail("expected throw")
        } catch InstallError.ambiguousPair {
            // ok
        } catch {
            XCTFail("got \(error)")
        }
        XCTAssertFalse(fs.fileExists(at: paths.binaryPath))
        XCTAssertEqual(transport.invocations.count, 1, "pair must not retry on 5xx")
    }

    func testNetworkErrorSurfacesAmbiguous() async {
        let transport = LifecycleMockTransport([
            .fail(URLError(.timedOut)),
        ])
        let (installer, fs, _, _, paths) = makeInstaller(transport: transport)
        do {
            _ = try await installer.run(InstallRequest(
                piURL: URL(string: "https://pi.example.test")!,
                pairingToken: "tk",
                hostname: "mac-1"))
            XCTFail("expected throw")
        } catch InstallError.ambiguousPair {
            // ok
        } catch {
            XCTFail("got \(error)")
        }
        XCTAssertFalse(fs.fileExists(at: paths.binaryPath))
    }

    func testLaunchctlBootstrapFailureLeavesBinaryInPlace() async throws {
        let transport = LifecycleMockTransport([.respond(status: 200, data: pair200JSON)])
        var script = FakeLaunchctlRunner.Script()
        script.bootstrap = [42]
        let launchctl = FakeLaunchctlRunner(script: script)
        let (installer, fs, _, keychain, paths) = makeInstaller(
            transport: transport,
            launchctl: launchctl)
        do {
            _ = try await installer.run(InstallRequest(
                piURL: URL(string: "https://pi.example.test")!,
                pairingToken: "tk",
                hostname: "mac-1"))
            XCTFail("expected throw")
        } catch InstallError.launchctlFailed(let code, _) {
            XCTAssertEqual(code, 42)
        } catch {
            XCTFail("got \(error)")
        }
        // Binary IS in place. Config + Keychain + state present. Operator
        // recovers via `crm-mac install --register-only`.
        XCTAssertTrue(fs.fileExists(at: paths.binaryPath))
        XCTAssertTrue(fs.fileExists(at: paths.configFilePath))
        XCTAssertTrue(fs.fileExists(at: paths.stateFilePath))
        XCTAssertEqual(keychain.currentValue, "k")
    }

    func testCodesignFailureCleansTempBinaryNoPair() async {
        let transport = LifecycleMockTransport([.respond(status: 200, data: pair200JSON)])
        let exec = FakeExecutableAdapter(currentExecutablePath: "/tmp/source/crm-mac")
        exec.failCodesignWith = "ad-hoc signing requires Mac developer mode"
        let (installer, fs, _, _, paths) = makeInstaller(transport: transport, executable: exec)
        do {
            _ = try await installer.run(InstallRequest(
                piURL: URL(string: "https://pi.example.test")!,
                pairingToken: "tk",
                hostname: "mac-1"))
            XCTFail("expected throw")
        } catch InstallError.codesignFailed {
            // ok
        } catch {
            XCTFail("got \(error)")
        }
        XCTAssertFalse(fs.fileExists(at: paths.binaryPath))
        XCTAssertFalse(fs.allPaths.contains(where: { $0.contains(".tmp.") }))
        // Pair was NOT attempted — codesign failure is pre-pair.
        XCTAssertEqual(transport.invocations.count, 0)
    }
}
