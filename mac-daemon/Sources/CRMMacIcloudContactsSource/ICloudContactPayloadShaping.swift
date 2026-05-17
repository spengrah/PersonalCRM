// ICloudContactPayloadShaping converts the daemon's neutral
// ContactRecord projection into the wire-shape
// ExternalContactUpsertedPayload that the Pi accepts at
// /api/v1/ingest/events.
//
// The Encodable conformance pins the JSON wire shape via explicit
// CodingKeys + manual `encode(to:)`. This mirrors PayloadShaping.swift
// in the messages source — wire shape is part of the type, not a
// keyEncodingStrategy convention.
//
// Birthday handling: emit ISO `YYYY-MM-DD` only when year + month +
// day are all present; otherwise omit the key. The Pi's payload
// contract treats birthday as optional.
//
// `photo_url` is intentionally always nil in v1 per the locked
// decision in the brief — CNContact.imageData is a base64 blob, not
// a URL, and the daemon has no Pi-side blob-storage endpoint to
// upload it to. A future PR can populate this field.
import Foundation
import CRMMacCore

public struct ExternalContactMethodValue: Encodable, Equatable, Sendable {
    public let value: String
    public let type: String?
    public let primary: Bool

    public init(value: String, type: String? = nil, primary: Bool = false) {
        self.value = value
        self.type = type
        self.primary = primary
    }

    enum CodingKeys: String, CodingKey {
        case value, type, primary
    }

    public func encode(to encoder: Encoder) throws {
        var c = encoder.container(keyedBy: CodingKeys.self)
        try c.encode(value, forKey: .value)
        if let type, !type.isEmpty {
            try c.encode(type, forKey: .type)
        }
        // omitempty on the Pi side: only emit when true so the wire
        // shape matches Go's struct-tag default-omit-on-zero.
        if primary {
            try c.encode(primary, forKey: .primary)
        }
    }
}

public struct ExternalContactAddressValue: Encodable, Equatable, Sendable {
    public let formatted: String
    public let type: String?

    public init(formatted: String, type: String? = nil) {
        self.formatted = formatted
        self.type = type
    }

    enum CodingKeys: String, CodingKey {
        case formatted, type
    }

    public func encode(to encoder: Encoder) throws {
        var c = encoder.container(keyedBy: CodingKeys.self)
        try c.encode(formatted, forKey: .formatted)
        if let type, !type.isEmpty {
            try c.encode(type, forKey: .type)
        }
    }
}

/// The wire envelope sent as the `payload` of an IngestEvent for
/// kind = `external_contact.upserted`. Mirrors the Go-side
/// `backend/internal/events/kinds.go:ExternalContactUpsertedPayload`
/// exactly (snake_case via CodingKeys; lowercase host_id; explicit
/// omission of empty optional fields).
public struct ExternalContactUpsertedPayload: Encodable, Equatable, Sendable {
    public let version: Int
    public let hostID: UUID
    public let source: String
    public let entityID: String
    public let displayName: String?
    public let firstName: String?
    public let lastName: String?
    public let emails: [ExternalContactMethodValue]
    public let phones: [ExternalContactMethodValue]
    public let addresses: [ExternalContactAddressValue]
    public let organization: String?
    public let jobTitle: String?
    public let birthday: String?
    public let photoURL: String?
    public let metadata: [String: String]

    enum CodingKeys: String, CodingKey {
        case version
        case hostID       = "host_id"
        case source
        case entityID     = "entity_id"
        case displayName  = "display_name"
        case firstName    = "first_name"
        case lastName     = "last_name"
        case emails
        case phones
        case addresses
        case organization
        case jobTitle     = "job_title"
        case birthday
        case photoURL     = "photo_url"
        case metadata
    }

    public init(
        version: Int,
        hostID: UUID,
        source: String,
        entityID: String,
        displayName: String? = nil,
        firstName: String? = nil,
        lastName: String? = nil,
        emails: [ExternalContactMethodValue] = [],
        phones: [ExternalContactMethodValue] = [],
        addresses: [ExternalContactAddressValue] = [],
        organization: String? = nil,
        jobTitle: String? = nil,
        birthday: String? = nil,
        photoURL: String? = nil,
        metadata: [String: String] = [:]
    ) {
        self.version = version
        self.hostID = hostID
        self.source = source
        self.entityID = entityID
        self.displayName = displayName
        self.firstName = firstName
        self.lastName = lastName
        self.emails = emails
        self.phones = phones
        self.addresses = addresses
        self.organization = organization
        self.jobTitle = jobTitle
        self.birthday = birthday
        self.photoURL = photoURL
        self.metadata = metadata
    }

    public func encode(to encoder: Encoder) throws {
        var c = encoder.container(keyedBy: CodingKeys.self)
        try c.encode(version, forKey: .version)
        // Lowercase UUID for Go wire-shape parity (Go's
        // uuid.UUID.MarshalJSON emits lowercase). Matches the
        // existing PayloadShaping.swift pattern.
        try c.encode(hostID.uuidString.lowercased(), forKey: .hostID)
        try c.encode(source, forKey: .source)
        try c.encode(entityID, forKey: .entityID)
        try c.encodeIfPresent(emptyToNil(displayName), forKey: .displayName)
        try c.encodeIfPresent(emptyToNil(firstName), forKey: .firstName)
        try c.encodeIfPresent(emptyToNil(lastName), forKey: .lastName)
        if !emails.isEmpty {
            try c.encode(emails, forKey: .emails)
        }
        if !phones.isEmpty {
            try c.encode(phones, forKey: .phones)
        }
        if !addresses.isEmpty {
            try c.encode(addresses, forKey: .addresses)
        }
        try c.encodeIfPresent(emptyToNil(organization), forKey: .organization)
        try c.encodeIfPresent(emptyToNil(jobTitle), forKey: .jobTitle)
        try c.encodeIfPresent(emptyToNil(birthday), forKey: .birthday)
        try c.encodeIfPresent(emptyToNil(photoURL), forKey: .photoURL)
        if !metadata.isEmpty {
            try c.encode(metadata, forKey: .metadata)
        }
    }

    private func emptyToNil(_ s: String?) -> String? {
        guard let s, !s.isEmpty else { return nil }
        return s
    }
}

/// The wire envelope sent as the `payload` of an IngestEvent for
/// kind = `external_contact.deleted`.
public struct ExternalContactDeletedPayload: Encodable, Equatable, Sendable {
    public let version: Int
    public let hostID: UUID
    public let source: String
    public let entityID: String

    enum CodingKeys: String, CodingKey {
        case version
        case hostID   = "host_id"
        case source
        case entityID = "entity_id"
    }

    public init(version: Int, hostID: UUID, source: String, entityID: String) {
        self.version = version
        self.hostID = hostID
        self.source = source
        self.entityID = entityID
    }

    public func encode(to encoder: Encoder) throws {
        var c = encoder.container(keyedBy: CodingKeys.self)
        try c.encode(version, forKey: .version)
        try c.encode(hostID.uuidString.lowercased(), forKey: .hostID)
        try c.encode(source, forKey: .source)
        try c.encode(entityID, forKey: .entityID)
    }
}

public enum ICloudContactPayloadShaping {

    /// Convert a ContactRecord into the wire-shape upserted payload.
    public static func shape(
        record: ContactRecord,
        hostID: UUID,
        source: String = "icloud_contacts"
    ) -> ExternalContactUpsertedPayload {
        let emails = record.emails.map {
            ExternalContactMethodValue(value: $0.value, type: $0.type, primary: $0.primary)
        }
        let phones = record.phones.map {
            ExternalContactMethodValue(value: $0.value, type: $0.type, primary: $0.primary)
        }
        let addresses = record.addresses.map {
            ExternalContactAddressValue(formatted: $0.formatted, type: $0.type)
        }
        let birthday = isoBirthday(from: record.birthday)
        // metadata.container_identifier is always emitted; the Pi
        // upserts this verbatim into external_contact.metadata.
        let metadata: [String: String] = [
            "container_identifier": record.containerIdentifier,
        ]
        // TODO(spec line 157): photo_url stays nil in v1 — CNContact
        // imageData is a base64 blob, not a URL; populating this
        // field requires Pi-side blob storage.
        return ExternalContactUpsertedPayload(
            version: CRMMacIcloudContactsSource.payloadVersion,
            hostID: hostID,
            source: source,
            entityID: record.identifier,
            displayName: record.displayName,
            firstName: record.firstName,
            lastName: record.lastName,
            emails: emails,
            phones: phones,
            addresses: addresses,
            organization: record.organization,
            jobTitle: record.jobTitle,
            birthday: birthday,
            photoURL: nil,
            metadata: metadata)
    }

    /// Convert a CNContact.identifier + host UUID into the wire-shape
    /// deleted payload. The prior content hash, which forms part of
    /// the delete source_id (`<entity>@deleted@<hash>`), is built
    /// separately via `SourceIDBuilder.deleteSourceID`.
    public static func shapeDeleted(
        identifier: String,
        hostID: UUID,
        source: String = "icloud_contacts"
    ) -> ExternalContactDeletedPayload {
        ExternalContactDeletedPayload(
            version: CRMMacIcloudContactsSource.payloadVersion,
            hostID: hostID,
            source: source,
            entityID: identifier)
    }

    /// Convert DateComponents to ISO `YYYY-MM-DD` if year + month +
    /// day are all present, else nil.
    static func isoBirthday(from components: DateComponents?) -> String? {
        guard let c = components,
              let y = c.year, let m = c.month, let d = c.day else {
            return nil
        }
        return String(format: "%04d-%02d-%02d", y, m, d)
    }
}
