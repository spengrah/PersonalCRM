// KnownIDsEndpoint — wire shapes for GET /api/v1/host/:id/sync/:source/known-ids.
//
// The endpoint returns the per-(host, source) set of live
// external_contact source_ids the Pi has on file for the calling
// host, paired with each row's last observed content hash. The Mac
// daemon's icloud_contacts source plugin uses this during recovery
// (token-invalid OR explicit recovery_requested flag) to (a)
// tombstone contacts the Pi has but the local scan no longer sees,
// AND (b) construct deterministic `<entity>@deleted@<hash>` source_ids.
//
// Response envelope: standard `{success, data, ...}`. The
// `data.ids` array is always non-nil (empty `[]` on a fresh CRM or
// for sources without external_contact rows). `last_content_hash`
// is nullable for rows imported before the column existed.
import Foundation

/// Decoded body of a single `data.ids[]` entry.
public struct KnownContactID: Decodable, Equatable, Sendable {
    public let sourceID: String
    public let lastContentHash: String?

    public init(sourceID: String, lastContentHash: String?) {
        self.sourceID = sourceID
        self.lastContentHash = lastContentHash
    }

    enum CodingKeys: String, CodingKey {
        case sourceID        = "source_id"
        case lastContentHash = "last_content_hash"
    }
}

/// Decoded body of the response's `data` field.
public struct KnownIDsData: Decodable, Equatable, Sendable {
    public let ids: [KnownContactID]

    public init(ids: [KnownContactID]) {
        self.ids = ids
    }
}
