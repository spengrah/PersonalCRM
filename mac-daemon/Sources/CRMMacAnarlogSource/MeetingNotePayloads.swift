// MeetingNotePayloads — wire envelopes for the meeting_note.recorded
// and meeting_note.deleted IngestEvents emitted by the
// anarlog_sessions plugin.
//
// Per the parent spec: field name is `source_id` (NOT `entity_id`)
// for meeting_note kinds, mirroring the spec's source_id naming
// convention. The Pi-side struct shape lands separately; this is
// the daemon's wire shape.
import Foundation

public struct MeetingNoteRecordedPayload: Encodable, Equatable, Sendable {
    public let version: Int
    public let hostID: UUID
    public let source: String
    /// Session UUID. Mirrors spec's `source_id` naming for this kind.
    public let sourceID: String
    /// Optional title from `_meta.json.title`. Empty string when
    /// absent; nil when the meta has no title field at all. The
    /// distinction is preserved on the wire so the Pi-side handler
    /// can normalize either way without losing data.
    public let title: String?
    /// Session creation timestamp from `_meta.json.created_at`.
    public let meetingAt: Date
    /// `_summary.md` body; nil when the file is absent.
    public let summary: String?
    /// `_memo.md` body; nil when the file is absent.
    public let memo: String?
    /// Participant `anarlog_human_id` UUIDs (the recording user has
    /// been filtered out upstream).
    public let participantIDs: [String]
    /// Reserved for future tag extraction from frontmatter; emitted
    /// as `[]` in v1 so the wire shape is stable.
    public let tags: [String]

    enum CodingKeys: String, CodingKey {
        case version
        case hostID         = "host_id"
        case source
        case sourceID       = "source_id"
        case title
        case meetingAt      = "meeting_at"
        case summary
        case memo
        case participantIDs = "participant_ids"
        case tags
    }

    public init(
        version: Int,
        hostID: UUID,
        source: String,
        sourceID: String,
        title: String?,
        meetingAt: Date,
        summary: String?,
        memo: String?,
        participantIDs: [String],
        tags: [String]
    ) {
        self.version = version
        self.hostID = hostID
        self.source = source
        self.sourceID = sourceID
        self.title = title
        self.meetingAt = meetingAt
        self.summary = summary
        self.memo = memo
        self.participantIDs = participantIDs
        self.tags = tags
    }

    public func encode(to encoder: Encoder) throws {
        var c = encoder.container(keyedBy: CodingKeys.self)
        try c.encode(version, forKey: .version)
        try c.encode(hostID.uuidString.lowercased(), forKey: .hostID)
        try c.encode(source, forKey: .source)
        try c.encode(sourceID, forKey: .sourceID)
        try c.encodeIfPresent(title, forKey: .title)
        // RFC3339 with `Z` for parity with Go time.Time.MarshalJSON.
        let formatter = ISO8601DateFormatter()
        formatter.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        try c.encode(formatter.string(from: meetingAt), forKey: .meetingAt)
        try c.encodeIfPresent(summary, forKey: .summary)
        try c.encodeIfPresent(memo, forKey: .memo)
        try c.encode(participantIDs, forKey: .participantIDs)
        try c.encode(tags, forKey: .tags)
    }
}

public struct MeetingNoteDeletedPayload: Encodable, Equatable, Sendable {
    public let version: Int
    public let hostID: UUID
    public let source: String
    public let sourceID: String

    enum CodingKeys: String, CodingKey {
        case version
        case hostID   = "host_id"
        case source
        case sourceID = "source_id"
    }

    public init(version: Int, hostID: UUID, source: String, sourceID: String) {
        self.version = version
        self.hostID = hostID
        self.source = source
        self.sourceID = sourceID
    }

    public func encode(to encoder: Encoder) throws {
        var c = encoder.container(keyedBy: CodingKeys.self)
        try c.encode(version, forKey: .version)
        try c.encode(hostID.uuidString.lowercased(), forKey: .hostID)
        try c.encode(source, forKey: .source)
        try c.encode(sourceID, forKey: .sourceID)
    }
}
