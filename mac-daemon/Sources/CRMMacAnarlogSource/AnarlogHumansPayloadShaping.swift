// AnarlogHumansPayloadShaping converts AnarlogHumanRecord into the
// wire-shape AnarlogExternalContactUpsertedPayload sent to
// /api/v1/ingest/events for kind=external_contact.upserted,
// source=anarlog_humans.
//
// Field mapping per the parent spec's humans frontmatter contract:
//   - displayName = frontmatter.name (fallback "<no name>" — Pi
//     requires non-empty for matching)
//   - emails      = frontmatter.emails (no type, primary=false)
//   - jobTitle    = frontmatter.job_title (nil when empty)
//   - metadata    = pinned + pin_order + memo? + linkedin_username? +
//                   org_id? + user_id? + created_at?
//
// The frontmatter `org_id` is a UUID reference (NOT a display name), so
// it lives in metadata rather than the top-level `organization` field
// the Pi-side struct exposes. The Pi-side enrichment can resolve it
// later from the anarlog organizations directory.
import Foundation
import CRMMacCore

public enum AnarlogHumansPayloadShaping {

    /// Convert a parsed human record into the wire payload.
    public static func shape(
        record: AnarlogHumanRecord,
        hostID: UUID
    ) -> AnarlogExternalContactUpsertedPayload {
        // Display name fallback — Pi rejects empty display names at
        // the matching layer.
        let displayName = record.name.isEmpty ? "<no name>" : record.name

        let emails = record.emails
            .filter { !$0.isEmpty }
            .map { AnarlogExternalContactMethodValue(value: $0) }

        // Always emit `pinned` + `pin_order` so metadata is never empty —
        // keeps the Pi-side wire contract stable when the operator's
        // frontmatter has no other fields.
        var metadata: [String: String] = [
            "pinned":    record.pinned ? "true" : "false",
            "pin_order": String(record.pinOrder),
        ]
        if !record.memo.isEmptyOrNil {
            metadata["memo"] = record.memo
        }
        if !record.linkedinUsername.isEmpty {
            metadata["linkedin_username"] = record.linkedinUsername
        }
        if !record.orgID.isEmpty {
            metadata["org_id"] = record.orgID
        }
        if !record.userID.isEmpty {
            metadata["user_id"] = record.userID
        }
        if let createdAt = record.createdAt {
            let f = ISO8601DateFormatter()
            f.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
            metadata["created_at"] = f.string(from: createdAt)
        }

        return AnarlogExternalContactUpsertedPayload(
            version: CRMMacAnarlogSource.humansPayloadVersion,
            hostID: hostID,
            source: SourceID.anarlogHumans.rawValue,
            entityID: record.uuid,
            displayName: displayName,
            emails: emails,
            jobTitle: record.jobTitle.isEmpty ? nil : record.jobTitle,
            metadata: metadata)
    }

    /// Construct the wire-shape delete payload. The prior payload
    /// hash that forms the `<uuid>@deleted@<hash>` source_id is built
    /// separately via AnarlogSourceIDBuilder.
    public static func shapeDeleted(
        entityID: String,
        hostID: UUID
    ) -> AnarlogExternalContactDeletedPayload {
        AnarlogExternalContactDeletedPayload(
            version: CRMMacAnarlogSource.humansPayloadVersion,
            hostID: hostID,
            source: SourceID.anarlogHumans.rawValue,
            entityID: entityID)
    }
}

private extension Optional where Wrapped == String {
    var isEmptyOrNil: Bool {
        switch self {
        case .none: return true
        case .some(let s): return s.isEmpty
        }
    }
}
