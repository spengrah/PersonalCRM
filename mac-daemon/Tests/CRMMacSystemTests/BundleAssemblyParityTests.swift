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
        // Force ad-hoc signing regardless of the developer's shell env so the
        // parity assertion is deterministic — otherwise a developer with
        // `CRM_MAC_CODESIGN_IDENTITY` exported runs a different path than CI.
        let exec = ProductionExecutableAdapter(signingIdentity: nil)
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

    // MARK: - CRMBuildSHA stamp coverage

    // These two methods live INSIDE BundleAssemblyParityTests on purpose:
    // CI's mac-daemon integration step runs
    // `swift test --filter 'BundleAssemblyParityTests|BundleSwapAtomicityTests'`,
    // so a separate test class would silently not run. Keeping them here
    // means CI picks them up with no ci.yml filter change.

    /// (a) Build-path stamp: `assemble_bundle.sh` writes CRMBuildSHA into
    /// Contents/Info.plist when CRM_BUILD_SHA is set, and the key survives
    /// under the codesign seal (it is inserted before the codesign pass).
    func testShellAssembleStampsCRMBuildSHAUnderCodesignSeal() throws {
        let workDir = try makeTempWorkDir()
        defer { try? FileManager.default.removeItem(at: workDir) }

        let machoSource = workDir.appendingPathComponent("crm-mac")
        try FileManager.default.copyItem(
            at: URL(fileURLWithPath: "/bin/echo"),
            to: machoSource)
        try FileManager.default.setAttributes(
            [.posixPermissions: 0o755],
            ofItemAtPath: machoSource.path)

        let infoPlistURL = sourceInfoPlistURL()
        let scriptURL = assembleScriptURL()
        let bundle = workDir.appendingPathComponent("stamped-bundle.app")

        let fixtureSHA = "deadbeef00000000000000000000000000000000"
        try runShellAssemble(
            scriptURL: scriptURL,
            machoSource: machoSource,
            bundlePath: bundle,
            infoPlistSource: infoPlistURL,
            extraEnv: ["CRM_BUILD_SHA": fixtureSHA])

        // The stamped key is present and equals the fixture SHA.
        let infoPlist = bundle.appendingPathComponent("Contents/Info.plist")
        let extracted = try runCommand(
            "/usr/bin/plutil",
            ["-extract", "CRMBuildSHA", "raw", infoPlist.path])
            .trimmingCharacters(in: .whitespacesAndNewlines)
        XCTAssertEqual(
            extracted, fixtureSHA,
            "assemble_bundle.sh did not stamp CRMBuildSHA into Contents/Info.plist")

        // The key is under the codesign seal: a strict/deep verify still
        // passes (the stamp was inserted before the codesign pass).
        _ = try runCommand(
            "/usr/bin/codesign",
            ["--verify", "--strict", "--deep", bundle.path])
    }

    /// (b) Install-path carry-through: the `loadInfoPlistContent()`
    /// transform `install --upgrade` uses (serialize Bundle.main's dict to
    /// XML, feed to BundleAssembler) is key-preserving — a CRMBuildSHA in
    /// the build-bundle dict survives into the assembled (installed) bundle
    /// with NO Swift change. A unit test cannot mutate Bundle.main, but it
    /// CAN drive the IDENTICAL PropertyListSerialization + BundleAssembler
    /// the production path uses.
    func testStampedInfoPlistSurvivesSerializeAndAssemble() throws {
        let workDir = try makeTempWorkDir()
        defer { try? FileManager.default.removeItem(at: workDir) }

        let machoSource = workDir.appendingPathComponent("crm-mac")
        try FileManager.default.copyItem(
            at: URL(fileURLWithPath: "/bin/echo"),
            to: machoSource)
        try FileManager.default.setAttributes(
            [.posixPermissions: 0o755],
            ofItemAtPath: machoSource.path)

        // 1. Parse the real source Info.plist into a dict and add
        //    CRMBuildSHA, mirroring a build-stamped bundle's dict as
        //    Bundle.main.infoDictionary would surface it.
        let fixtureSHA = "feedface00000000000000000000000000000000"
        let sourceBytes = try Data(contentsOf: sourceInfoPlistURL())
        guard var dict = try PropertyListSerialization.propertyList(
            from: sourceBytes, options: [], format: nil) as? [String: Any] else {
            XCTFail("source Info.plist did not parse into a [String: Any] dict")
            return
        }
        dict["CRMBuildSHA"] = fixtureSHA

        // 2. Re-serialize via the IDENTICAL call loadInfoPlistContent() makes.
        let serialized = try PropertyListSerialization.data(
            fromPropertyList: dict, format: .xml, options: 0)

        // 3. Feed those bytes to the real BundleAssembler (the same Swift
        //    assembler `install --upgrade` uses). Force ad-hoc signing for
        //    determinism, matching the byte-identity test above.
        let buildHome = ProcessInfo.processInfo.environment["HOME"]
            ?? "/Users/runner"
        let launchAgentContent = Self.renderShellEquivalentLaunchAgent(
            buildHome: buildHome)
        let assembledBundle = workDir.appendingPathComponent("assembled-bundle.app")
        let fs = ProductionFilesystemAdapter()
        let exec = ProductionExecutableAdapter(signingIdentity: nil)
        let assembler = BundleAssembler(filesystem: fs, executable: exec)
        try assembler.assemble(BundleAssemblerInput(
            machoSourcePath: machoSource.path,
            bundlePath: assembledBundle.path,
            launchAgentPlistContent: launchAgentContent,
            infoPlistContent: serialized,
            codesignIdentifier: Daemon.label))

        // 4. The assembled (installed-equivalent) bundle STILL carries the
        //    stamp — proving the serialize→assemble round-trip is
        //    key-preserving, which is what carries a build-time stamp into
        //    the installed bundle.
        let infoPlist = assembledBundle.appendingPathComponent("Contents/Info.plist")
        let extracted = try runCommand(
            "/usr/bin/plutil",
            ["-extract", "CRMBuildSHA", "raw", infoPlist.path])
            .trimmingCharacters(in: .whitespacesAndNewlines)
        XCTAssertEqual(
            extracted, fixtureSHA,
            "CRMBuildSHA did not survive the serialize→assemble round-trip")
    }

    // MARK: - helpers

    private func sourceInfoPlistURL() -> URL {
        URL(fileURLWithPath: #filePath)
            .deletingLastPathComponent()  // CRMMacSystemTests/
            .deletingLastPathComponent()  // Tests/
            .deletingLastPathComponent()  // mac-daemon/
            .appendingPathComponent("Sources/crm-mac/Info.plist")
    }

    private func assembleScriptURL() -> URL {
        URL(fileURLWithPath: #filePath)
            .deletingLastPathComponent()
            .deletingLastPathComponent()
            .deletingLastPathComponent()
            .appendingPathComponent("Scripts/assemble_bundle.sh")
    }

    /// Run an executable and return stdout, failing the test on non-zero exit.
    @discardableResult
    private func runCommand(_ launchPath: String, _ args: [String]) throws -> String {
        let proc = Process()
        proc.executableURL = URL(fileURLWithPath: launchPath)
        proc.arguments = args
        let outPipe = Pipe(), errPipe = Pipe()
        proc.standardOutput = outPipe
        proc.standardError = errPipe
        try proc.run()
        let outData = outPipe.fileHandleForReading.readDataToEndOfFile()
        let errData = errPipe.fileHandleForReading.readDataToEndOfFile()
        proc.waitUntilExit()
        let out = String(data: outData, encoding: .utf8) ?? ""
        if proc.terminationStatus != 0 {
            let err = String(data: errData, encoding: .utf8) ?? ""
            XCTFail("\(launchPath) \(args.joined(separator: " ")) exit \(proc.terminationStatus): \(err)")
        }
        return out
    }

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
        infoPlistSource: URL,
        extraEnv: [String: String] = [:]
    ) throws {
        let proc = Process()
        proc.executableURL = URL(fileURLWithPath: "/bin/bash")
        proc.arguments = [
            scriptURL.path,
            machoSource.path,
            bundlePath.path,
            infoPlistSource.path,
        ]
        // Strip CRM_MAC_CODESIGN_IDENTITY from the child env so the shell
        // script signs ad-hoc regardless of the developer's exported shell
        // env — matches the Swift adapter pinning above. Also strip
        // CRM_BUILD_SHA: assemble_bundle.sh stamps Contents/Info.plist when
        // it is set, but the Swift path is fed the raw source-plist bytes
        // (unstamped) — a developer with CRM_BUILD_SHA exported would
        // otherwise break the byte-identity assertion on Info.plist. Tests
        // that DO want a stamp pass it back in explicitly via `extraEnv`.
        var env = ProcessInfo.processInfo.environment
        env.removeValue(forKey: "CRM_MAC_CODESIGN_IDENTITY")
        env.removeValue(forKey: "CRM_BUILD_SHA")
        for (key, value) in extraEnv {
            env[key] = value
        }
        proc.environment = env
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
