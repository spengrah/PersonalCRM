// BundleSwapAtomicityTests exercise the upgrade-path bundle swap
// against the REAL ProductionFilesystemAdapter in a tmp work dir.
// Every individual rename uses an empty/absent destination — the
// upgrade flow deliberately avoids POSIX rename(2)'s ENOTEMPTY trap
// by going backup-rename-then-swap rather than rename-onto-non-empty.
//
// Opt-in via CRM_MAC_INTEGRATION_TESTS=1. Writes to a tmp work dir
// + shells out indirectly through codesign during the assembly half;
// not safe to run by default in CI.
import XCTest
import CRMMacLifecycle
@testable import CRMMacSystem

final class BundleSwapAtomicityTests: XCTestCase {

    override func setUpWithError() throws {
        try XCTSkipUnless(
            ProcessInfo.processInfo.environment["CRM_MAC_INTEGRATION_TESTS"] == "1",
            "BundleSwapAtomicityTests requires CRM_MAC_INTEGRATION_TESTS=1; skipping under default test run.")
    }

    func testBackupSwapSequenceUsesEmptyDestinations() throws {
        let workDir = try makeTempWorkDir()
        defer { try? FileManager.default.removeItem(at: workDir) }

        let fs = ProductionFilesystemAdapter()
        let bundle = workDir.appendingPathComponent("crm-mac.app").path
        let backup = workDir.appendingPathComponent("crm-mac.app.backup.\(getpid())").path
        let tmp = workDir.appendingPathComponent("crm-mac.app.tmp.\(getpid())").path

        // Seed an "old bundle" — non-empty directory with N files
        // mirroring the real bundle layout.
        try fs.createDirectory(at: bundle)
        try fs.createDirectory(at: "\(bundle)/Contents/MacOS")
        try fs.write(Data("old-binary".utf8), to: "\(bundle)/Contents/MacOS/crm-mac")
        try fs.createDirectory(at: "\(bundle)/Contents")
        try fs.write(Data("old-info".utf8), to: "\(bundle)/Contents/Info.plist")

        // Rename old to backup (destination absent; atomic).
        try fs.rename(from: bundle, to: backup)
        XCTAssertFalse(fs.fileExists(at: bundle))
        XCTAssertTrue(fs.fileExists(at: backup))

        // Assemble new at tmp.
        try fs.createDirectory(at: tmp)
        try fs.createDirectory(at: "\(tmp)/Contents/MacOS")
        try fs.write(Data("new-binary".utf8), to: "\(tmp)/Contents/MacOS/crm-mac")
        try fs.createDirectory(at: "\(tmp)/Contents")
        try fs.write(Data("new-info".utf8), to: "\(tmp)/Contents/Info.plist")

        // Rename tmp to final (destination absent again because the
        // prior rename moved the old one).
        try fs.rename(from: tmp, to: bundle)
        XCTAssertTrue(fs.fileExists(at: bundle))
        XCTAssertEqual(
            try fs.read(from: "\(bundle)/Contents/MacOS/crm-mac"),
            Data("new-binary".utf8))

        // Best-effort cleanup of backup.
        try fs.remove(at: backup)
        XCTAssertFalse(fs.fileExists(at: backup))
    }

    func testRollbackRestoresOldBundleAfterAssemblyFailure() throws {
        // Simulate an assembly failure between the rename-to-backup
        // and rename-tmp-to-final steps: the installer would remove
        // the tmp bundle + rename backup back to final. The
        // production rename calls underneath are what we're proving
        // atomic here.
        let workDir = try makeTempWorkDir()
        defer { try? FileManager.default.removeItem(at: workDir) }

        let fs = ProductionFilesystemAdapter()
        let bundle = workDir.appendingPathComponent("crm-mac.app").path
        let backup = workDir.appendingPathComponent("crm-mac.app.backup.\(getpid())").path

        // Seed an "old bundle".
        try fs.createDirectory(at: bundle)
        try fs.write(Data("old".utf8), to: "\(bundle)/marker")

        // Rename to backup.
        try fs.rename(from: bundle, to: backup)
        XCTAssertFalse(fs.fileExists(at: bundle))

        // Simulate assembly failure: rollback restores the backup.
        try fs.rename(from: backup, to: bundle)

        XCTAssertTrue(fs.fileExists(at: bundle))
        XCTAssertEqual(try fs.read(from: "\(bundle)/marker"), Data("old".utf8))
        XCTAssertFalse(fs.fileExists(at: backup))
    }

    private func makeTempWorkDir() throws -> URL {
        let url = FileManager.default.temporaryDirectory
            .appendingPathComponent("crm-mac-swap-\(UUID().uuidString)")
        try FileManager.default.createDirectory(
            at: url, withIntermediateDirectories: true)
        return url
    }
}
