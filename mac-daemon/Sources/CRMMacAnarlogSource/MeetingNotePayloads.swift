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
    /// Optional CLI record `title`; nil omits the key on the wire.
    public let title: String?
    /// CLI record `created_at` timestamp.
    public let meetingAt: Date
    /// Markdown from the first CLI record summary; nil when absent or empty.
    public let summary: String?
    /// Markdown from the CLI record note; nil when absent or empty.
    public let memo: String?
    /// CLI record participants excluding the operator and zero-ID sentinel.
    public let participantIDs: [String]
    /// Always [] in payload version 1; the key stays so the wire shape is stable.
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
