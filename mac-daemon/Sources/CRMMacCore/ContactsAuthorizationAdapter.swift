// ContactsAuthorizationAdapter exposes the Contacts framework's
// `CNContactStore.authorizationStatus` + `requestAccess` to the
// install + doctor + daemon flows without dragging the Contacts
// framework into the Foundation-only targets (which must stay
// testable on any platform).
//
// Lives in CRMMacCore so both CRMMacLifecycle (Doctor) and
// CRMMacIcloudContactsSource (per-tick auth probe) reference it
// without one importing the other.
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
    /// false on denial. Called by both `crm-mac install` AND the
    /// daemon's per-tick auth probe when status is `.notDetermined`.
    /// Calling from the launchd-spawned daemon is what attributes
    /// the TCC grant to the bundle ID (`xyz.spengrah.crm-mac`); the
    /// shell-spawned install path attributes to the parent terminal
    /// and is kept only as the user-driven entry point on a fresh
    /// pair.
    func requestAccess() async throws -> Bool
}
