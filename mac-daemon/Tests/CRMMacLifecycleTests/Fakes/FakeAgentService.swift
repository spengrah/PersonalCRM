import Foundation
@testable import CRMMacLifecycle

/// FakeAgentService records calls + returns scripted outcomes/errors.
/// Default behavior models a clean system: not registered; register()
/// succeeds and returns `.registered`; currentStatus() returns
/// `.notRegistered`. Tests override fields on `script` to model
/// already-registered, requires-approval, etc.
public final class FakeAgentService: AgentService, @unchecked Sendable {
    public struct Script {
        /// Sequence of statuses returned by `currentStatus()`. The
        /// fake pops the head; if the array empties, the last value
        /// repeats. Default: `[.notRegistered]`.
        public var statusSequence: [AgentServiceStatus] = [.notRegistered]
        /// Outcome value returned from `register()` on success.
        /// Default `.registered`; tests set `.alreadyRegistered` to
        /// model re-register or `--register-only` on an already-good
        /// install.
        public var nextRegisterOutcome: AgentRegisterOutcome = .registered
        /// If non-nil, `register()` throws this error instead of
        /// returning the outcome.
        public var registerThrows: AgentServiceError?
        /// If non-nil, `unregister()` throws this error.
        public var unregisterThrows: AgentServiceError?
        public init() {}
    }

    public var script: Script
    public private(set) var registerCalls: Int = 0
    /// Of the register calls, how many returned `.alreadyRegistered`.
    /// Drives StartCommand's "already registered" message tests.
    public private(set) var registerAlreadyRegisteredCount: Int = 0
    public private(set) var unregisterCalls: Int = 0
    public private(set) var statusCalls: Int = 0

    public init(script: Script = Script()) {
        self.script = script
    }

    public func register() throws -> AgentRegisterOutcome {
        registerCalls += 1
        if let err = script.registerThrows {
            throw err
        }
        let outcome = script.nextRegisterOutcome
        if outcome == .alreadyRegistered {
            registerAlreadyRegisteredCount += 1
        }
        return outcome
    }

    public func unregister() async throws {
        unregisterCalls += 1
        if let err = script.unregisterThrows {
            throw err
        }
    }

    public func currentStatus() -> AgentServiceStatus {
        statusCalls += 1
        if script.statusSequence.count > 1 {
            return script.statusSequence.removeFirst()
        }
        return script.statusSequence.first ?? .notRegistered
    }
}
