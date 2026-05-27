// AnarlogSessionMetaParser decodes a session's `_meta.json` into an
// AnarlogSessionMeta. Custom DateDecodingStrategy handles the multiple
// timestamp shapes Anarlog has shipped (see AnarlogTimestampParser).
//
// Unparseable JSON or missing required fields → nil → P0 carry-forward.
import Foundation

public enum AnarlogSessionMetaParser {

    /// Parse `_meta.json` bytes for a session whose directory is
    /// named `uuid`. Returns nil on:
    ///   - non-UTF-8 bytes
    ///   - JSON parse failure
    ///   - missing `created_at` OR `created_at` parse failure
    ///   - missing both `title` and `created_at` (insufficient
    ///     structure to ship)
    public static func parse(
        uuid: String,
        metaJSONBytes: Data
    ) -> AnarlogSessionMeta? {
        // Permissive decode: we explicitly pull each field via
        // JSONSerialization rather than a strict struct decoder, so
        // unknown fields are silently ignored and a missing optional
        // doesn't trip a strict Decodable.
        let parsed: Any?
        do {
            parsed = try JSONSerialization.jsonObject(
                with: metaJSONBytes, options: [])
        } catch {
            return nil
        }
        guard let obj = parsed as? [String: Any] else { return nil }

        let title = (obj["title"] as? String) ?? ""
        guard let createdAtRaw = obj["created_at"] as? String,
              let createdAt = AnarlogTimestampParser.parse(createdAtRaw) else {
            return nil
        }
        let userID = (obj["user_id"] as? String) ?? ""

        var participants: [AnarlogSessionParticipant] = []
        if let rawParticipants = obj["participants"] as? [Any] {
            for entry in rawParticipants {
                guard let entryObj = entry as? [String: Any] else { continue }
                guard let humanID = entryObj["human_id"] as? String,
                      !humanID.isEmpty else {
                    continue
                }
                // Skip the recording user (self) per spec line 188.
                if !userID.isEmpty && humanID == userID {
                    continue
                }
                // Also skip the well-known self-UUID sentinel even if
                // user_id is empty in this file.
                if humanID == CRMMacAnarlogSource.selfHumanUUID {
                    continue
                }
                participants.append(AnarlogSessionParticipant(humanID: humanID))
            }
        }

        return AnarlogSessionMeta(
            uuid: uuid,
            title: title,
            createdAt: createdAt,
            userID: userID,
            participants: participants)
    }
}
