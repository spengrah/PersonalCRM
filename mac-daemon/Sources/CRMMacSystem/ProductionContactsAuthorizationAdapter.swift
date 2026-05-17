// ProductionContactsAuthorizationAdapter wraps `CNContactStore.authorizationStatus`
// + `requestAccess` for the production daemon + install flow. The
// `import Contacts` is isolated to CRMMacSystem so CRMMacLifecycle
// stays framework-free + testable everywhere.
import Foundation
@preconcurrency import Contacts
import CRMMacLifecycle

/// `@unchecked Sendable` because `CNContactStore` is documented thread-
/// safe for the methods we call (`authorizationStatus(for:)` is a
/// static class method; `requestAccess(for:completionHandler:)` is
/// callable from any thread per Apple docs) but Foundation hasn't
/// retrofitted formal `Sendable` to the type.
public struct ProductionContactsAuthorizationAdapter: ContactsAuthorizationAdapter, @unchecked Sendable {
    private let store: CNContactStore

    public init(store: CNContactStore = CNContactStore()) {
        self.store = store
    }

    public func authorizationStatus() -> ContactsAuthorizationStatus {
        let raw = CNContactStore.authorizationStatus(for: .contacts)
        return Self.mapStatus(raw)
    }

    public func requestAccess() async throws -> Bool {
        try await withCheckedThrowingContinuation { cont in
            store.requestAccess(for: .contacts) { granted, error in
                if let error {
                    cont.resume(throwing: error)
                    return
                }
                cont.resume(returning: granted)
            }
        }
    }

    static func mapStatus(_ raw: CNAuthorizationStatus) -> ContactsAuthorizationStatus {
        switch raw {
        case .notDetermined: return .notDetermined
        case .restricted:    return .restricted
        case .denied:        return .denied
        case .authorized:    return .authorized
        case .limited:       return .limited
        @unknown default:
            // A future macOS Contacts framework adding a new status
            // is fail-closed: treat as not-determined so the daemon's
            // tick refuses to proceed and the operator sees the
            // issue in `crm-mac doctor`.
            return .notDetermined
        }
    }
}
