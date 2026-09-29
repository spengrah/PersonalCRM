// AnarlogSessionsPayloadShaping converts a CLI AnarlogSessionRecord
// into the meeting_note.recorded wire payload.
import Foundation
import CRMMacCore

public enum AnarlogSessionsPayloadShaping {

    /// Convert the CLI record projection into the wire payload.
    public static func shape(
        record: AnarlogSessionRecord,
        operatorPersonID: String,
        hostID: UUID
    ) -> MeetingNoteRecordedPayload {
        MeetingNoteRecordedPayload(
            version: CRMMacAnarlogSource.meetingNotePayloadVersion,
            hostID: hostID,
            source: SourceID.anarlogSessions.rawValue,
            sourceID: record.id,
            title: record.title,
            meetingAt: record.createdAt,
            summary: emptyToNil(record.summaries.first),
            memo: emptyToNil(record.memo),
            participantIDs: record.participants(excludingOperator: operatorPersonID).map(\.personID),
            tags: [])
    }

    /// Construct the wire-shape delete payload.
    public static func shapeDeleted(
        sessionID: String,
        hostID: UUID
    ) -> MeetingNoteDeletedPayload {
        MeetingNoteDeletedPayload(
            version: CRMMacAnarlogSource.meetingNotePayloadVersion,
            hostID: hostID,
            source: SourceID.anarlogSessions.rawValue,
            sourceID: sessionID)
    }

    private static func emptyToNil(_ value: String?) -> String? {
        guard let value, !value.isEmpty else { return nil }
        return value
    }
}
