// ProductionContactContainerEnumerator walks `CNContactStore.containers(matching:)`
// and maps each `CNContainer.containerType` to the Foundation-only
// `ContainerKind` enum (in CRMMacCore) so Doctor + the container
// picker stay framework-free.
//
// Default-include computation lives in `ContainerPicker.defaults(for:)`
// (in CRMMacIcloudContactsSource); this adapter just surfaces the
// `defaultIncluded` flag inline so callers don't need to import the
// picker module just to render container info.
import Foundation
@preconcurrency import Contacts
import CRMMacCore
import CRMMacLifecycle

/// `@unchecked Sendable` because `CNContactStore.containers(matching:)`
/// is documented thread-safe per Apple docs (the store is the
/// thread-safe entry point for read-only operations) but Foundation
/// hasn't retrofitted formal `Sendable` to the type.
public struct ProductionContactContainerEnumerator: ContactContainerEnumerator, @unchecked Sendable {
    private let store: CNContactStore

    public init(store: CNContactStore = CNContactStore()) {
        self.store = store
    }

    public func listContainers() throws -> [ContainerInfo] {
        // CNContactStore.containers(matching: nil) requires Contacts
        // authorization; without it it returns an empty list (no
        // throw on macOS), which we surface as .notAuthorized so
        // Doctor can produce a clear status line.
        let raw = CNContactStore.authorizationStatus(for: .contacts)
        if raw == .denied || raw == .restricted || raw == .notDetermined {
            throw ContactContainerEnumeratorError.notAuthorized
        }
        let containers: [CNContainer]
        do {
            containers = try store.containers(matching: nil)
        } catch {
            throw ContactContainerEnumeratorError.underlying(String(describing: error))
        }
        return containers.map { c in
            let kind = Self.mapKind(c.type)
            return ContainerInfo(
                identifier: c.identifier,
                name: c.name,
                type: kind,
                defaultIncluded: Self.isDefaultIncluded(kind: kind, name: c.name))
        }
    }

    /// Foundation-only mirror of `CNContainerType`. Mirrors
    /// `ContainerPicker.defaults(for:)`'s case dispatch — keeping
    /// the two in sync is a maintenance burden but the alternative
    /// is dragging the Contacts framework into CRMMacIcloudContactsSource's
    /// picker, which would cascade into CRMMacLifecycle when the
    /// picker render output is reused in the doctor flow.
    static func mapKind(_ raw: CNContainerType) -> ContainerKind {
        switch raw {
        case .local:      return .local
        case .cardDAV:    return .cardDAV
        case .exchange:   return .exchange
        case .unassigned: return .unassigned
        @unknown default:
            return .unknown(rawValue: raw.rawValue)
        }
    }

    /// Mirrors `ContainerPicker.defaults(for:)`. Kept inline here so
    /// the adapter can pre-populate `defaultIncluded` without the
    /// picker module being a CRMMacLifecycle dependency.
    static func isDefaultIncluded(kind: ContainerKind, name: String) -> Bool {
        switch kind {
        case .local:
            return true
        case .cardDAV:
            return name.lowercased() == "icloud"
        case .exchange, .unassigned, .unknown:
            return false
        }
    }
}
