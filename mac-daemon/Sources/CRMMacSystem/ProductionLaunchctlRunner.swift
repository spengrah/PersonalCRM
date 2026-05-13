// ProductionLaunchctlRunner shells out to /bin/launchctl. The user-
// domain target ("gui/<uid>") is computed from `getuid()` so the
// daemon's plist can run under whichever account installed it.
//
// Uses classic `launchctl bootstrap gui/<uid> <plist>` /
// `launchctl bootout gui/<uid>/<label>` rather than SMAppService.agent
// (which requires an .app bundle; this binary is a bare CLI).
import Foundation
import CRMMacLifecycle

public struct ProductionLaunchctlRunner: LaunchctlRunner {
    public let launchctlPath: String
    public let userIDProvider: () -> UInt32

    public init(
        launchctlPath: String = "/bin/launchctl",
        userIDProvider: @escaping () -> UInt32 = { UInt32(getuid()) }
    ) {
        self.launchctlPath = launchctlPath
        self.userIDProvider = userIDProvider
    }

    public func bootstrap(plistPath: String) throws -> LaunchctlInvocation {
        try run(arguments: ["bootstrap", "gui/\(userIDProvider())", plistPath])
    }

    public func bootout(label: String) throws -> LaunchctlInvocation {
        try run(arguments: ["bootout", "gui/\(userIDProvider())/\(label)"])
    }

    public func printService(label: String) throws -> LaunchctlInvocation {
        try run(arguments: ["print", "gui/\(userIDProvider())/\(label)"])
    }

    private func run(arguments: [String]) throws -> LaunchctlInvocation {
        let proc = Process()
        proc.executableURL = URL(fileURLWithPath: launchctlPath)
        proc.arguments = arguments
        let outPipe = Pipe()
        let errPipe = Pipe()
        proc.standardOutput = outPipe
        proc.standardError = errPipe
        do {
            try proc.run()
        } catch {
            throw LaunchctlSystemError.spawnFailed(String(describing: error))
        }
        proc.waitUntilExit()
        let stdout = String(data: outPipe.fileHandleForReading.readDataToEndOfFile(), encoding: .utf8) ?? ""
        let stderr = String(data: errPipe.fileHandleForReading.readDataToEndOfFile(), encoding: .utf8) ?? ""
        return LaunchctlInvocation(
            arguments: arguments,
            exitCode: proc.terminationStatus,
            stdout: stdout,
            stderr: stderr)
    }
}

public enum LaunchctlSystemError: Error, CustomStringConvertible {
    case spawnFailed(String)
    public var description: String {
        switch self {
        case .spawnFailed(let m): return "launchctl spawn failed: \(m)"
        }
    }
}
