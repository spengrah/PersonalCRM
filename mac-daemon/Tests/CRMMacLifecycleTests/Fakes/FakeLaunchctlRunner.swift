import Foundation
@testable import CRMMacLifecycle

/// Records every launchctl invocation; returns a scripted exit code
/// per invocation. Defaults model a clean system where no service is
/// registered: bootstrap/bootout succeed (exit 0), and printService
/// reports "service unknown" (exit 1) so the Installer preflight does
/// not see an existing registration. Tests that need a registered
/// service set `script.printService = [0]` explicitly.
public final class FakeLaunchctlRunner: LaunchctlRunner, @unchecked Sendable {
    public struct Script {
        public var bootstrap: [Int32] = [0]
        public var bootout: [Int32] = [0]
        public var printService: [Int32] = [1]
        public init() {}
    }

    public var script: Script
    public private(set) var bootstrapCalls: [String] = []
    public private(set) var bootoutCalls: [String] = []
    public private(set) var printServiceCalls: [String] = []
    /// If non-nil, `printService(label:)` throws this error on the
    /// next invocation. Used to exercise the launchctl-spawn-failure
    /// branch of Installer.existingInstallDetected.
    public var printServiceThrowsOnce: Error?

    public init(script: Script = Script()) {
        self.script = script
    }

    public func bootstrap(plistPath: String) throws -> LaunchctlInvocation {
        bootstrapCalls.append(plistPath)
        let exit = script.bootstrap.isEmpty ? 0 : script.bootstrap.removeFirst()
        return LaunchctlInvocation(
            arguments: ["bootstrap", "gui/501", plistPath],
            exitCode: exit)
    }

    public func bootout(label: String) throws -> LaunchctlInvocation {
        bootoutCalls.append(label)
        let exit = script.bootout.isEmpty ? 0 : script.bootout.removeFirst()
        return LaunchctlInvocation(
            arguments: ["bootout", "gui/501/\(label)"],
            exitCode: exit)
    }

    public func printService(label: String) throws -> LaunchctlInvocation {
        printServiceCalls.append(label)
        if let err = printServiceThrowsOnce {
            printServiceThrowsOnce = nil
            throw err
        }
        // Default to exit 1 ("service unknown") when the script is
        // exhausted so multi-call tests do not silently flip to
        // "registered" on the second probe.
        let exit = script.printService.isEmpty ? 1 : script.printService.removeFirst()
        return LaunchctlInvocation(
            arguments: ["print", "gui/501/\(label)"],
            exitCode: exit)
    }
}
