import XCTest
import CRMMacCore
@testable import CRMMacLifecycle

final class UninstallerTests: XCTestCase {
    func testDefaultRemovesBundleAndKeychain() async throws {
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        try fs.createDirectory(at: paths.bundleAppPath)
        try fs.write(Data("bin".utf8), to: paths.bundleBinaryPath)
        try fs.write(Data("plist".utf8), to: paths.bundlePlistPath)
        try fs.write(Data("info".utf8), to: paths.bundleInfoPlistPath)
        try fs.write(Data("config".utf8), to: paths.configFilePath)
        let keychain = InMemoryKeychainStore(initial: "key")
        let agentService = FakeAgentService()
        let uninstaller = Uninstaller(UninstallerDependencies(
            paths: paths,
            filesystem: fs,
            keychain: keychain,
            agentService: agentService,
            processSignaller: FakeProcessSignaller(),
            logger: NoopLogger()))

        let summary = try await uninstaller.run(UninstallRequest())
        XCTAssertTrue(summary.unregisterInvoked)
        XCTAssertTrue(summary.bundleDeleted)
        XCTAssertTrue(summary.keychainDeleted)
        XCTAssertFalse(summary.purged)
        XCTAssertEqual(agentService.unregisterCalls, 1)
        XCTAssertFalse(fs.fileExists(at: paths.bundleAppPath))
        XCTAssertFalse(fs.fileExists(at: paths.bundleBinaryPath))
        XCTAssertNil(keychain.currentValue)
        // config still present (no --purge).
        XCTAssertTrue(fs.fileExists(at: paths.configFilePath))
    }

    func testPurgeRemovesEverything() async throws {
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        try fs.createDirectory(at: paths.bundleAppPath)
        try fs.write(Data("bin".utf8), to: paths.bundleBinaryPath)
        try fs.write(Data("config".utf8), to: paths.configFilePath)
        try fs.write(Data("state".utf8), to: paths.stateFilePath)
        let icloudHashCachePath = URL(fileURLWithPath: paths.configDirPath)
            .appendingPathComponent("icloud_contacts_hashes.json").path
        try fs.write(Data("{\"schema_version\":1,\"hashes\":{}}".utf8),
                     to: icloudHashCachePath)
        let keychain = InMemoryKeychainStore(initial: "key")
        let uninstaller = Uninstaller(UninstallerDependencies(
            paths: paths,
            filesystem: fs,
            keychain: keychain,
            agentService: FakeAgentService(),
            processSignaller: FakeProcessSignaller(),
            logger: NoopLogger()))

        let summary = try await uninstaller.run(UninstallRequest(purge: true))
        XCTAssertTrue(summary.purged)
        XCTAssertFalse(fs.fileExists(at: paths.configFilePath))
        XCTAssertFalse(fs.fileExists(at: paths.stateFilePath))
        XCTAssertFalse(fs.fileExists(at: paths.bundleAppPath))
        XCTAssertFalse(fs.fileExists(at: icloudHashCachePath),
                       "purge must include icloud_contacts_hashes.json so a re-pair starts clean")
    }

    func testPurgeLeavesIcloudHashCacheWhenNotPurging() async throws {
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        try fs.createDirectory(at: paths.bundleAppPath)
        let icloudHashCachePath = URL(fileURLWithPath: paths.configDirPath)
            .appendingPathComponent("icloud_contacts_hashes.json").path
        try fs.write(Data("{\"schema_version\":1,\"hashes\":{}}".utf8),
                     to: icloudHashCachePath)
        let uninstaller = Uninstaller(UninstallerDependencies(
            paths: paths,
            filesystem: fs,
            keychain: InMemoryKeychainStore(initial: "k"),
            agentService: FakeAgentService(),
            processSignaller: FakeProcessSignaller(),
            logger: NoopLogger()))
        _ = try await uninstaller.run(UninstallRequest(purge: false))
        XCTAssertTrue(fs.fileExists(at: icloudHashCachePath),
                      "default uninstall preserves icloud hash cache")
    }

    func testBundleAlreadyAbsentSurfacesAsNotDeleted() async throws {
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        // No bundle seeded.
        let uninstaller = Uninstaller(UninstallerDependencies(
            paths: paths,
            filesystem: fs,
            keychain: InMemoryKeychainStore(initial: "k"),
            agentService: FakeAgentService(),
            processSignaller: FakeProcessSignaller(),
            logger: NoopLogger()))
        let summary = try await uninstaller.run(UninstallRequest())
        XCTAssertFalse(summary.bundleDeleted, "bundle was already absent")
        XCTAssertTrue(summary.keychainDeleted)
    }

    func testKeychainAlreadyAbsentStillSucceeds() async throws {
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        let keychain = InMemoryKeychainStore()
        let uninstaller = Uninstaller(UninstallerDependencies(
            paths: paths,
            filesystem: fs,
            keychain: keychain,
            agentService: FakeAgentService(),
            processSignaller: FakeProcessSignaller(),
            logger: NoopLogger()))
        let summary = try await uninstaller.run(UninstallRequest())
        XCTAssertTrue(summary.keychainDeleted, "delete on missing entry is a successful no-op")
    }

    func testUnregisterErrorIsTolerated() async throws {
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        try fs.createDirectory(at: paths.bundleAppPath)
        try fs.write(Data("bin".utf8), to: paths.bundleBinaryPath)
        var script = FakeAgentService.Script()
        script.unregisterThrows = .unregistrationFailed("not registered")
        let agent = FakeAgentService(script: script)
        let uninstaller = Uninstaller(UninstallerDependencies(
            paths: paths,
            filesystem: fs,
            keychain: InMemoryKeychainStore(initial: "k"),
            agentService: agent,
            processSignaller: FakeProcessSignaller(),
            logger: NoopLogger()))
        let summary = try await uninstaller.run(UninstallRequest())
        // Continues even though unregister threw.
        XCTAssertTrue(summary.unregisterInvoked)
        XCTAssertTrue(summary.bundleDeleted)
        XCTAssertTrue(summary.keychainDeleted)
    }

    func testLegacyArtifactsCleanedUp() async throws {
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        // Seed both legacy artifacts + a new bundle (post-migration
        // could-have-been-state where uninstall sweeps the leftovers).
        try fs.write(Data("legacy plist".utf8), to: paths.legacyPlistPath)
        try fs.write(Data("legacy bin".utf8), to: paths.legacyBinaryPath)
        try fs.createDirectory(at: paths.bundleAppPath)
        try fs.write(Data("new bin".utf8), to: paths.bundleBinaryPath)
        let uninstaller = Uninstaller(UninstallerDependencies(
            paths: paths,
            filesystem: fs,
            keychain: InMemoryKeychainStore(initial: "k"),
            agentService: FakeAgentService(),
            processSignaller: FakeProcessSignaller(),
            logger: NoopLogger(),
            legacyLaunchctl: FakeLaunchctlRunner()))
        let summary = try await uninstaller.run(UninstallRequest())
        XCTAssertTrue(summary.legacyPlistDeleted)
        XCTAssertTrue(summary.legacyBinaryDeleted)
        XCTAssertFalse(fs.fileExists(at: paths.legacyPlistPath))
        XCTAssertFalse(fs.fileExists(at: paths.legacyBinaryPath))
        XCTAssertFalse(fs.fileExists(at: paths.bundleAppPath))
    }
}
