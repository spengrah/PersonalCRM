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
    /// Payload version emitted on every external_contact.upserted
    /// envelope for source=anarlog_humans.
    public static let humansPayloadVersion: Int = 1

    /// Payload version emitted on every meeting_note.recorded /
    /// meeting_note.deleted envelope for source=anarlog_sessions.
    public static let meetingNotePayloadVersion: Int = 1

    /// Default cadence for the people plugin: 30 min (arc I9).
    public static let humansTickInterval: TimeInterval = 30 * 60

    /// Default cadence for the sessions plugin's hourly safety poll per
    /// spec line 59: 60 min. (FSEvents drives the real-time path.)
    public static let sessionsSafetyTickInterval: TimeInterval = 60 * 60

    /// Hard cap on the size of a single event payload. Anything larger
    /// triggers a `payload_too_large` warning + cursor-entry preserved
    /// per the P0 invariant — the daemon never emits a partial payload.
    public static let maxPayloadBytes: Int = 60 * 1024

    /// Self-human UUID sentinel — the user's own human file is named
    /// `00000000-0000-0000-0000-000000000000.md` per spec line 188 and
    /// is skipped at the reader level.
    public static let selfHumanUUID: String = "00000000-0000-0000-0000-000000000000"

    /// Files / dirs the sessions reader silently skips at the top
    /// level of the configured `sessions/` directory. Tracks the
    /// parent spec's skip-list plus a few entries observed in the
    /// real Anarlog notes folder.
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
}
