// ContactContainerEnumerator is the protocol Doctor + the install
// flow use to list CNContainers without importing the Contacts
// framework. The production impl in CRMMacSystem
// (`ProductionContactContainerEnumerator`) wraps
// `CNContactStore.containers(matching:)` and maps `CNContainerType`
// to the Foundation-only `ContainerKind` enum.
//
// Tests inject a stub returning canned `[ContainerInfo]` so allowlist
// sanity + picker plumbing can run without Contacts permission.
import Foundation
import CRMMacCore

public enum ContactContainerEnumeratorError: Error, Equatable, CustomStringConvertible {
    case notAuthorized
    case underlying(String)

    public var description: String {
        switch self {
        case .notAuthorized:
            return "container enumeration: Contacts authorization missing"
        case .underlying(let s):
            return "container enumeration: \(s)"
        }
    }
}

public protocol ContactContainerEnumerator: Sendable {
    /// Enumerate visible CNContainers. May throw .notAuthorized if
    /// the daemon lacks Contacts permission OR .underlying for any
    /// other Contacts-framework failure.
    func listContainers() throws -> [ContainerInfo]
}
