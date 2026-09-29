// AnarlogHumansPayloadShaping maps CLI session participants to the
// external_contact.upserted wire payload with empty metadata.
import Foundation
import CRMMacCore

public enum AnarlogHumansPayloadShaping {
    public static func shape(
        participant: AnarlogParticipant,
        hostID: UUID
    ) -> AnarlogExternalContactUpsertedPayload {
        let displayName = participant.displayName.flatMap { $0.isEmpty ? nil : $0 } ?? "<no name>"
        let emails = participant.email.flatMap { $0.isEmpty ? nil : $0 }
            .map { [AnarlogExternalContactMethodValue(value: $0)] } ?? []
        let jobTitle = participant.jobTitle.flatMap { $0.isEmpty ? nil : $0 }

        return AnarlogExternalContactUpsertedPayload(
            version: CRMMacAnarlogSource.humansPayloadVersion,
            hostID: hostID,
            source: SourceID.anarlogHumans.rawValue,
            entityID: participant.personID,
            displayName: displayName,
            emails: emails,
            jobTitle: jobTitle,
            metadata: [:])
    }
}
