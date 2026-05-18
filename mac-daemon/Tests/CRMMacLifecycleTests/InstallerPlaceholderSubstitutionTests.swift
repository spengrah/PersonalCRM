// InstallerPlaceholderSubstitutionTests cover the
// `__INSTALL_PREFIX__` substitution step (plan D7 step 5, D14
// InstallerPlaceholderSubstitutionTests). The build-time shell
// script writes the placeholder; the install-time Swift code
// substitutes it with the real bundle app path before
// SMAppService.register reads the file.
//
// Substitution is exercised through the public Installer surface
// (fresh install runs assembly + atomic-rename + substitution +
// register end-to-end). Direct unit-testing of the private
// substituteInstallPrefixPlaceholder helper would require exposing
// it; the end-to-end assertion is sufficient.
import XCTest
@testable import CRMMacLifecycle
@testable import CRMMacPiClient

final class InstallerPlaceholderSubstitutionTests: XCTestCase {

    func testInstallerSubstitutesPlaceholderAfterAssembly() async throws {
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        fs.seedFile(at: "/tmp/source/crm-mac")
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
                PiClient(baseURL: url, transport: LifecycleMockTransport([.respond(status: 200, data: pair200JSON)]).asTransport(), sleep: noopSleep)
            },
            clock: FixedClock(),
            logger: NoopLogger()))
        _ = try await installer.run(InstallRequest(
            piURL: URL(string: "https://x")!,
            pairingToken: "tk",
            hostname: "mac-1"))

        // The substituted plist contains the real install-time bundle
        // path, NOT the placeholder.
        let plistData = try fs.read(from: paths.bundlePlistPath)
        let plistStr = String(data: plistData, encoding: .utf8) ?? ""
        XCTAssertFalse(plistStr.contains(Installer.installPrefixPlaceholder),
            "installer must substitute the placeholder")
        XCTAssertTrue(plistStr.contains(paths.bundleAppPath),
            "installer must substitute with the real bundle path")
        // PropertyListSerialization parses it.
        XCTAssertNoThrow(
            try PropertyListSerialization.propertyList(from: plistData, options: [], format: nil),
            "substituted plist must still parse as a valid plist")
    }

    func testSubstitutionXMLEscapesUnusualPaths() async throws {
        // Per Codex r6 #5: substituting a raw bundle path into
        // already-rendered XML breaks `LaunchAgentPlist`'s
        // XML-escape guarantee for unusual home directory characters
        // (`&`, `<`, `>`, `"`, `'`). The installer must apply the
        // same escape function during substitution so the resulting
        // plist still parses.
        let paths = LifecyclePaths(
            configDirPath: "/tmp/o&malley/cfg",
            binDirPath: "/tmp/o&malley/cfg/bin",
            configFilePath: "/tmp/o&malley/cfg/config.json",
            stateFilePath: "/tmp/o&malley/cfg/state.json",
            launchAgentsDirPath: "/tmp/o&malley/LaunchAgents",
            logsDirPath: "/tmp/o&malley/logs",
            stdoutLogPath: "/tmp/o&malley/logs/stdout.log",
            stderrLogPath: "/tmp/o&malley/logs/stderr.log",
            // The bundle app path contains `&` — XML-significant.
            bundleAppPath: "/tmp/o&malley/cfg/crm-mac.app",
            bundleBinaryPath: "/tmp/o&malley/cfg/crm-mac.app/Contents/MacOS/crm-mac",
            bundlePlistPath: "/tmp/o&malley/cfg/crm-mac.app/Contents/Library/LaunchAgents/\(Daemon.label).plist",
            bundleInfoPlistPath: "/tmp/o&malley/cfg/crm-mac.app/Contents/Info.plist",
            legacyBinaryPath: "/tmp/o&malley/cfg/bin/crm-mac",
            legacyPlistPath: "/tmp/o&malley/LaunchAgents/\(Daemon.label).plist")
        let fs = InMemoryFilesystem()
        fs.seedFile(at: "/tmp/source/crm-mac")
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
                PiClient(baseURL: url, transport: LifecycleMockTransport([.respond(status: 200, data: pair200JSON)]).asTransport(), sleep: noopSleep)
            },
            clock: FixedClock(),
            logger: NoopLogger()))
        _ = try await installer.run(InstallRequest(
            piURL: URL(string: "https://x")!,
            pairingToken: "tk",
            hostname: "mac-1"))
        // The embedded plist must parse — proves the substitution
        // didn't break the XML.
        let plistData = try fs.read(from: paths.bundlePlistPath)
        XCTAssertNoThrow(
            try PropertyListSerialization.propertyList(from: plistData, options: [], format: nil),
            "substitution must XML-escape the bundle path")
        // The decoded ProgramArguments[0] must round-trip back to the
        // raw bundle binary path (the escape is invisible to the
        // plist parser).
        let parsed = try PropertyListSerialization.propertyList(
            from: plistData, options: [], format: nil) as! [String: Any]
        let args = parsed["ProgramArguments"] as! [String]
        XCTAssertEqual(args.first, paths.bundleBinaryPath,
            "decoded ProgramArguments[0] must equal the raw bundleBinaryPath after XML-decode")
    }

    func testSubstitutionIdempotentViaRegisterOnly() async throws {
        // Set up an already-installed bundle whose embedded plist
        // has the placeholder already replaced. Run register-only
        // (which calls the substitute helper) — a second run on the
        // same install must be a no-op (no placeholder to substitute).
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        fs.seedFile(at: "/tmp/source/crm-mac")
        // Existing bundle with the placeholder already substituted.
        let alreadySubstituted = """
        <?xml version="1.0" encoding="UTF-8"?>
        <plist version="1.0"><dict>
        <key>ProgramArguments</key>
        <array><string>\(paths.bundleAppPath)/Contents/MacOS/crm-mac</string></array>
        </dict></plist>
        """
        try fs.createDirectory(at: paths.bundleAppPath)
        try fs.write(Data("bin".utf8), to: paths.bundleBinaryPath)
        try fs.write(Data(alreadySubstituted.utf8), to: paths.bundlePlistPath)
        let cfg = DaemonConfig(
            piURL: URL(string: "https://x")!,
            hostID: UUID(),
            hostname: "mac-1",
            installedAt: Date())
        let encoder = JSONEncoder()
        encoder.dateEncodingStrategy = .iso8601
        try fs.write(try encoder.encode(cfg), to: paths.configFilePath)
        let keychain = InMemoryKeychainStore(initial: "k")
        let exec = FakeExecutableAdapter(currentExecutablePath: "/tmp/source/crm-mac")
        let installer = Installer(InstallerDependencies(
            paths: paths,
            filesystem: fs,
            executable: exec,
            keychain: keychain,
            agentService: FakeAgentService(),
            processSignaller: FakeProcessSignaller(),
            bundleAssembler: BundleAssembler(filesystem: fs, executable: exec),
            piClientFactory: { url in
                PiClient(baseURL: url, transport: LifecycleMockTransport([]).asTransport(), sleep: noopSleep)
            },
            clock: FixedClock(),
            logger: NoopLogger()))
        _ = try await installer.run(InstallRequest(
            piURL: URL(string: "https://x")!,
            pairingToken: "ignored",
            hostname: "ignored",
            registerOnly: true))
        // File unchanged — idempotent.
        let post = try fs.read(from: paths.bundlePlistPath)
        XCTAssertEqual(post, Data(alreadySubstituted.utf8))
    }
}
