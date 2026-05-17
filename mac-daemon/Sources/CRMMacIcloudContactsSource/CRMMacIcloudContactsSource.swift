// CRMMacIcloudContactsSource is the namespace for the icloud_contacts
// source plugin and its supporting types. The module hosts the
// thin shell over Apple's Contacts framework (CNContactStoreReader),
// the actor-based per-tick orchestrator (ICloudContactsSourcePlugin),
// the cross-language content-hash cache (ContactHashCache), and the
// pure-logic helpers (ContainerPicker, ICloudContactPayloadShaping,
// SourceIDBuilder, ICloudContactsPublisher).
//
// Contacts framework imports are isolated to this target so the rest
// of the daemon stays Foundation-only.
import Foundation

public enum CRMMacIcloudContactsSource {
    /// Payload version emitted on every external_contact.upserted /
    /// external_contact.deleted envelope. Bumped lockstep with the
    /// Pi-side payload contract.
    public static let payloadVersion: Int = 1

    /// Default cadence per spec line 55: 15 minutes. The plugin
    /// accepts an override at construction time so unit tests can
    /// drive ticks deterministically.
    public static let defaultTickInterval: TimeInterval = 15 * 60
}
