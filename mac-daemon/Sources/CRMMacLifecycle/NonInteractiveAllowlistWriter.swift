// NonInteractiveAllowlistWriter performs the state-then-config
// write of an iCloud Contacts CNContainer allowlist WITHOUT
// touching the Contacts framework.
//
// Why it exists: the daemon under launchd is the only process
// context where CNContactStore is attributed to the bundle ID
// (`xyz.spengrah.crm-mac`); shell-spawned CLI subcommands hit the
// parent terminal's TCC permission, which typically lacks
// Contacts access. The non-interactive path (`--containers
// <uuid,uuid,…>`) trusts the operator-supplied identifiers, writes
// the allowlist, and lets the daemon's next tick validate against
// the live CNContainer list.
//
// Type signature deliberately excludes any ContactsAuthorizationAdapter
// or ContactContainerEnumerator parameter — that absence is the
// contract this writer enforces. If a future contributor reintroduces
// a Contacts-framework call into the non-interactive path, it can't
// route through this struct.
//
// Crash-safety contract (matches `configure containers`'s interactive
// flow): on a non-empty diff, the recovery flag is bumped in
// `state.json` FIRST, then `config.json` is replaced. A crash
// between the two leaves the daemon recovering against the OLD
// allowlist on next tick — still correct. A `.noOp` outcome
// (picked == existing) skips both writes entirely so a no-op
// re-request-permission run does NOT trigger a spurious recovery
// cycle.
import Foundation
import CRMMacCore

public enum NonInteractiveAllowlistWriteOutcome: Equatable {
    case wrote(pickedIDs: [String])
    /// New allowlist equals existing. No state or config write
    /// happens; recovery flag is NOT bumped.
    case noOp
}

/// Failures surfaced by `NonInteractiveAllowlistWriter.write(_:)`.
/// Distinguishes the two write phases so CLI wrappers can emit
/// phase-specific recovery guidance.
public enum NonInteractiveAllowlistWriteError: Error {
    /// The state-write (recovery flag) failed. No config-write was
    /// attempted; the existing allowlist is intact and no recovery
    /// cycle has been triggered.
    case stateWriteFailed(underlying: Error)
    /// The state-write succeeded (recovery flag IS set) but the
    /// subsequent config-write failed. The existing config.json is
    /// intact (atomic-rename), but the next tick will see the
    /// recovery flag and reconcile against the OLD allowlist. The
    /// operator should re-run the same command to complete the
    /// swap.
    case configWriteFailedAfterStateWrite(underlying: Error)
    /// Config-write failed and no state-write happened (either the
    /// caller passed `mutatingExistingConfig: false` with an empty
    /// existing allowlist, OR the no-op short-circuit fired). No
    /// recovery cycle is pending.
    case configWriteFailed(underlying: Error)
}

public struct NonInteractiveAllowlistWriter {
    public let configStore: ConfigStore
    public let stateStore: StateStore
    /// True when the caller is mutating an existing config (the
    /// daemon may already be running with the old allowlist).
    /// Drives the recovery-flag bump alongside the non-empty
    /// existing-allowlist guard. Fresh-install callers pass false;
    /// `--re-request-permission` and `configure containers` pass
    /// true.
    public let mutatingExistingConfig: Bool

    public init(
        configStore: ConfigStore,
        stateStore: StateStore,
        mutatingExistingConfig: Bool
    ) {
        self.configStore = configStore
        self.stateStore = stateStore
        self.mutatingExistingConfig = mutatingExistingConfig
    }

    /// Apply the caller-provided allowlist. Returns `.noOp` when
    /// the new set equals the existing set; in that case neither
    /// the recovery flag nor the config file is touched. Failures
    /// surface as typed `NonInteractiveAllowlistWriteError` cases
    /// so CLI wrappers can emit phase-specific recovery guidance.
    public func write(pickedIDs: [String]) async throws -> NonInteractiveAllowlistWriteOutcome {
        let existing = (try? configStore.loadICloudContactsConfig()?.containers) ?? []
        if Set(existing) == Set(pickedIDs) {
            return .noOp
        }
        let stateWriteHappened = mutatingExistingConfig || !existing.isEmpty
        if stateWriteHappened {
            // State-write FIRST. Crash between state and config
            // writes → daemon recovers against OLD allowlist on
            // next tick (still correct; idempotent).
            let mutator = StateMutator(store: stateStore)
            do {
                try await mutator.mutate { state in
                    var src = state.sources["icloud_contacts"] ?? SourceState()
                    src.lastError = "recovery_requested:allowlist_changed"
                    src.lastErrorAt = Date()
                    state.sources["icloud_contacts"] = src
                }
            } catch {
                throw NonInteractiveAllowlistWriteError.stateWriteFailed(
                    underlying: error)
            }
        }
        do {
            try configStore.saveICloudContactsConfig(
                ICloudContactsConfig(containers: pickedIDs))
        } catch {
            if stateWriteHappened {
                throw NonInteractiveAllowlistWriteError
                    .configWriteFailedAfterStateWrite(underlying: error)
            }
            throw NonInteractiveAllowlistWriteError
                .configWriteFailed(underlying: error)
        }
        return .wrote(pickedIDs: pickedIDs)
    }
}
