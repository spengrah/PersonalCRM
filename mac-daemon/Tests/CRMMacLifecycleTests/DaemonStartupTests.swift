import XCTest
import CRMMacCore
@testable import CRMMacLifecycle

final class DaemonStartupTests: XCTestCase {
    private func writeConfig(to fs: InMemoryFilesystem, at path: String) throws {
        let cfg = DaemonConfig(
            piURL: URL(string: "https://pi.example.test")!,
            hostID: UUID(uuidString: "11111111-2222-3333-4444-555555555555")!,
            hostname: "mac-1",
            installedAt: Date(timeIntervalSince1970: 1_700_000_000))
        let encoder = JSONEncoder()
        encoder.dateEncodingStrategy = .iso8601
        try fs.write(try encoder.encode(cfg), to: path)
    }

    private func writeState(to fs: InMemoryFilesystem, at path: String) throws {
        let state = DaemonState(schemaVersion: 1)
        let encoder = JSONEncoder()
        encoder.dateEncodingStrategy = .iso8601
        try fs.write(try encoder.encode(state), to: path)
    }

    private func makeStartup(
        fs: InMemoryFilesystem,
        keychain: InMemoryKeychainStore
    ) -> DaemonStartup {
        let paths = TestPaths.make()
        return DaemonStartup(
            paths: paths,
            keychain: keychain,
            logger: NoopLogger(),
            stateStoreFactory: { url in
                StateStore(fileURL: url, fileManager: .default)
            },
            configStoreFactory: { url in
                ConfigStore(fileURL: url, fileManager: .default)
            })
    }

    func testHappyPath() throws {
        // The DaemonStartup uses ConfigStore / StateStore directly,
        // which read from disk, not InMemoryFilesystem. We exercise
        // the failure-mapping paths (config / keychain / state) with
        // a temp directory below.
        let tempDir = FileManager.default.temporaryDirectory
            .appendingPathComponent("crm-mac-startup-\(UUID().uuidString)")
        defer { try? FileManager.default.removeItem(at: tempDir) }
        try FileManager.default.createDirectory(at: tempDir, withIntermediateDirectories: true)
        let configURL = tempDir.appendingPathComponent("config.json")
        let stateURL = tempDir.appendingPathComponent("state.json")
        let configStore = ConfigStore(fileURL: configURL)
        let stateStore = StateStore(fileURL: stateURL)
        let cfg = DaemonConfig(
            piURL: URL(string: "https://pi.example.test")!,
            hostID: UUID(uuidString: "11111111-2222-3333-4444-555555555555")!,
            hostname: "mac-1",
            installedAt: Date(timeIntervalSince1970: 1_700_000_000))
        try configStore.save(cfg)
        try stateStore.save(DaemonState(schemaVersion: 1))
        let keychain = InMemoryKeychainStore(initial: "k")

        let paths = LifecyclePaths(
            configDirPath: tempDir.path,
            binDirPath: "/dev/null",
            configFilePath: configURL.path,
            stateFilePath: stateURL.path,
            launchAgentsDirPath: "/dev/null",
            logsDirPath: "/dev/null",
            stdoutLogPath: "/dev/null",
            stderrLogPath: "/dev/null",
            bundleAppPath: "/dev/null",
            bundleBinaryPath: "/dev/null",
            bundlePlistPath: "/dev/null",
            bundleInfoPlistPath: "/dev/null",
            legacyBinaryPath: "/dev/null",
            legacyPlistPath: "/dev/null")
        let startup = DaemonStartup(paths: paths, keychain: keychain, logger: NoopLogger())
        let artifacts = try startup.run()
        XCTAssertEqual(artifacts.config.hostname, "mac-1")
        XCTAssertEqual(artifacts.apiKey, "k")
    }

    func testMissingConfigThrowsConfigError() {
        let tempDir = FileManager.default.temporaryDirectory
            .appendingPathComponent("crm-mac-startup-\(UUID().uuidString)")
        defer { try? FileManager.default.removeItem(at: tempDir) }
        let paths = LifecyclePaths(
            configDirPath: tempDir.path,
            binDirPath: "/dev/null",
            configFilePath: tempDir.appendingPathComponent("config.json").path,
            stateFilePath: tempDir.appendingPathComponent("state.json").path,
            launchAgentsDirPath: "/dev/null",
            logsDirPath: "/dev/null",
            stdoutLogPath: "/dev/null",
            stderrLogPath: "/dev/null",
            bundleAppPath: "/dev/null",
            bundleBinaryPath: "/dev/null",
            bundlePlistPath: "/dev/null",
            bundleInfoPlistPath: "/dev/null",
            legacyBinaryPath: "/dev/null",
            legacyPlistPath: "/dev/null")
        let startup = DaemonStartup(
            paths: paths,
            keychain: InMemoryKeychainStore(initial: "k"),
            logger: NoopLogger())
        XCTAssertThrowsError(try startup.run()) { error in
            guard let e = error as? DaemonStartupError, case .config = e else {
                XCTFail("expected config error, got \(error)")
                return
            }
            XCTAssertEqual(e.exitCode, .configFailure)
        }
    }

    func testMissingKeychainThrowsKeychainError() throws {
        let tempDir = FileManager.default.temporaryDirectory
            .appendingPathComponent("crm-mac-startup-\(UUID().uuidString)")
        defer { try? FileManager.default.removeItem(at: tempDir) }
        try FileManager.default.createDirectory(at: tempDir, withIntermediateDirectories: true)
        let configURL = tempDir.appendingPathComponent("config.json")
        try ConfigStore(fileURL: configURL).save(DaemonConfig(
            piURL: URL(string: "https://pi.example.test")!,
            hostID: UUID(),
            hostname: "mac-1",
            installedAt: Date(timeIntervalSince1970: 1_700_000_000)))
        let paths = LifecyclePaths(
            configDirPath: tempDir.path,
            binDirPath: "/dev/null",
            configFilePath: configURL.path,
            stateFilePath: tempDir.appendingPathComponent("state.json").path,
            launchAgentsDirPath: "/dev/null",
            logsDirPath: "/dev/null",
            stdoutLogPath: "/dev/null",
            stderrLogPath: "/dev/null",
            bundleAppPath: "/dev/null",
            bundleBinaryPath: "/dev/null",
            bundlePlistPath: "/dev/null",
            bundleInfoPlistPath: "/dev/null",
            legacyBinaryPath: "/dev/null",
            legacyPlistPath: "/dev/null")
        // Empty keychain — no api-key.
        let startup = DaemonStartup(
            paths: paths,
            keychain: InMemoryKeychainStore(),
            logger: NoopLogger())
        XCTAssertThrowsError(try startup.run()) { error in
            guard let e = error as? DaemonStartupError, case .keychain = e else {
                XCTFail("expected keychain error, got \(error)")
                return
            }
            XCTAssertEqual(e.exitCode, .keychainFailure)
        }
    }

    func testMissingStateThrowsStateError() throws {
        let tempDir = FileManager.default.temporaryDirectory
            .appendingPathComponent("crm-mac-startup-\(UUID().uuidString)")
        defer { try? FileManager.default.removeItem(at: tempDir) }
        try FileManager.default.createDirectory(at: tempDir, withIntermediateDirectories: true)
        let configURL = tempDir.appendingPathComponent("config.json")
        try ConfigStore(fileURL: configURL).save(DaemonConfig(
            piURL: URL(string: "https://pi.example.test")!,
            hostID: UUID(),
            hostname: "mac-1",
            installedAt: Date(timeIntervalSince1970: 1_700_000_000)))
        let paths = LifecyclePaths(
            configDirPath: tempDir.path,
            binDirPath: "/dev/null",
            configFilePath: configURL.path,
            stateFilePath: tempDir.appendingPathComponent("state.json").path,
            launchAgentsDirPath: "/dev/null",
            logsDirPath: "/dev/null",
            stdoutLogPath: "/dev/null",
            stderrLogPath: "/dev/null",
            bundleAppPath: "/dev/null",
            bundleBinaryPath: "/dev/null",
            bundlePlistPath: "/dev/null",
            bundleInfoPlistPath: "/dev/null",
            legacyBinaryPath: "/dev/null",
            legacyPlistPath: "/dev/null")
        let startup = DaemonStartup(
            paths: paths,
            keychain: InMemoryKeychainStore(initial: "k"),
            logger: NoopLogger())
        XCTAssertThrowsError(try startup.run()) { error in
            guard let e = error as? DaemonStartupError, case .state = e else {
                XCTFail("expected state error, got \(error)")
                return
            }
            XCTAssertEqual(e.exitCode, .stateFailure)
        }
    }

    func testExitCodeMap() {
        XCTAssertEqual(DaemonExitCode.clean.rawValue, 0)
        XCTAssertEqual(DaemonExitCode.authRevoked.rawValue, 1)
        XCTAssertEqual(DaemonExitCode.upgradeRequired.rawValue, 2)
        XCTAssertEqual(DaemonExitCode.configFailure.rawValue, 3)
        XCTAssertEqual(DaemonExitCode.keychainFailure.rawValue, 4)
        XCTAssertEqual(DaemonExitCode.stateFailure.rawValue, 5)
    }
}
