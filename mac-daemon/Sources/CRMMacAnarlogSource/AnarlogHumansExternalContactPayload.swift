// AnarlogHumansExternalContactPayload — duplicate of
// CRMMacIcloudContactsSource.ExternalContactUpsertedPayload /
// ExternalContactDeletedPayload, intentionally copied into this
// target so CRMMacAnarlogSource carries no cross-source dependency.
//
// Wire shape mirrors the Go-side
// `backend/internal/events/kinds.go:ExternalContactUpsertedPayload` —
// snake_case via explicit CodingKeys; explicit omission of empty
// optional fields via the Encodable conformance; lowercase host_id
// for Go uuid.UUID JSON parity.
import Foundation
import CRMMacCore

public struct AnarlogExternalContactMethodValue: Encodable, Equatable, Sendable {
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
        if primary {
            try c.encode(primary, forKey: .primary)
        }
    }
}

public struct AnarlogExternalContactUpsertedPayload: Encodable, Equatable, Sendable {
    public let version: Int
    public let hostID: UUID
    public let source: String
    public let entityID: String
    public let displayName: String?
    public let emails: [AnarlogExternalContactMethodValue]
    public let jobTitle: String?
    public let metadata: [String: String]

    enum CodingKeys: String, CodingKey {
        case version
        case hostID      = "host_id"
        case source
        case entityID    = "entity_id"
        case displayName = "display_name"
        case emails
        case jobTitle    = "job_title"
        case metadata
    }

    public init(
        version: Int,
        hostID: UUID,
        source: String,
        entityID: String,
        displayName: String? = nil,
        emails: [AnarlogExternalContactMethodValue] = [],
        jobTitle: String? = nil,
        metadata: [String: String] = [:]
    ) {
        self.version = version
        self.hostID = hostID
        self.source = source
        self.entityID = entityID
        self.displayName = displayName
        self.emails = emails
        self.jobTitle = jobTitle
        self.metadata = metadata
    }

    public func encode(to encoder: Encoder) throws {
        var c = encoder.container(keyedBy: CodingKeys.self)
        try c.encode(version, forKey: .version)
        try c.encode(hostID.uuidString.lowercased(), forKey: .hostID)
        try c.encode(source, forKey: .source)
        try c.encode(entityID, forKey: .entityID)
        try c.encodeIfPresent(nonEmpty(displayName), forKey: .displayName)
        if !emails.isEmpty {
            try c.encode(emails, forKey: .emails)
        }
        try c.encodeIfPresent(nonEmpty(jobTitle), forKey: .jobTitle)
        if !metadata.isEmpty {
            try c.encode(metadata, forKey: .metadata)
        }
    }

    private func nonEmpty(_ s: String?) -> String? {
        guard let s, !s.isEmpty else { return nil }
        return s
    }
}

public struct AnarlogExternalContactDeletedPayload: Encodable, Equatable, Sendable {
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
