import Foundation
@testable import CRMMacLifecycle

/// Records every launchctl invocation; returns a scripted exit code
/// per invocation, or a default of 0 when the script is empty.
public final class FakeLaunchctlRunner: LaunchctlRunner {
    public struct Script {
        public var bootstrap: [Int32] = [0]
        public var bootout: [Int32] = [0]
        public var printService: [Int32] = [0]
        public init() {}
    }

    public var script: Script
    public private(set) var bootstrapCalls: [String] = []
    public private(set) var bootoutCalls: [String] = []
    public private(set) var printServiceCalls: [String] = []

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
        let exit = script.printService.isEmpty ? 0 : script.printService.removeFirst()
        return LaunchctlInvocation(
            arguments: ["print", "gui/501/\(label)"],
            exitCode: exit)
    }
}
