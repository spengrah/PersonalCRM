// SourceIDBuilder constructs the IngestEvent source_id strings the
// Pi uses to dedupe external_contact events.
//
// Per spec §3 (source_id discriminator table at .ai/spec/mac-daemon.md
// lines 336-349):
//   - external_contact.upserted: `<entity_id>@<content_hash_hex>`
//   - external_contact.deleted:  `<entity_id>@deleted@<prior_content_hash_hex>`
//                                (or `<entity_id>@deleted@unknown` when
//                                the daemon has no prior hash on hand)
//
// The Pi recomputes the upserted hash from the payload and rejects on
// mismatch (EXTERNAL_CONTACT_HASH_MISMATCH); for delete it compares
// against the stored last_content_hash and rejects with
// EXTERNAL_CONTACT_DELETE_HASH_MISMATCH. The daemon's local
// ContactHashCache is the source of truth for the prior hash on a
// delete path; the `@deleted@unknown` sentinel is a documented
// fallback the Pi accepts when the cache was wiped or never seen the
// contact.
import Foundation

public enum SourceIDBuilder {
    /// `<entity_id>@<hash>`. Used as the IngestEvent.source_id for
    /// every external_contact.upserted event.
    public static func upsertSourceID(entityID: String, contentHash: String) -> String {
        "\(entityID)@\(contentHash)"
    }

    /// `<entity_id>@deleted@<prior_hash>` when a prior hash is known;
    /// `<entity_id>@deleted@unknown` when the daemon has none. The
    /// `unknown` literal is a Pi-side accepted sentinel; using it
    /// makes the event still dedup-stable across replays but loses
    /// the strict "this delete matches THIS content version"
    /// invariant.
    public static func deleteSourceID(
        entityID: String,
        priorContentHash: String?
    ) -> String {
        let suffix = priorContentHash ?? "unknown"
        return "\(entityID)@deleted@\(suffix)"
    }
}
