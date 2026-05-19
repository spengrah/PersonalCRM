// InstallerRollbackFailureTests cover the `upgradeRollbackFailed`
// composed-error path. If the
// initial register fails AND the backup-restore rename ALSO fails,
// the installer must surface BOTH errors so the operator knows the
// previous bundle is stranded at `<bundle>.backup.<pid>`.
//
// Uses a FilesystemAdapter that delegates to InMemoryFilesystem but
// injects a one-shot rename failure for the restore step.
import XCTest
import CRMMacCore
@testable import CRMMacLifecycle
@testable import CRMMacPiClient

/// Filesystem fake that delegates to an underlying `InMemoryFilesystem`
/// and fails a rename when the source path matches a configured
/// prefix. Used to exercise the upgrade rollback's
/// restore-backup-failure branch.
final class RenameFailingFilesystem: FilesystemAdapter, @unchecked Sendable {
    let underlying: InMemoryFilesystem
    /// If non-nil, any rename whose `from` path begins with this
    /// prefix throws an injected ioError instead of succeeding.
    var failRenameFromPrefix: String?

    init(_ underlying: InMemoryFilesystem) {
        self.underlying = underlying
    }
    func createDirectory(at path: String) throws { try underlying.createDirectory(at: path) }
    func copy(from src: String, to dst: String) throws { try underlying.copy(from: src, to: dst) }
    func rename(from src: String, to dst: String) throws {
        if let prefix = failRenameFromPrefix, src.hasPrefix(prefix) {
            throw FilesystemError.ioError("injected rename failure for \(src)")
        }
        try underlying.rename(from: src, to: dst)
    }
    func remove(at path: String) throws { try underlying.remove(at: path) }
    func makeExecutable(at path: String) throws { try underlying.makeExecutable(at: path) }
    func fileExists(at path: String) -> Bool { underlying.fileExists(at: path) }
    func write(_ data: Data, to path: String) throws { try underlying.write(data, to: path) }
    func read(from path: String) throws -> Data { try underlying.read(from: path) }
}

final class InstallerRollbackFailureTests: XCTestCase {
    func testUpgradeRollbackFailureSurfacesComposedError() async throws {
        let paths = TestPaths.make()
        let inner = InMemoryFilesystem()
        inner.seedFile(at: "/tmp/source/crm-mac")
        try inner.createDirectory(at: paths.bundleAppPath)
        try inner.write(Data("old-bin".utf8), to: paths.bundleBinaryPath)
        let config = DaemonConfig(
            piURL: URL(string: "https://x")!,
            hostID: UUID(),
            hostname: "mac-1",
            installedAt: Date())
        let encoder = JSONEncoder()
        encoder.dateEncodingStrategy = .iso8601
        try inner.write(try encoder.encode(config), to: paths.configFilePath)
        let fs = RenameFailingFilesystem(inner)
        // Make any rename from the backup path fail.
        let backupPrefix = "\(paths.configDirPath)/crm-mac.app.backup."
        fs.failRenameFromPrefix = backupPrefix

        var script = FakeAgentService.Script()
        script.registerThrows = .registrationFailed(
            message: "denied", requiresApproval: true)
        let agent = FakeAgentService(script: script)
        let exec = FakeExecutableAdapter(currentExecutablePath: "/tmp/source/crm-mac")
        let installer = Installer(InstallerDependencies(
            paths: paths,
            filesystem: fs,
            executable: exec,
            keychain: InMemoryKeychainStore(initial: "k"),
            agentService: agent,
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
                pairingToken: "ignored",
                hostname: "ignored",
                upgrade: true))
            XCTFail("expected upgradeRollbackFailed")
        } catch InstallError.upgradeRollbackFailed(let original, let restore, let backup) {
            // The original error is the InstallError.agentRegistrationFailed
            // case; its `description` renders "agent registration failed: ..."
            // (lowercase). The composed-error path passes that rendered
            // string through `String(describing:)`.
            XCTAssertTrue(original.lowercased().contains("registration") || original.lowercased().contains("registrationfailed"),
                "original error must describe the register failure; got \(original)")
            XCTAssertTrue(restore.contains("injected rename failure"),
                "restore error must describe the rename failure; got \(restore)")
            XCTAssertTrue(backup.hasPrefix(backupPrefix),
                "composed error must carry the surviving backup path; got \(backup)")
        } catch {
            XCTFail("got \(error)")
        }
    }
}
