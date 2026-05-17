// ContainerInfo is the Foundation-only projection of a `CNContainer`
// the container picker (in `CRMMacIcloudContactsSource`) AND Doctor's
// allowlist-sanity check (in `CRMMacLifecycle`) both consume. Keeping
// the type in `CRMMacCore` avoids a cross-target dependency cycle —
// CRMMacLifecycle MUST stay framework-free, and CRMMacIcloudContactsSource
// already depends on CRMMacCore.
//
// The production reader maps `CNContainer.containerType` to
// `ContainerKind` so this type carries no Contacts-framework imports.
import Foundation

public struct ContainerInfo: Equatable, Sendable, Hashable {
    /// `CNContainer.identifier` (a UUID-like opaque string). Stable
    /// across machine restarts on a given Mac for a given account.
    public var identifier: String
    /// Human-readable name — e.g. "iCloud", "On My Mac", "Google".
    public var name: String
    /// Foundation mirror of `CNContainerType`.
    public var type: ContainerKind
    /// Whether the default-selection logic (see
    /// `ContainerPicker.defaults(for:)`) recommends including this
    /// container in the allowlist on first install. Surfaced in the
    /// picker UI as a `[default]` suffix.
    public var defaultIncluded: Bool

    public init(
        identifier: String,
        name: String,
        type: ContainerKind,
        defaultIncluded: Bool
    ) {
        self.identifier = identifier
        self.name = name
        self.type = type
        self.defaultIncluded = defaultIncluded
    }
}
