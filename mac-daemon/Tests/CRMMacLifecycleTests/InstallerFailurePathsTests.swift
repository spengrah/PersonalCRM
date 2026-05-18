import XCTest
import CRMMacCore
@testable import CRMMacLifecycle
@testable import CRMMacPiClient

final class InstallerFailurePathsTests: XCTestCase {
    private func makeInstaller(
        transport: LifecycleMockTransport,
        agentService: FakeAgentService = FakeAgentService(),
        keychain: InMemoryKeychainStore = InMemoryKeychainStore(),
        executable: FakeExecutableAdapter? = nil
    ) -> (Installer, InMemoryFilesystem, FakeAgentService, InMemoryKeychainStore, LifecyclePaths, FakeExecutableAdapter) {
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        fs.seedFile(at: "/tmp/source/crm-mac")
        let exec = executable ?? FakeExecutableAdapter(currentExecutablePath: "/tmp/source/crm-mac")
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
        return (installer, fs, agentService, keychain, paths, exec)
    }

    func test410CleansTempBundleAndDoesNotPersist() async {
        let transport = LifecycleMockTransport([.respond(status: 410, data: pair410JSON)])
        let (installer, fs, agentService, keychain, paths, _) = makeInstaller(transport: transport)

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
        // No bundle at install location.
        XCTAssertFalse(fs.fileExists(at: paths.bundleAppPath))
        // No tmp.
        XCTAssertFalse(fs.allPaths.contains(where: { $0.contains(".tmp.") }))
        XCTAssertFalse(fs.allDirs.contains(where: { $0.contains(".tmp.") }))
        // No config / state / Keychain side effects.
        XCTAssertFalse(fs.fileExists(at: paths.configFilePath))
        XCTAssertFalse(fs.fileExists(at: paths.stateFilePath))
        XCTAssertNil(keychain.currentValue)
        // No agent registration attempted.
        XCTAssertEqual(agentService.registerCalls, 0)
    }

    func test409CleansTempBundle() async {
        let transport = LifecycleMockTransport([.respond(status: 409, data: pair409JSON)])
        let (installer, fs, _, _, paths, _) = makeInstaller(transport: transport)
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
        XCTAssertFalse(fs.fileExists(at: paths.bundleAppPath))
        XCTAssertFalse(fs.allDirs.contains(where: { $0.contains(".tmp.") }))
    }

    func test5xxCleansTempBundleAndSurfacesAmbiguous() async {
        let transport = LifecycleMockTransport([
            .respond(status: 502, data: Data("{}".utf8)),
        ])
        let (installer, fs, _, _, paths, _) = makeInstaller(transport: transport)
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
        XCTAssertFalse(fs.fileExists(at: paths.bundleAppPath))
        XCTAssertEqual(transport.invocations.count, 1, "pair must not retry on 5xx")
    }

    func testNetworkErrorSurfacesAmbiguous() async {
        let transport = LifecycleMockTransport([
            .fail(URLError(.timedOut)),
        ])
        let (installer, fs, _, _, paths, _) = makeInstaller(transport: transport)
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
        XCTAssertFalse(fs.fileExists(at: paths.bundleAppPath))
    }

    func testBundleAssemblyFailureCleansTmpAndDoesNotPair() async {
        let transport = LifecycleMockTransport([.respond(status: 200, data: pair200JSON)])
        let exec = FakeExecutableAdapter(currentExecutablePath: "/tmp/source/crm-mac")
        exec.failBundleCodesignWith = "injected codesign failure"
        let (installer, fs, _, _, paths, _) = makeInstaller(transport: transport, executable: exec)
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
        // No bundle at final path; no tmp left over; no pair attempt.
        XCTAssertFalse(fs.fileExists(at: paths.bundleAppPath))
        XCTAssertFalse(fs.allDirs.contains(where: { $0.contains(".tmp.") }))
        XCTAssertEqual(transport.invocations.count, 0)
    }

    func testAgentRegistrationFailureLeavesBundleInPlace() async throws {
        let transport = LifecycleMockTransport([.respond(status: 200, data: pair200JSON)])
        var script = FakeAgentService.Script()
        script.registerThrows = .registrationFailed(
            message: "requires approval", requiresApproval: true)
        let agent = FakeAgentService(script: script)
        let (installer, fs, _, keychain, paths, _) = makeInstaller(
            transport: transport,
            agentService: agent)
        do {
            _ = try await installer.run(InstallRequest(
                piURL: URL(string: "https://pi.example.test")!,
                pairingToken: "tk",
                hostname: "mac-1"))
            XCTFail("expected throw")
        } catch InstallError.agentRegistrationFailed(_, let requiresApproval) {
            XCTAssertTrue(requiresApproval)
        } catch {
            XCTFail("got \(error)")
        }
        // Bundle, config, state, keychain all in place.
        XCTAssertTrue(fs.fileExists(at: paths.bundleAppPath))
        XCTAssertTrue(fs.fileExists(at: paths.configFilePath))
        XCTAssertTrue(fs.fileExists(at: paths.stateFilePath))
        XCTAssertEqual(keychain.currentValue, "k")
    }

    func testInfoPlistWriteFailureCleansTmpAndDoesNotPair() async {
        // Inject a failure when the assembler writes Info.plist into
        // the tmp bundle. Assembly fails before pair.
        let transport = LifecycleMockTransport([.respond(status: 200, data: pair200JSON)])
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        fs.seedFile(at: "/tmp/source/crm-mac")
        // Compute the expected tmp Info.plist path.
        let tmpInfoPlist =
            "\(paths.configDirPath)/crm-mac.app.tmp.\(ProcessInfo.processInfo.processIdentifier)/Contents/Info.plist"
        fs.failWritesAtPath = tmpInfoPlist
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
                PiClient(baseURL: url, transport: transport.asTransport(), sleep: noopSleep)
            },
            clock: FixedClock(),
            logger: NoopLogger()))
        do {
            _ = try await installer.run(InstallRequest(
                piURL: URL(string: "https://pi.example.test")!,
                pairingToken: "tk",
                hostname: "mac-1"))
            XCTFail("expected throw")
        } catch InstallError.filesystemFailed {
            // ok
        } catch {
            XCTFail("got \(error)")
        }
        XCTAssertFalse(fs.fileExists(at: paths.bundleAppPath))
        XCTAssertEqual(transport.invocations.count, 0)
    }
}
