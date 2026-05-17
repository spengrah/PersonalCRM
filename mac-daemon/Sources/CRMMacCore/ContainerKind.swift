// ContainerKind is the Foundation-only mirror of `CNContainerType`.
// Defined here so `CRMMacLifecycle` (Doctor) can describe containers
// without importing the Contacts framework. The production reader in
// `CRMMacIcloudContactsSource` maps `CNContainerType` → `ContainerKind`.
//
// `unknown(rawValue:)` exists so an Apple-side enum addition (a new
// container type added in a future macOS) flows through the daemon as
// a typed value rather than a compile failure on `@unknown default`.
// The container picker's defaults logic (see
// `ContainerPicker.defaults(for:)`) treats `.unknown` as
// default-EXCLUDE per the fail-closed policy in plan D-JC9.
import Foundation

public enum ContainerKind: Equatable, Hashable, Sendable, Codable {
    case local
    case cardDAV
    case exchange
    case unassigned
    case unknown(rawValue: Int)
}
