// BundleAssemblyParityTests proves the shell-script bundle-assembly
// (`mac-daemon/Scripts/assemble_bundle.sh`) and the Swift
// `BundleAssembler` produce byte-identical bundles when given the
// SAME source-tree inputs.
//
// Opt-in via the `CRM_MAC_INTEGRATION_TESTS=1` env var. The test
// shells out to /bin/bash + writes to a tmp work dir + invokes
// /usr/bin/codesign — it's not safe to run in arbitrary CI matrices.
// CI sets the env var explicitly in a dedicated step.
import XCTest
import CryptoKit
import CRMMacLifecycle
@testable import CRMMacSystem

final class BundleAssemblyParityTests: XCTestCase {

    override func setUpWithError() throws {
        try XCTSkipUnless(
            ProcessInfo.processInfo.environment["CRM_MAC_INTEGRATION_TESTS"] == "1",
            "BundleAssemblyParityTests requires CRM_MAC_INTEGRATION_TESTS=1; skipping under default test run.")
    }

    func testShellAndSwiftBundlesAreByteIdentical() throws {
        let workDir = try makeTempWorkDir()
        defer { try? FileManager.default.removeItem(at: workDir) }

        // Use /bin/echo as the minimal Mach-O input — it's a real
        // signed Mach-O in /bin, small, present on every macOS host.
        // The actual contents don't matter for byte-identity; we just
        // need a valid Mach-O the codesign call can sign.
        let machoSource = workDir.appendingPathComponent("crm-mac")
        try FileManager.default.copyItem(
            at: URL(fileURLWithPath: "/bin/echo"),
            to: machoSource)
        try FileManager.default.setAttributes(
            [.posixPermissions: 0o755],
            ofItemAtPath: machoSource.path)

        // Locate the source-tree Info.plist relative to this test
        // file. Same #filePath descent used by InfoPlistFixtureTests.
        let infoPlistURL = URL(fileURLWithPath: #filePath)
            .deletingLastPathComponent()  // CRMMacSystemTests/
            .deletingLastPathComponent()  // Tests/
            .deletingLastPathComponent()  // mac-daemon/
            .appendingPathComponent("Sources/crm-mac/Info.plist")
        let scriptURL = URL(fileURLWithPath: #filePath)
            .deletingLastPathComponent()
            .deletingLastPathComponent()
            .deletingLastPathComponent()
            .appendingPathComponent("Scripts/assemble_bundle.sh")

        let shellBundle = workDir.appendingPathComponent("shell-bundle.app")
        let swiftBundle = workDir.appendingPathComponent("swift-bundle.app")

        // Render the LaunchAgent plist exactly as the shell script
        // would: the shell uses the build-time $HOME for logs +
        // config-dir paths. We pass the SAME values to the Swift
        // implementation via `launchAgentPlistContent`.
        let buildHome = ProcessInfo.processInfo.environment["HOME"]
            ?? "/Users/runner"
        let launchAgentContent = Self.renderShellEquivalentLaunchAgent(
            buildHome: buildHome)

        // Invoke the shell script.
        try runShellAssemble(
            scriptURL: scriptURL,
            machoSource: machoSource,
            bundlePath: shellBundle,
            infoPlistSource: infoPlistURL)

        // Invoke the Swift BundleAssembler. Use the SAME
        // infoPlistContent (raw bytes of the source-tree file) as the
        // shell-script reads.
        let infoPlistBytes = try Data(contentsOf: infoPlistURL)
        let fs = ProductionFilesystemAdapter()
        let exec = ProductionExecutableAdapter()
        let assembler = BundleAssembler(filesystem: fs, executable: exec)
        try assembler.assemble(BundleAssemblerInput(
            machoSourcePath: machoSource.path,
            bundlePath: swiftBundle.path,
            launchAgentPlistContent: launchAgentContent,
            infoPlistContent: infoPlistBytes,
            codesignIdentifier: Daemon.label))

        // Walk both trees; assert file-list parity. The codesign
        // signature blobs in `Contents/_CodeSignature/CodeResources`
        // contain a build-time timestamp that intentionally drifts
        // between calls — we exclude that path from the byte-identity
        // assertion. The presence/absence of the file IS asserted
        // (both must produce it).
        let shellPaths = try enumerateRelativePaths(under: shellBundle)
        let swiftPaths = try enumerateRelativePaths(under: swiftBundle)
        XCTAssertEqual(
            shellPaths.sorted(),
            swiftPaths.sorted(),
            "shell and swift produced different file lists")

        // For every non-_CodeSignature file: assert SHA-256 matches.
        for rel in shellPaths.sorted() where !rel.hasPrefix("Contents/_CodeSignature/") {
            let shellSha = try sha256(of: shellBundle.appendingPathComponent(rel))
            let swiftSha = try sha256(of: swiftBundle.appendingPathComponent(rel))
            XCTAssertEqual(
                shellSha, swiftSha,
                "byte mismatch at \(rel): shell=\(shellSha) swift=\(swiftSha)")
        }
    }

    // MARK: - helpers

    private static func renderShellEquivalentLaunchAgent(buildHome: String) -> String {
        // Mirrors the heredoc in Scripts/assemble_bundle.sh exactly.
        // Any divergence between this string and the shell script's
        // output is what we're hunting — the SHA-256 check below
        // surfaces it.
        return """
        <?xml version="1.0" encoding="UTF-8"?>
        <!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
        <plist version="1.0">
        <dict>
            <key>Label</key>
            <string>\(Daemon.label)</string>
            <key>ProgramArguments</key>
            <array>
                <string>__INSTALL_PREFIX__/Contents/MacOS/crm-mac</string>
                <string>daemon</string>
            </array>
            <key>RunAtLoad</key>
            <true/>
            <key>KeepAlive</key>
            <dict>
                <key>Crashed</key>
                <true/>
            </dict>
            <key>ProcessType</key>
            <string>Background</string>
            <key>StandardOutPath</key>
            <string>\(buildHome)/Library/Logs/crm-mac/stdout.log</string>
            <key>StandardErrorPath</key>
            <string>\(buildHome)/Library/Logs/crm-mac/stderr.log</string>
            <key>EnvironmentVariables</key>
            <dict>
                <key>CRM_MAC_CONFIG_DIR</key>
                <string>\(buildHome)/Library/Application Support/crm-mac</string>
            </dict>
        </dict>
        </plist>

        """
    }

    private func makeTempWorkDir() throws -> URL {
        let url = FileManager.default.temporaryDirectory
            .appendingPathComponent("crm-mac-parity-\(UUID().uuidString)")
        try FileManager.default.createDirectory(
            at: url, withIntermediateDirectories: true)
        return url
    }

    private func runShellAssemble(
        scriptURL: URL,
        machoSource: URL,
        bundlePath: URL,
        infoPlistSource: URL
    ) throws {
        let proc = Process()
        proc.executableURL = URL(fileURLWithPath: "/bin/bash")
        proc.arguments = [
            scriptURL.path,
            machoSource.path,
            bundlePath.path,
            infoPlistSource.path,
        ]
        let outPipe = Pipe(), errPipe = Pipe()
        proc.standardOutput = outPipe
        proc.standardError = errPipe
        try proc.run()
        proc.waitUntilExit()
        if proc.terminationStatus != 0 {
            let err = String(data: errPipe.fileHandleForReading.readDataToEndOfFile(),
                             encoding: .utf8) ?? ""
            XCTFail("assemble_bundle.sh exit \(proc.terminationStatus): \(err)")
        }
    }

    private func enumerateRelativePaths(under root: URL) throws -> [String] {
        guard let enumerator = FileManager.default.enumerator(
            at: root,
            includingPropertiesForKeys: [.isDirectoryKey],
            options: []) else {
            return []
        }
        var out: [String] = []
        let prefix = root.path + "/"
        for case let url as URL in enumerator {
            // Skip directories — we compare files by SHA-256 below;
            // directory existence is implied by file presence.
            let resourceValues = try url.resourceValues(forKeys: [.isDirectoryKey])
            if resourceValues.isDirectory == true { continue }
            let p = url.path
            guard p.hasPrefix(prefix) else { continue }
            out.append(String(p.dropFirst(prefix.count)))
        }
        return out
    }

    private func sha256(of url: URL) throws -> String {
        let data = try Data(contentsOf: url)
        let digest = SHA256.hash(data: data)
        return digest.map { String(format: "%02x", $0) }.joined()
    }
}
