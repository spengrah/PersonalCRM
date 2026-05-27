// SourceIDBuilder constructs the IngestEvent source_id strings the
// Pi uses to dedupe events. The recipe matches
// CRMMacIcloudContactsSource.SourceIDBuilder verbatim — intentionally
// duplicated (~30 LOC) so this target carries no cross-source
// dependency.
//
// Per spec §3 source_id discriminator table:
//   - external_contact.upserted / meeting_note.recorded:
//       `<entity_id>@<payload_hash>`
//   - external_contact.deleted / meeting_note.deleted:
//       `<entity_id>@deleted@<prior_payload_hash>`  OR
//       `<entity_id>@deleted@unknown` when the daemon has no prior
//       hash on hand (recovery + carry-forward fallback).
import Foundation

public enum AnarlogSourceIDBuilder {
    /// `<entity_id>@<payload_hash>`. Used as the IngestEvent.source_id
    /// for every external_contact.upserted / meeting_note.recorded
    /// event.
    public static func upsertSourceID(
        entityID: String,
        payloadHash: String
    ) -> String {
        "\(entityID)@\(payloadHash)"
    }

    /// `<entity_id>@deleted@<prior_payload_hash>` when a prior hash is
    /// known; `<entity_id>@deleted@unknown` when the daemon has none.
    /// The `unknown` literal is a Pi-side accepted sentinel; using it
    /// keeps the event dedup-stable across replays but loses the
    /// strict "this delete matches THIS content version" invariant.
    public static func deleteSourceID(
        entityID: String,
        priorPayloadHash: String?
    ) -> String {
        let suffix: String
        if let h = priorPayloadHash, !h.isEmpty {
            suffix = h
        } else {
            suffix = "unknown"
        }
        return "\(entityID)@deleted@\(suffix)"
    }
}
