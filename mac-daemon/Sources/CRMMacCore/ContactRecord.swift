// ContactRecord and friends are the daemon's in-process projection of
// a single iCloud contact (one CNContact). The production reader
// (`CNContactStoreReader` in `CRMMacIcloudContactsSource`) converts
// `CNContact` → `ContactRecord` so the pure-logic emission pipeline
// (payload shaping + content hashing + source_id construction +
// dispatcher decisions) stays free of any Contacts framework
// dependency and is fully unit-testable in CI.
//
// The struct intentionally lives in `CRMMacCore` (Foundation only).
// Targets that need to inspect contact data (Doctor's allowlist sanity
// check, the icloud source plugin, future analytics) all import
// CRMMacCore.
import Foundation

/// A single iCloud contact, normalized to the subset of fields the
/// Pi-side `ExternalContactUpsertedPayload` accepts.
public struct ContactRecord: Codable, Equatable, Sendable {
    /// Stable `CNContact.identifier`. The Pi's `entity_id` /
    /// `source_id` prefix is derived from this.
    public var identifier: String
    /// The `CNContainer.identifier` this contact belongs to. Used by
    /// the source plugin's allowlist filter AND emitted as
    /// `metadata.container_identifier`.
    public var containerIdentifier: String
    /// Display name (`CNContactFormatter.string(from: contact,
    /// style: .fullName)` on the reader side). Optional because
    /// CNContact allows empty contacts.
    public var displayName: String?
    public var firstName: String?
    public var lastName: String?
    public var emails: [ContactEmail]
    public var phones: [ContactPhone]
    public var addresses: [ContactAddress]
    public var organization: String?
    public var jobTitle: String?
    /// Date components extracted from `CNContact.birthday`. Encoded
    /// to ISO `YYYY-MM-DD` only when year + month + day are all
    /// present; otherwise omitted from the payload.
    public var birthday: DateComponents?

    public init(
        identifier: String,
        containerIdentifier: String,
        displayName: String? = nil,
        firstName: String? = nil,
        lastName: String? = nil,
        emails: [ContactEmail] = [],
        phones: [ContactPhone] = [],
        addresses: [ContactAddress] = [],
        organization: String? = nil,
        jobTitle: String? = nil,
        birthday: DateComponents? = nil
    ) {
        self.identifier = identifier
        self.containerIdentifier = containerIdentifier
        self.displayName = displayName
        self.firstName = firstName
        self.lastName = lastName
        self.emails = emails
        self.phones = phones
        self.addresses = addresses
        self.organization = organization
        self.jobTitle = jobTitle
        self.birthday = birthday
    }
}

/// One email or phone value with optional display tag + primary flag.
public struct ContactEmail: Codable, Equatable, Sendable {
    public var value: String
    public var type: String?
    public var primary: Bool

    public init(value: String, type: String? = nil, primary: Bool = false) {
        self.value = value
        self.type = type
        self.primary = primary
    }
}

public struct ContactPhone: Codable, Equatable, Sendable {
    public var value: String
    public var type: String?
    public var primary: Bool

    public init(value: String, type: String? = nil, primary: Bool = false) {
        self.value = value
        self.type = type
        self.primary = primary
    }
}

/// Postal address. The Pi-side payload is metadata-only — no
/// geocoding, no per-field decomposition.
public struct ContactAddress: Codable, Equatable, Sendable {
    public var formatted: String
    public var type: String?

    public init(formatted: String, type: String? = nil) {
        self.formatted = formatted
        self.type = type
    }
}
