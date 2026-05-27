// AnarlogSessionMeta is the daemon's neutral projection of an
// Anarlog `sessions/<uuid>/_meta.json` file.
//
// Observed v1.0.x shape:
//   { id: "<session-uuid>",
//     title: "...",
//     created_at: "2026-03-16T20:34:49.936Z",
//     user_id: "<self-human-uuid>",
//     participants: [ { human_id, id, session_id, source, user_id } ] }
//
// Field set per spec lines 196-198. Unknown keys are not retained on
// the meta projection — we capture what we ship Pi-side and rely on
// the file-bytes hash to detect any change including additions.
import Foundation

public struct AnarlogSessionParticipant: Equatable, Sendable {
    /// The participant's human UUID — maps Pi-side to a
    /// `external_identity` row of type `anarlog_human_id`. Sessions
    /// reference humans via this id; the matching itself lives in PR 3+.
    public let humanID: String

    public init(humanID: String) {
        self.humanID = humanID
    }
}

public struct AnarlogSessionMeta: Equatable, Sendable {
    /// File-system UUID — derived from the directory name, NOT
    /// `_meta.json.id`. The two should match; on mismatch the
    /// directory name wins (it's the cursor key).
    public let uuid: String

    /// `_meta.json.title` — may be empty.
    public let title: String

    /// `_meta.json.created_at` parsed via AnarlogTimestampParser.
    /// Required; sessions whose `created_at` fails to parse are
    /// treated as malformed → P0 carry-forward.
    public let createdAt: Date

    /// `_meta.json.user_id` — the recording user's self-human UUID.
    /// Filtered out of participants per spec line 188.
    public let userID: String

    /// `_meta.json.participants[].human_id`. Self-user UUIDs are
    /// pre-filtered. Empty when the meta lists none (or all were the
    /// self user).
    public let participants: [AnarlogSessionParticipant]
}
