// CRMMacAnarlogSource is the namespace for the anarlog reader source
// plugins (anarlog_humans + anarlog_sessions) and their supporting
// types. Both plugins live in this one target because they share a
// root directory + helper code and have no framework-specific deps
// (pure Foundation). Failure isolation is preserved by giving each
// plugin its own actor instance.
//
// FSEvents (CoreServices) is used only by the sessions plugin's
// watcher; that lives in CRMMacSystem to keep CoreServices imports
// out of this target.
import Foundation

public enum CRMMacAnarlogSource {
    /// Payload version emitted on every external_contact.upserted /
    /// external_contact.deleted envelope for source=anarlog_humans.
    public static let humansPayloadVersion: Int = 1

    /// Payload version emitted on every meeting_note.recorded /
    /// meeting_note.deleted envelope for source=anarlog_sessions.
    public static let meetingNotePayloadVersion: Int = 1

    /// Default cadence for the humans plugin per spec line 58: ~5 min.
    public static let humansTickInterval: TimeInterval = 5 * 60

    /// Default cadence for the sessions plugin's hourly safety poll per
    /// spec line 59: 60 min. (FSEvents drives the real-time path.)
    public static let sessionsSafetyTickInterval: TimeInterval = 60 * 60

    /// Backfill floor for sessions per spec line 61: 2026-01-01.
    /// Sessions whose `_meta.json.created_at` is older than this date
    /// are marked with a sentinel cursor entry and never emit a
    /// `meeting_note.recorded` event.
    public static let sessionsBackfillFloor: Date = {
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime]
        return f.date(from: "2026-01-01T00:00:00Z")!
    }()

    /// Hard cap on the size of a single event payload. Anything larger
    /// triggers a `payload_too_large` warning + cursor-entry preserved
    /// per the P0 invariant — the daemon never emits a partial payload.
    public static let maxPayloadBytes: Int = 60 * 1024

    /// Self-human UUID sentinel — the user's own human file is named
    /// `00000000-0000-0000-0000-000000000000.md` per spec line 188 and
    /// is skipped at the reader level.
    public static let selfHumanUUID: String = "00000000-0000-0000-0000-000000000000"

    /// Files / dirs the sessions reader silently skips at the top
    /// level of the configured `sessions/` directory. Per the parent
    /// spec line 201 + JC1 inspection.
    public static let sessionSkipEntries: Set<String> = [
        "chats",
        "daily_notes.json",
        "chat_shortcuts.json",
        "memories.json",
        "tasks.json",
        "settings.json",
        "store.json",
        "search_index",
        "plugins",
        "events.json",
        "calendars.json",
        ".hyprnote",
        ".DS_Store",
        "AGENTS.md",
        "organizations",
        "templates.json",
    ]

    /// Files the humans reader silently skips.
    public static let humanSkipEntries: Set<String> = [
        ".DS_Store",
        "AGENTS.md",
        // Self-human sentinel (spec line 188).
        "\(selfHumanUUID).md",
    ]
}
