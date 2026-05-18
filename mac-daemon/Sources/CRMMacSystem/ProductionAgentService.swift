// ProductionAgentService wraps `SMAppService.agent(plistName:)`.
// Lives in CRMMacSystem so the lifecycle target stays
// Foundation-only.
//
// Tested manually only — SMAppService can't actually register in CI
// because there's no real bundle on disk and TCC/admin can't be
// scripted. The CI surface for AgentService is the FakeAgentService
// in CRMMacLifecycleTests. The Hypothesis Validation procedure in
// the PR description exercises the production wrapper on the
// operator's Mac.
import Foundation
import ServiceManagement
import CRMMacLifecycle

@available(macOS 13.0, *)
public struct ProductionAgentService: AgentService {
    public let plistName: String

    public init(plistName: String) {
        self.plistName = plistName
    }

    public func register() throws -> AgentRegisterOutcome {
        let svc = SMAppService.agent(plistName: plistName)
        do {
            try svc.register()
            return .registered
        } catch {
            // ServiceManagement exports the error-code constants as
            // Int32 (Apple's OSStatus-style):
            //   - kSMErrorAlreadyRegistered  (macOS 13+)
            //   - kSMErrorLaunchDeniedByUser (macOS 13+)
            // The error DOMAIN constant `SMAppServiceErrorDomain` is
            // macOS-15-only (header annotation
            // `API_AVAILABLE(macos(15.0))`). Package.swift targets
            // macOS 14, so we cannot reference the symbol directly.
            // We use the literal string "SMAppServiceErrorDomain"
            // (the stable runtime value of the constant — verified
            // against ServiceManagement.framework on macOS 13-15).
            // Scoping the code switch to this domain prevents
            // misclassifying an unrelated NSError that happens to
            // have a colliding `code` value.
            //
            // CAVEAT: if Apple ever renames the underlying domain
            // string in a future SDK (highly unlikely — would break
            // every existing SMAppService client), this branch
            // silently stops matching and the wrapper falls through
            // to the generic registrationFailed path. The Hypothesis
            // Validation procedure catches this regression because an
            // already-registered re-register would surface as a
            // registrationFailed rather than the expected no-op.
            let ns = error as NSError
            if ns.domain == "SMAppServiceErrorDomain" {
                switch ns.code {
                case Int(kSMErrorAlreadyRegistered):
                    return .alreadyRegistered
                case Int(kSMErrorLaunchDeniedByUser):
                    throw AgentServiceError.registrationFailed(
                        message: String(describing: error),
                        requiresApproval: true)
                default:
                    break
                }
            }
            throw AgentServiceError.registrationFailed(
                message: String(describing: error),
                requiresApproval: false)
        }
    }

    public func unregister() async throws {
        let svc = SMAppService.agent(plistName: plistName)
        try await withCheckedThrowingContinuation { (continuation: CheckedContinuation<Void, Error>) in
            svc.unregister { err in
                if let err = err {
                    continuation.resume(
                        throwing: AgentServiceError.unregistrationFailed(
                            String(describing: err)))
                } else {
                    continuation.resume(returning: ())
                }
            }
        }
    }

    public func currentStatus() -> AgentServiceStatus {
        let svc = SMAppService.agent(plistName: plistName)
        switch svc.status {
        case .enabled: return .enabled
        case .requiresApproval: return .requiresApproval
        case .notFound: return .notFound
        case .notRegistered: return .notRegistered
        @unknown default: return .notRegistered
        }
    }
}
