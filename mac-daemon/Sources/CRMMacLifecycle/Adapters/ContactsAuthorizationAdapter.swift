// ContactsAuthorizationAdapter exposes the Contacts framework's
// `CNContactStore.authorizationStatus` + `requestAccess` to the
// install + doctor + daemon flows without dragging the Contacts
// framework into CRMMacLifecycle (which must stay testable on any
// platform).
//
// Production impl lives in CRMMacSystem (`ProductionContactsAuthorizationAdapter`).
// Tests inject a stub returning canned statuses.
import Foundation

/// Foundation-only mirror of `CNAuthorizationStatus` for the
/// .contacts entity. `.limited` was added in macOS 14 for the
/// privacy-friendlier "limited Contacts access" option; we treat
/// it as authorized.
public enum ContactsAuthorizationStatus: Equatable, Sendable {
    case notDetermined
    case restricted
    case denied
    case authorized
    case limited
}

public protocol ContactsAuthorizationAdapter: Sendable {
    /// Synchronous status read. Mirrors
    /// `CNContactStore.authorizationStatus(for: .contacts)`.
    func authorizationStatus() -> ContactsAuthorizationStatus

    /// Prompt the user for Contacts access. Returns true on grant,
    /// false on denial. Called only by `crm-mac install` —
    /// daemon ticks must NOT prompt (the daemon runs headless).
    func requestAccess() async throws -> Bool
}
