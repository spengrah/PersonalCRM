// BundleCodesignSealTests prove that the install-time bundle
// assembly produces a bundle whose codesign seal stays intact —
// i.e. a real `codesign --verify` against the assembled bundle
// exits 0.
//
// Regression coverage for the placeholder-substitution ordering bug:
// an earlier implementation wrote the embedded LaunchAgent plist with
// an __INSTALL_PREFIX__ placeholder, codesigned the bundle, then
// substituted the placeholder with the real bundle path after
// codesign — invalidating the seal. SMAppService accepted the first
// register() on a freshly-installed bundle but `crm-mac install
// --register-only` and `crm-mac doctor` failed with
// "Codesigning failure loading plist ... code: -67054" because
// they go through stricter codesign validation.
//
// Opt-in via CRM_MAC_INTEGRATION_TESTS=1. Shells out to
// /usr/bin/codesign + writes to a tmp work dir; not safe to run in
// arbitrary CI matrices.
import XCTest
import CRMMacLifecycle
@testable import CRMMacSystem

final class BundleCodesignSealTests: XCTestCase {

    override func setUpWithError() throws {
        try XCTSkipUnless(
            ProcessInfo.processInfo.environment["CRM_MAC_INTEGRATION_TESTS"] == "1",
            "BundleCodesignSealTests requires CRM_MAC_INTEGRATION_TESTS=1; skipping under default test run.")
    }

    func testAssembledBundleVerifiesUnderCodesign() throws {
        let workDir = try makeTempWorkDir()
        defer { try? FileManager.default.removeItem(at: workDir) }

        // Use /bin/echo as the minimal Mach-O input (same approach as
        // BundleAssemblyParityTests — a real signed Mach-O the
        // codesign call can re-sign).
        let machoSource = workDir.appendingPathComponent("crm-mac")
        try FileManager.default.copyItem(
            at: URL(fileURLWithPath: "/bin/echo"),
            to: machoSource)
        try FileManager.default.setAttributes(
            [.posixPermissions: 0o755],
            ofItemAtPath: machoSource.path)

        // Locate the source-tree Info.plist relative to this test file.
        let infoPlistURL = URL(fileURLWithPath: #filePath)
            .deletingLastPathComponent()  // CRMMacSystemTests/
            .deletingLastPathComponent()  // Tests/
            .deletingLastPathComponent()  // mac-daemon/
            .appendingPathComponent("Sources/crm-mac/Info.plist")
        let infoPlistBytes = try Data(contentsOf: infoPlistURL)

        let bundlePath = workDir.appendingPathComponent("crm-mac.app").path

        // Render the LaunchAgent plist with the real bundle path
        // embedded — same as the installer's renderLaunchAgentContent.
        // The bug we're regressing against was: render with a
        // placeholder, codesign, then rewrite post-codesign. If a
        // future change reintroduces that ordering, the
        // `codesign --verify` step below will fail because rewriting
        // a sealed resource invalidates the bundle's
        // Contents/_CodeSignature/CodeResources manifest.
        let launchAgentContent = LaunchAgentPlist(
            label: Daemon.label,
            binaryPath: "\(bundlePath)/Contents/MacOS/crm-mac",
            configDirPath: "\(workDir.path)/cfg",
            stdoutPath: "\(workDir.path)/logs/stdout.log",
            stderrPath: "\(workDir.path)/logs/stderr.log"
        ).render()

        let fs = ProductionFilesystemAdapter()
        let exec = ProductionExecutableAdapter()
        let assembler = BundleAssembler(filesystem: fs, executable: exec)
        try assembler.assemble(BundleAssemblerInput(
            machoSourcePath: machoSource.path,
            bundlePath: bundlePath,
            launchAgentPlistContent: launchAgentContent,
            infoPlistContent: infoPlistBytes,
            codesignIdentifier: Daemon.label))

        // The actual regression assertion: `codesign --verify` must
        // exit 0. A broken seal (the bug we're guarding against)
        // surfaces as exit 1 with "a sealed resource is missing or
        // invalid" pointing at the embedded LaunchAgent plist.
        let verify = runCodesignVerify(bundlePath: bundlePath)
        XCTAssertEqual(verify.exitCode, 0,
            "codesign --verify failed (exit=\(verify.exitCode)) — embedded LaunchAgent plist seal is broken. stderr: \(verify.stderr)")
    }

    // MARK: - helpers

    private struct CodesignResult {
        let exitCode: Int32
        let stderr: String
    }

    private func runCodesignVerify(bundlePath: String) -> CodesignResult {
        let proc = Process()
        proc.executableURL = URL(fileURLWithPath: "/usr/bin/codesign")
        proc.arguments = ["--verify", "--verbose", bundlePath]
        let errPipe = Pipe()
        proc.standardError = errPipe
        do {
            try proc.run()
        } catch {
            return CodesignResult(exitCode: -1, stderr: "spawn failed: \(error.localizedDescription)")
        }
        proc.waitUntilExit()
        let stderr = String(
            data: errPipe.fileHandleForReading.readDataToEndOfFile(),
            encoding: .utf8) ?? ""
        return CodesignResult(exitCode: proc.terminationStatus, stderr: stderr)
    }

    private func makeTempWorkDir() throws -> URL {
        let url = FileManager.default.temporaryDirectory
            .appendingPathComponent("crm-mac-seal-\(UUID().uuidString)")
        try FileManager.default.createDirectory(
            at: url, withIntermediateDirectories: true)
        return url
    }
}
