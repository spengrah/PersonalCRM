// CRMMacAnarlogSource is the namespace for the Anarlog source plugins
// and their supporting types. Both plugins read Anarlog only through
// the CLI and share its client, decoder, eligibility rules, and
// participant filter. Separate actors preserve failure isolation.
import Foundation

public enum CRMMacAnarlogSource {
    /// Payload version emitted on every external_contact.upserted
    /// envelope for source=anarlog_humans.
    public static let humansPayloadVersion: Int = 1

    /// Payload version emitted on every meeting_note.recorded /
    /// meeting_note.deleted envelope for source=anarlog_sessions.
    public static let meetingNotePayloadVersion: Int = 1

    /// Default cadence for the people plugin: 30 minutes, so a change
    /// reaches the CRM within about an hour.
    public static let humansTickInterval: TimeInterval = 30 * 60

    /// Default cadence for the sessions plugin: 30 minutes, so settled
    /// sessions reach the CRM within about an hour.
    public static let sessionsSafetyTickInterval: TimeInterval = 30 * 60

    /// Default CLI client factory for both source plugins and the
    /// session metadata lookup.
    public static let makeCLIClient: @Sendable (_ cliPath: String?) -> any AnarlogCLIClient = {
        AnarlogCLIProcessClient(cliPath: $0)
    }

    /// Hard cap on the size of a single event payload. Anything larger
    /// triggers a `payload_too_large` warning + cursor-entry preserved
    /// per the P0 invariant — the daemon never emits a partial payload.
    public static let maxPayloadBytes: Int = 60 * 1024

    /// Legacy Anarlog sessions list the operator under the all-zero
    /// person ID, which the participant filter always excludes.
    public static let selfHumanUUID: String = "00000000-0000-0000-0000-000000000000"

}
