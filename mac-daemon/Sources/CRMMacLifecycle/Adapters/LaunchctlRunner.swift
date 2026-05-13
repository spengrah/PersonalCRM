// LaunchctlRunner abstracts the `launchctl bootstrap`/`bootout`/`print`
// shell-outs the installer + uninstaller + doctor + status commands
// use. Production impl in CRMMacSystem shells out via Process; the
// fake records invocations and returns scripted results.
import Foundation

public struct LaunchctlInvocation: Equatable {
    public let arguments: [String]
    public let exitCode: Int32
    public let stdout: String
    public let stderr: String

    public init(arguments: [String], exitCode: Int32, stdout: String = "", stderr: String = "") {
        self.arguments = arguments
        self.exitCode = exitCode
        self.stdout = stdout
        self.stderr = stderr
    }
}

public protocol LaunchctlRunner {
    /// Run `launchctl bootstrap gui/<uid> <plistPath>`. Returns the
    /// invocation result; non-zero exit codes are NOT thrown — the
    /// caller decides whether to surface them based on idempotence
    /// expectations.
    func bootstrap(plistPath: String) throws -> LaunchctlInvocation
    /// Run `launchctl bootout gui/<uid>/<label>`.
    func bootout(label: String) throws -> LaunchctlInvocation
    /// Run `launchctl print gui/<uid>/<label>`. Exit code 0 means the
    /// service is known; non-zero means unregistered.
    func printService(label: String) throws -> LaunchctlInvocation
}
