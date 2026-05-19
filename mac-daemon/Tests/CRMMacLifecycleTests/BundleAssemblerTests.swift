// BundleAssemblerTests exercise the Swift install-time bundle
// assembly logic. The shell-script equivalent
// (`Scripts/assemble_bundle.sh`) is exercised by
// `BundleAssemblyParityTests` in CRMMacSystemTests (real Foundation
// adapter, env-gated). These tests use the InMemoryFilesystem fake
// + FakeExecutableAdapter so they run in any CI environment.
import XCTest
@testable import CRMMacLifecycle

final class BundleAssemblerTests: XCTestCase {

    private static let fixtureInfoPlistBytes: Data = {
        let xml = """
        <?xml version="1.0" encoding="UTF-8"?>
        <!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
        <plist version="1.0">
        <dict>
            <key>CFBundleIdentifier</key>
            <string>xyz.spengrah.crm-mac</string>
        </dict>
        </plist>
        """
        return Data(xml.utf8)
    }()

    private static let fixtureLaunchAgentContent: String = """
        <?xml version="1.0" encoding="UTF-8"?>
        <plist version="1.0">
        <dict>
            <key>Label</key>
            <string>xyz.spengrah.crm-mac</string>
            <key>ProgramArguments</key>
            <array>
                <string>/tmp/install/crm-mac.app/Contents/MacOS/crm-mac</string>
                <string>daemon</string>
            </array>
        </dict>
        </plist>
        """

    private func makeInput(bundlePath: String) -> BundleAssemblerInput {
        BundleAssemblerInput(
            machoSourcePath: "/tmp/source/crm-mac",
            bundlePath: bundlePath,
            launchAgentPlistContent: Self.fixtureLaunchAgentContent,
            infoPlistContent: Self.fixtureInfoPlistBytes,
            codesignIdentifier: Daemon.label)
    }

    func testAssembleProducesCompleteBundleStructure() throws {
        let fs = InMemoryFilesystem()
        fs.seedFile(at: "/tmp/source/crm-mac")
        let exec = FakeExecutableAdapter()
        let assembler = BundleAssembler(filesystem: fs, executable: exec)
        let bundlePath = "/tmp/install/crm-mac.app"
        try assembler.assemble(makeInput(bundlePath: bundlePath))

        XCTAssertTrue(fs.fileExists(at: bundlePath))
        XCTAssertTrue(fs.fileExists(at: "\(bundlePath)/Contents"))
        XCTAssertTrue(fs.fileExists(at: "\(bundlePath)/Contents/MacOS"))
        XCTAssertTrue(fs.fileExists(at: "\(bundlePath)/Contents/Library/LaunchAgents"))
        XCTAssertTrue(fs.fileExists(at: "\(bundlePath)/Contents/MacOS/crm-mac"))
        XCTAssertTrue(fs.fileExists(at: "\(bundlePath)/Contents/Info.plist"))
        XCTAssertTrue(fs.fileExists(at: "\(bundlePath)/Contents/Library/LaunchAgents/\(Daemon.label).plist"))
    }

    func testMachoIsExecutableAfterAssemble() throws {
        let fs = InMemoryFilesystem()
        fs.seedFile(at: "/tmp/source/crm-mac")
        let exec = FakeExecutableAdapter()
        let assembler = BundleAssembler(filesystem: fs, executable: exec)
        let bundlePath = "/tmp/install/crm-mac.app"
        try assembler.assemble(makeInput(bundlePath: bundlePath))
        let machoDest = "\(bundlePath)/Contents/MacOS/crm-mac"
        XCTAssertTrue(fs.madeExecutable.contains(machoDest))
    }

    func testCodesignInvokedOnceWithBundleAndIdentifier() throws {
        let fs = InMemoryFilesystem()
        fs.seedFile(at: "/tmp/source/crm-mac")
        let exec = FakeExecutableAdapter()
        let assembler = BundleAssembler(filesystem: fs, executable: exec)
        let bundlePath = "/tmp/install/crm-mac.app"
        try assembler.assemble(makeInput(bundlePath: bundlePath))
        XCTAssertEqual(exec.bundleCodesignCalls.count, 1)
        XCTAssertEqual(exec.bundleCodesignCalls.first?.bundlePath, bundlePath)
        XCTAssertEqual(exec.bundleCodesignCalls.first?.identifier, Daemon.label)
        // The single-Mach-O path is NOT used by BundleAssembler.
        XCTAssertEqual(exec.codesignCalls.count, 0)
    }

    func testInfoPlistContentMatchesInput() throws {
        let fs = InMemoryFilesystem()
        fs.seedFile(at: "/tmp/source/crm-mac")
        let exec = FakeExecutableAdapter()
        let assembler = BundleAssembler(filesystem: fs, executable: exec)
        let bundlePath = "/tmp/install/crm-mac.app"
        try assembler.assemble(makeInput(bundlePath: bundlePath))
        let written = try fs.read(from: "\(bundlePath)/Contents/Info.plist")
        XCTAssertEqual(written, Self.fixtureInfoPlistBytes)
    }

    func testLaunchAgentPlistContentMatchesInput() throws {
        let fs = InMemoryFilesystem()
        fs.seedFile(at: "/tmp/source/crm-mac")
        let exec = FakeExecutableAdapter()
        let assembler = BundleAssembler(filesystem: fs, executable: exec)
        let bundlePath = "/tmp/install/crm-mac.app"
        try assembler.assemble(makeInput(bundlePath: bundlePath))
        let written = try fs.read(
            from: "\(bundlePath)/Contents/Library/LaunchAgents/\(Daemon.label).plist")
        XCTAssertEqual(written, Data(Self.fixtureLaunchAgentContent.utf8))
    }

    func testCodesignFailurePropagates() throws {
        let fs = InMemoryFilesystem()
        fs.seedFile(at: "/tmp/source/crm-mac")
        let exec = FakeExecutableAdapter()
        exec.failBundleCodesignWith = "injected codesign failure"
        let assembler = BundleAssembler(filesystem: fs, executable: exec)
        let bundlePath = "/tmp/install/crm-mac.app"
        XCTAssertThrowsError(try assembler.assemble(makeInput(bundlePath: bundlePath))) { error in
            guard let e = error as? ExecutableAdapterError else {
                XCTFail("expected ExecutableAdapterError, got \(error)")
                return
            }
            if case .codesignFailed(let m) = e {
                XCTAssertTrue(m.contains("injected codesign failure"))
            } else {
                XCTFail("expected codesignFailed, got \(e)")
            }
        }
    }
}
