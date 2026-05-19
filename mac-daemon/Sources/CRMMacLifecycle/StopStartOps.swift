// StopStartOps — the dependency-injected logic for the `crm-mac stop`
// and `crm-mac start` subcommands. Keeps the ArgumentParser
// shells thin: command parses flags, builds the dependency struct,
// delegates here.
import Foundation
import CRMMacCore

public struct StopOpsDependencies {
    public let paths: LifecyclePaths
    public let filesystem: FilesystemAdapter
    public let agentService: AgentService
    public let processSignaller: ProcessSignaller
    public let logger: LoggerProtocol

    public init(
        paths: LifecyclePaths,
        filesystem: FilesystemAdapter,
        agentService: AgentService,
        processSignaller: ProcessSignaller,
        logger: LoggerProtocol
    ) {
        self.paths = paths
        self.filesystem = filesystem
        self.agentService = agentService
        self.processSignaller = processSignaller
        self.logger = logger
    }
}

public struct StopOpsResult: Equatable {
    public let stopped: Bool
    public let pid: pid_t
    public let unregisterInvoked: Bool

    public init(stopped: Bool, pid: pid_t, unregisterInvoked: Bool) {
        self.stopped = stopped
        self.pid = pid
        self.unregisterInvoked = unregisterInvoked
    }
}

public enum StopOps {
    public static func run(
        _ deps: StopOpsDependencies,
        timeoutSeconds: TimeInterval
    ) async -> StopOpsResult {
        var unregisterInvoked = false
        do {
            try await deps.agentService.unregister()
            unregisterInvoked = true
        } catch {
            unregisterInvoked = true
            deps.logger.warning("stop: agentService.unregister failed (continuing)", metadata: [
                "error": .private("\(error)"),
            ])
        }

        // Decide whether a daemon process is running. A present pidfile
        // is the signal — we don't conclude "not running" just because
        // we couldn't parse the pid. If parsing fails we skip the
        // SIGTERM but still poll the flock (the canonical
        // "is the daemon alive" probe). If the file is absent the
        // daemon is definitely not running.
        var pid: pid_t = 0
        let pidfilePresent = deps.filesystem.fileExists(at: deps.paths.pidfilePath)
        if pidfilePresent {
            if let data = try? deps.filesystem.read(from: deps.paths.pidfilePath),
               let raw = String(data: data, encoding: .utf8),
               let parsed = pid_t(raw.trimmingCharacters(in: .whitespacesAndNewlines)),
               parsed > 0 {
                pid = parsed
                do {
                    try deps.processSignaller.sendSIGTERM(pid: pid)
                } catch {
                    deps.logger.warning("stop: SIGTERM failed (continuing)", metadata: [
                        "pid": .public("\(pid)"),
                        "error": .private("\(error)"),
                    ])
                }
            } else {
                // Pidfile exists but we couldn't parse a pid. Don't
                // claim a clean stop without verification — fall
                // through to the flock probe, which is authoritative.
                deps.logger.warning("stop: pidfile present but unreadable/malformed; relying on flock probe", metadata: [
                    "path": .private(deps.paths.pidfilePath),
                ])
            }
        }
        let stopped: Bool
        if !pidfilePresent {
            stopped = true
        } else {
            stopped = await deps.processSignaller.waitForPidfileRelease(
                path: deps.paths.pidfilePath,
                timeoutSeconds: timeoutSeconds)
        }
        return StopOpsResult(
            stopped: stopped,
            pid: pid,
            unregisterInvoked: unregisterInvoked)
    }
}

public struct StartOpsDependencies {
    public let agentService: AgentService
    public let logger: LoggerProtocol

    public init(
        agentService: AgentService,
        logger: LoggerProtocol
    ) {
        self.agentService = agentService
        self.logger = logger
    }
}

public struct StartOpsResult: Equatable {
    public let outcome: AgentRegisterOutcome
    public let finalStatus: AgentServiceStatus
    public let started: Bool

    public init(
        outcome: AgentRegisterOutcome,
        finalStatus: AgentServiceStatus,
        started: Bool
    ) {
        self.outcome = outcome
        self.finalStatus = finalStatus
        self.started = started
    }
}

public enum StartOpsError: Error, CustomStringConvertible {
    case registerFailed(AgentServiceError)
    public var description: String {
        switch self {
        case .registerFailed(let e): return "register failed: \(e)"
        }
    }
}

public enum StartOps {
    /// Register; then poll `currentStatus()` for `.enabled` until
    /// `statusPollTimeoutSeconds` elapses. `started` is true iff the
    /// final status is `.enabled`.
    public static func run(
        _ deps: StartOpsDependencies,
        statusPollTimeoutSeconds: TimeInterval,
        statusPollIntervalNs: UInt64 = 200_000_000
    ) async throws -> StartOpsResult {
        let outcome: AgentRegisterOutcome
        do {
            outcome = try deps.agentService.register()
        } catch let err as AgentServiceError {
            throw StartOpsError.registerFailed(err)
        } catch {
            throw StartOpsError.registerFailed(.registrationFailed(
                message: "\(error)", requiresApproval: false))
        }

        let deadline = Date().addingTimeInterval(statusPollTimeoutSeconds)
        var status = deps.agentService.currentStatus()
        while status != .enabled, Date() < deadline {
            try? await Task.sleep(nanoseconds: statusPollIntervalNs)
            status = deps.agentService.currentStatus()
        }
        return StartOpsResult(
            outcome: outcome,
            finalStatus: status,
            started: status == .enabled)
    }
}
