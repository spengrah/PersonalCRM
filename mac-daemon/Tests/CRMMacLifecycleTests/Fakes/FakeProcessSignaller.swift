import Foundation
@testable import CRMMacLifecycle

/// FakeProcessSignaller records sigterm + wait calls and returns a
/// scripted "released" result. Default: every wait returns true
/// (daemon stopped cleanly). Tests set `nextPidfileReleaseResult =
/// false` to model the daemon-refuses-to-stop branch.
public final class FakeProcessSignaller: ProcessSignaller, @unchecked Sendable {
    public var nextPidfileReleaseResult: Bool = true
    /// If non-nil, `sendSIGTERM` throws this on the next invocation.
    public var sendSIGTERMThrowsOnce: ProcessSignallerError?

    public private(set) var sigtermCalls: [pid_t] = []
    public private(set) var waitForPidfileReleaseCalls: [String] = []

    public init() {}

    public func sendSIGTERM(pid: pid_t) throws {
        sigtermCalls.append(pid)
        if let err = sendSIGTERMThrowsOnce {
            sendSIGTERMThrowsOnce = nil
            throw err
        }
    }

    public func waitForPidfileRelease(
        path: String,
        timeoutSeconds: TimeInterval
    ) async -> Bool {
        waitForPidfileReleaseCalls.append(path)
        return nextPidfileReleaseResult
    }
}
