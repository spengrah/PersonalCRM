// AnarlogSessionsPayloadShaping converts a session's
// AnarlogSessionMeta + summary/memo bytes into the wire-shape
// MeetingNoteRecordedPayload sent to /api/v1/ingest/events for
// kind=meeting_note.recorded, source=anarlog_sessions.
//
// summary / memo are passed in as already-decoded strings (the
// plugin handles file I/O so this stays pure).
//
// title-vs-empty: when `meta.title` is empty AND that's all the meta
// gives us, we still emit `title: ""` so the wire shape is
// deterministic. Pi-side normalization can decide whether empty
// becomes nil.
import Foundation
import CRMMacCore

public enum AnarlogSessionsPayloadShaping {

    /// Convert a parsed session meta + optional summary/memo into the
    /// wire payload.
    public static func shape(
        meta: AnarlogSessionMeta,
        summary: String?,
        memo: String?,
        hostID: UUID
    ) -> MeetingNoteRecordedPayload {
        MeetingNoteRecordedPayload(
            version: CRMMacAnarlogSource.meetingNotePayloadVersion,
            hostID: hostID,
            source: SourceID.anarlogSessions.rawValue,
            sourceID: meta.uuid,
            title: meta.title,
            meetingAt: meta.createdAt,
            summary: emptyToNil(summary),
            memo: emptyToNil(memo),
            participantIDs: meta.participants.map(\.humanID),
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

    /// Returns the pre-backfill-floor check used by the sessions
    /// plugin to decide whether to emit a sentinel cursor entry.
    public static func isPreBackfillFloor(_ meta: AnarlogSessionMeta) -> Bool {
        meta.createdAt < CRMMacAnarlogSource.sessionsBackfillFloor
    }

    private static func emptyToNil(_ s: String?) -> String? {
        guard let s, !s.isEmpty else { return nil }
        return s
    }
}
