// ContactChange is the enum a `CNChangeHistoryFetchRequest` walk emits
// one-per-event. The production reader
// (`CNContactStoreReader.changeHistory(from:)`) maps each
// `CNChangeHistoryEvent` subtype to this Foundation-only enum so the
// downstream dispatcher in `ICloudContactsSourcePlugin` can be
// unit-tested without Contacts permission.
//
// The `.unknown` case is the fail-closed signal for unrecognized
// event subtypes — the dispatcher logs, marks the source unhealthy,
// holds the cursor, and sets a one-shot recovery flag so the next
// tick performs a full /known-ids reconciliation instead of silently
// skipping the event.
import Foundation

public enum ContactChange: Equatable, Sendable {
    /// CNChangeHistoryAddContactEvent.
    case add(ContactRecord)
    /// CNChangeHistoryUpdateContactEvent.
    case update(ContactRecord)
    /// CNChangeHistoryDeleteContactEvent. Carries only the
    /// CNContact.identifier — Apple doesn't surface container or
    /// content metadata on delete events.
    case delete(identifier: String)
    /// Unrecognized `CNChangeHistoryEvent` subtype. Carries a
    /// human-readable description for log diagnostics; the dispatcher
    /// treats this as fail-closed.
    case unknown(rawEventDescription: String)
}
