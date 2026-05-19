// InstallerLaunchAgentEmbeddingTests prove the embedded LaunchAgent
// plist references the real install-time bundle path AT the moment
// the bundle is codesigned — not after. The plist is a sealed
// resource under the bundle codesign manifest; modifying it after
// `codesignBundle` runs invalidates the seal and SMAppService
// rejects the bundle on subsequent register() calls
// ("Codesigning failure loading plist ... code: -67054").
//
// The earlier implementation rendered the plist with an
// __INSTALL_PREFIX__ placeholder, codesigned the bundle, then
// rewrote the plist post-codesign. The end-to-end install appeared
// to succeed (SMAppService accepts the initial submit without
// strict codesign validation) but `crm-mac install --register-only`
// and `crm-mac doctor` failed downstream. These tests pin the
// invariant that codesign-time plist content == final plist
// content.
import XCTest
import CRMMacCore
@testable import CRMMacLifecycle
@testable import CRMMacPiClient

final class InstallerLaunchAgentEmbeddingTests: XCTestCase {

    func testPlistEmbedsRealBundlePathAtCodesignTime() async throws {
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        fs.seedFile(at: "/tmp/source/crm-mac")
        // Snapshot the embedded plist content when the bundle is
        // codesigned. If the installer modifies the plist after
        // codesign returns, the snapshot will diverge from the final
        // on-disk plist — and we'll catch it.
        let exec = SnapshottingFakeExecutableAdapter(
            currentExecutablePath: "/tmp/source/crm-mac",
            filesystem: fs)
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

        XCTAssertEqual(exec.bundleCodesignCalls.count, 1)
        guard let snapshot = exec.plistAtCodesignTime else {
            return XCTFail("codesign snapshot was not captured — bundle codesign step did not run")
        }
        let snapshotStr = String(data: snapshot, encoding: .utf8) ?? ""
        // Codesign-time plist must already contain the real bundle
        // path — NOT the placeholder. Any future regression that
        // reintroduces post-codesign substitution will fail here.
        XCTAssertFalse(snapshotStr.contains("__INSTALL_PREFIX__"),
            "embedded plist must contain the real bundle path at codesign time")
        XCTAssertTrue(snapshotStr.contains(paths.bundleAppPath),
            "embedded plist must reference the install path at codesign time")
        XCTAssertTrue(snapshotStr.contains("\(paths.bundleAppPath)/Contents/MacOS/crm-mac"),
            "embedded plist's ProgramArguments[0] must point at the install-time binary")

        // Final on-disk plist (post-rename) must be byte-identical to
        // the codesign-time snapshot — proves no mutation between
        // codesign and the end of install.
        let finalPlist = try fs.read(from: paths.bundlePlistPath)
        XCTAssertEqual(snapshot, finalPlist,
            "plist content must NOT change between codesign and final install — would invalidate the codesign seal")

        // Plist must still parse as a valid plist.
        XCTAssertNoThrow(
            try PropertyListSerialization.propertyList(from: finalPlist, options: [], format: nil),
            "embedded plist must be a valid plist")
    }

    func testPlistXMLEscapesUnusualPaths() async throws {
        // Embedding a raw bundle path into the plist must XML-escape
        // characters like `&`, `<`, `>`, `"`, `'`. The
        // LaunchAgentPlist renderer handles this — this test pins
        // that the installer goes through the renderer (not a raw
        // string-concat) for unusual home directory paths.
        let paths = LifecyclePaths(
            configDirPath: "/tmp/o&malley/cfg",
            binDirPath: "/tmp/o&malley/cfg/bin",
            configFilePath: "/tmp/o&malley/cfg/config.json",
            stateFilePath: "/tmp/o&malley/cfg/state.json",
            launchAgentsDirPath: "/tmp/o&malley/LaunchAgents",
            logsDirPath: "/tmp/o&malley/logs",
            stdoutLogPath: "/tmp/o&malley/logs/stdout.log",
            stderrLogPath: "/tmp/o&malley/logs/stderr.log",
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
        let plistData = try fs.read(from: paths.bundlePlistPath)
        XCTAssertNoThrow(
            try PropertyListSerialization.propertyList(from: plistData, options: [], format: nil),
            "embedded plist must XML-escape the bundle path")
        // ProgramArguments[0] must round-trip back to the raw bundle
        // binary path (the escape is invisible to the plist parser).
        let parsed = try PropertyListSerialization.propertyList(
            from: plistData, options: [], format: nil) as! [String: Any]
        let args = parsed["ProgramArguments"] as! [String]
        XCTAssertEqual(args.first, paths.bundleBinaryPath,
            "decoded ProgramArguments[0] must equal the raw bundleBinaryPath after XML-decode")
    }
}

/// Test double that snapshots the embedded LaunchAgent plist at the
/// instant `codesignBundle` is called. Lets a test assert what
/// the codesign pass actually sealed, separately from what the final
/// on-disk plist contains.
final class SnapshottingFakeExecutableAdapter: ExecutableAdapter, @unchecked Sendable {
    private let inner: FakeExecutableAdapter
    private let filesystem: InMemoryFilesystem
    private(set) var plistAtCodesignTime: Data?

    init(currentExecutablePath: String, filesystem: InMemoryFilesystem) {
        self.inner = FakeExecutableAdapter(currentExecutablePath: currentExecutablePath)
        self.filesystem = filesystem
    }

    var bundleCodesignCalls: [FakeExecutableAdapter.BundleCodesignCall] {
        inner.bundleCodesignCalls
    }

    func currentExecutablePath() throws -> String {
        try inner.currentExecutablePath()
    }

    func adhocCodesign(path: String) throws {
        try inner.adhocCodesign(path: path)
    }

    func codesignBundle(bundlePath: String, identifier: String) throws {
        let plistPath = "\(bundlePath)/\(BundleAssembler.launchAgentPlistRelativePath)"
        plistAtCodesignTime = try? filesystem.read(from: plistPath)
        try inner.codesignBundle(bundlePath: bundlePath, identifier: identifier)
    }
}
