// AllowlistConfigureFlow centralises the dispatch decision for
// every place the daemon CLI mutates the iCloud Contacts CNContainer
// allowlist:
//
//   - `crm-mac install`'s post-pair Contacts/picker flow
//   - `crm-mac install --re-request-permission` re-runs
//   - `crm-mac configure containers` (interactive picker)
//   - `crm-mac configure containers --containers <uuid,…>`
//   - `crm-mac configure containers --list`
//
// The contract this flow enforces is the regression guard for
// issue #322: non-interactive `--containers <uuid,…>` modes must
// NOT invoke the Contacts framework. Shell-spawned CLI subcommands
// hit the parent terminal's TCC permission, not the daemon's
// bundle-attributed permission, so calling `requestAccess()` /
// `listContainers()` would fail with a misleading error. The
// non-interactive path trusts the operator-supplied UUIDs; the
// daemon's next tick under launchd is the authoritative validation
// point.
//
// Adapters are injected as closures so tests can substitute
// call-counting spies and assert zero invocations on the
// non-interactive branches. Production callers (in the executable
// target) pass closures that build the real CRMMacSystem adapters.
import Foundation
import CRMMacCore

public struct AllowlistConfigureFlow: Sendable {
    public enum Mode: Equatable, Sendable {
        /// Fresh-install post-pair; the operator just paired and is
        /// picking the initial allowlist from the picker UI.
        case freshInstallInteractive
        /// Fresh-install post-pair with operator-supplied UUIDs.
        case freshInstallNonInteractive(rawContainers: String)
        /// `--re-request-permission` re-running the picker against
        /// an existing config.
        case reRequestPermissionInteractive
        /// `--re-request-permission` with operator-supplied UUIDs.
        case reRequestPermissionNonInteractive(rawContainers: String)
        /// `configure containers` interactive picker run.
        case configureInteractive
        /// `configure containers --containers <uuid,…>`
        /// non-interactive run.
        case configureNonInteractive(rawContainers: String)
        /// `configure containers --list` enumerate-only mode.
        case configureList
    }

    public enum Outcome: Equatable {
        case wrote(pickedIDs: [String])
        /// `picked == existing`; neither state nor config was
        /// modified. Both the writer and the flow propagate this so
        /// the CLI wrappers can print "No allowlist changes
        /// detected." instead of "Allowlist updated.".
        case noOp
        case listed(visible: [ContainerInfo])
        /// Interactive write succeeded with a non-noop diff.
        case completedInteractive(pickedIDs: [String])
    }

    public let mode: Mode
    public let configStore: ConfigStore
    public let stateStore: StateStore
    /// Closure that returns the production auth adapter. Invoked
    /// ONLY on interactive modes — never on non-interactive or
    /// `configureList` modes. Tests assert this via a spy that
    /// counts invocations of `authorizationStatus()` /
    /// `requestAccess()`.
    public let authAdapter: @Sendable () -> ContactsAuthorizationAdapter
    /// Closure that returns the production container enumerator.
    /// Invoked ONLY on interactive modes AND `configureList`;
    /// never on non-interactive modes.
    public let enumerator: @Sendable () -> ContactContainerEnumerator
    /// Interactive picker. Called only on interactive modes, after
    /// auth + enumeration succeed. Closures may throw
    /// `AllowlistConfigureFlowError.userAborted` to abort the run
    /// (e.g. the configure-containers y/N decline path) or any
    /// other error which is propagated to the caller verbatim.
    public let interactivePicker: @Sendable (_ visible: [ContainerInfo]) throws -> [String]

    public init(
        mode: Mode,
        configStore: ConfigStore,
        stateStore: StateStore,
        authAdapter: @escaping @Sendable () -> ContactsAuthorizationAdapter,
        enumerator: @escaping @Sendable () -> ContactContainerEnumerator,
        interactivePicker: @escaping @Sendable (_ visible: [ContainerInfo]) throws -> [String]
    ) {
        self.mode = mode
        self.configStore = configStore
        self.stateStore = stateStore
        self.authAdapter = authAdapter
        self.enumerator = enumerator
        self.interactivePicker = interactivePicker
    }

    public func run() async throws -> Outcome {
        switch mode {
        case .freshInstallNonInteractive(let raw),
             .reRequestPermissionNonInteractive(let raw),
             .configureNonInteractive(let raw):
            // Non-interactive: NO Contacts framework calls.
            let pickedIDs = ContainerAllowlistInput.parse(raw)
            let writer = NonInteractiveAllowlistWriter(
                configStore: configStore,
                stateStore: stateStore,
                mutatingExistingConfig: !mode.isFreshInstall)
            switch try await writer.write(pickedIDs: pickedIDs) {
            case .wrote(let ids): return .wrote(pickedIDs: ids)
            case .noOp: return .noOp
            }
        case .configureList:
            let visible = try enumerator().listContainers()
            return .listed(visible: visible)
        case .freshInstallInteractive,
             .reRequestPermissionInteractive,
             .configureInteractive:
            let granted = try await authAdapter().requestAccess()
            guard granted else {
                throw AllowlistConfigureFlowError.permissionDenied
            }
            let visible = try enumerator().listContainers()
            let pickedIDs = try interactivePicker(visible)
            let writer = NonInteractiveAllowlistWriter(
                configStore: configStore,
                stateStore: stateStore,
                mutatingExistingConfig: !mode.isFreshInstall)
            switch try await writer.write(pickedIDs: pickedIDs) {
            case .wrote: return .completedInteractive(pickedIDs: pickedIDs)
            case .noOp:  return .noOp
            }
        }
    }
}

public enum AllowlistConfigureFlowError: Error, Equatable {
    case permissionDenied
    /// Thrown by the `interactivePicker` closure (configure
    /// containers only) when the user declines the y/N diff
    /// confirmation. The flow does not raise this itself; it only
    /// propagates from the closure.
    case userAborted
}

extension AllowlistConfigureFlow.Mode {
    public var isFreshInstall: Bool {
        switch self {
        case .freshInstallInteractive, .freshInstallNonInteractive: return true
        default: return false
        }
    }
}
