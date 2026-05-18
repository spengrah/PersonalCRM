// AgentService abstracts the SMAppService API surface the
// install/upgrade/uninstall/doctor/status workflows need (plan D3).
// Production impl `ProductionAgentService` (CRMMacSystem) wraps
// `SMAppService.agent(plistName:)`. The protocol lives in
// CRMMacLifecycle so the workflows don't depend on the
// ServiceManagement framework — only the production adapter does.
//
// Replaces the previous `LaunchctlRunner` shape, which spoke in
// launchctl exit codes. The new surface is typed (status enum,
// outcome enum, typed error) so the workflows can branch on
// "requires approval" vs. "already registered" vs. "bundle missing"
// without parsing strings.
//
// The launchctl path is kept (LaunchctlRunner / ProductionLaunchctlRunner)
// for the legacy migration cleanup ONLY — bootout the pre-bundle
// launchd registration and delete the legacy plist.
import Foundation

/// Outcome of `AgentService.register()`. Apple's
/// `SMAppService.register()` throws on re-registration; the production
/// wrapper maps that specific failure mode to `.alreadyRegistered`
/// (it's a no-op from the system's perspective but observable from
/// the caller's — e.g. StartCommand prints a different hint).
public enum AgentRegisterOutcome: Equatable {
    /// Apple's `register()` succeeded; the agent is newly bootstrapped.
    case registered
    /// Apple's `register()` returned `kSMErrorAlreadyRegistered`. The
    /// agent was already registered before this call. From the
    /// system's perspective this is a no-op; from the caller's
    /// perspective the outcome is observable.
    case alreadyRegistered
}

/// Wire type for `SMAppService.Status`. The protocol lives in
/// CRMMacLifecycle so workflows can branch on these cases without
/// importing the ServiceManagement framework.
public enum AgentServiceStatus: String, Equatable {
    /// Service is registered and the agent is allowed to run.
    case enabled
    /// Service is registered but the user has not yet approved it in
    /// System Settings → General → Login Items → Allow in Background.
    case requiresApproval = "requires_approval"
    /// Service is not yet registered or has been unregistered.
    case notRegistered = "not_registered"
    /// The bundle / embedded plist is missing or unreachable.
    case notFound = "not_found"
}

public enum AgentServiceError: Error, CustomStringConvertible {
    /// `register()` failed. The production adapter maps
    /// `kSMErrorLaunchDeniedByUser` (operator denied in Login Items)
    /// to `requiresApproval: true`; all other failures surface with
    /// `requiresApproval: false` and the underlying error string.
    case registrationFailed(message: String, requiresApproval: Bool)
    /// `unregister()` failed.
    case unregistrationFailed(String)
    /// The bundle is missing or unreachable from the caller's
    /// perspective. Distinct from `.registrationFailed` because the
    /// operator's recovery path differs (re-run install --upgrade
    /// vs. approve in System Settings).
    case bundleNotFound(String)

    public var description: String {
        switch self {
        case .registrationFailed(let m, let approval):
            let suffix = approval
                ? " — approve crm-mac in System Settings → General → Login Items → Allow in Background, then re-run."
                : ""
            return "agent registration failed: \(m)\(suffix)"
        case .unregistrationFailed(let m):
            return "agent unregistration failed: \(m)"
        case .bundleNotFound(let m):
            return "agent bundle not found: \(m)"
        }
    }
}

public protocol AgentService {
    /// Register the agent with SMAppService. Apple's
    /// `SMAppService.register()` throws on re-registration; the
    /// production wrapper maps `kSMErrorAlreadyRegistered` to the
    /// `.alreadyRegistered` outcome and `kSMErrorLaunchDeniedByUser`
    /// to `AgentServiceError.registrationFailed(requiresApproval: true)`.
    /// All other errors throw `.registrationFailed(requiresApproval: false)`.
    /// See `ProductionAgentService.register()` for the error mapping
    /// implementation (plan D21).
    func register() throws -> AgentRegisterOutcome

    /// Unregister the agent. Async because Apple's
    /// `SMAppService.unregister(completionHandler:)` is callback-based;
    /// the wrapper bridges via a checked continuation. The Installer
    /// + Uninstaller combine this with a SIGTERM-then-poll on the
    /// pidfile (via ProcessSignaller) since `unregister` only removes
    /// the launchd registration — it does NOT terminate an
    /// already-running daemon process.
    func unregister() async throws

    /// Inspect the current registration state.  Used by Doctor +
    /// Status to surface to the operator without making a network
    /// call.
    func currentStatus() -> AgentServiceStatus
}
