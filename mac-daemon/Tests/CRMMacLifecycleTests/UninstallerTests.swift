import XCTest
@testable import CRMMacLifecycle

final class UninstallerTests: XCTestCase {
    func testDefaultRemovesPlistAndKeychain() throws {
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        try fs.write(Data("plist".utf8), to: paths.plistPath)
        try fs.write(Data("config".utf8), to: paths.configFilePath)
        try fs.write(Data("binary".utf8), to: paths.binaryPath)
        let keychain = InMemoryKeychainStore(initial: "key")
        let launchctl = FakeLaunchctlRunner()
        let uninstaller = Uninstaller(UninstallerDependencies(
            paths: paths,
            filesystem: fs,
            keychain: keychain,
            launchctl: launchctl,
            logger: NoopLogger()))

        let summary = try uninstaller.run(UninstallRequest())
        XCTAssertTrue(summary.bootoutInvoked)
        XCTAssertTrue(summary.plistDeleted)
        XCTAssertTrue(summary.keychainDeleted)
        XCTAssertFalse(summary.purged)
        XCTAssertEqual(launchctl.bootoutCalls.count, 1)
        XCTAssertFalse(fs.fileExists(at: paths.plistPath))
        XCTAssertNil(keychain.currentValue)
        // config + binary still present (no --purge).
        XCTAssertTrue(fs.fileExists(at: paths.configFilePath))
        XCTAssertTrue(fs.fileExists(at: paths.binaryPath))
    }

    func testPurgeRemovesEverything() throws {
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        try fs.write(Data("plist".utf8), to: paths.plistPath)
        try fs.write(Data("config".utf8), to: paths.configFilePath)
        try fs.write(Data("state".utf8), to: paths.stateFilePath)
        try fs.write(Data("binary".utf8), to: paths.binaryPath)
        let keychain = InMemoryKeychainStore(initial: "key")
        let uninstaller = Uninstaller(UninstallerDependencies(
            paths: paths,
            filesystem: fs,
            keychain: keychain,
            launchctl: FakeLaunchctlRunner(),
            logger: NoopLogger()))

        let summary = try uninstaller.run(UninstallRequest(purge: true))
        XCTAssertTrue(summary.purged)
        XCTAssertFalse(fs.fileExists(at: paths.configFilePath))
        XCTAssertFalse(fs.fileExists(at: paths.stateFilePath))
        XCTAssertFalse(fs.fileExists(at: paths.binaryPath))
    }

    func testBootoutNonZeroExitIsTolerated() throws {
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        var script = FakeLaunchctlRunner.Script()
        script.bootout = [3]
        let launchctl = FakeLaunchctlRunner(script: script)
        let uninstaller = Uninstaller(UninstallerDependencies(
            paths: paths,
            filesystem: fs,
            keychain: InMemoryKeychainStore(),
            launchctl: launchctl,
            logger: NoopLogger()))
        let summary = try uninstaller.run(UninstallRequest())
        XCTAssertEqual(summary.bootoutExitCode, 3)
        XCTAssertTrue(summary.bootoutInvoked)
    }
}
