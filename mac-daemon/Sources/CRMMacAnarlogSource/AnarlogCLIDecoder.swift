import CoreFoundation
import CRMMacCore
import Foundation

enum AnarlogCLIDecoder {

    static func envelope(_ stdout: Data, command: String) throws(AnarlogCLIFailure) -> [String: Any] {
        guard let root = object(from: stdout) else {
            throw .malformedOutput(command: command)
        }
        guard let schemaVersion = root["schema_version"] as? String else {
            throw .missingField(command: command, field: "schema_version")
        }
        guard schemaVersion == "1" else {
            throw .unsupportedSchemaVersion(schemaVersion)
        }
        return root
    }

    static func listPage(
        _ stdout: Data
    ) throws(AnarlogCLIFailure) -> (entries: [AnarlogSessionListEntry], nextOffset: Int?) {
        let root = try envelope(stdout, command: "meetings list")
        guard let data = root["data"] as? [Any] else {
            throw .missingField(command: "meetings list", field: "data")
        }
        var entries: [AnarlogSessionListEntry] = []
        entries.reserveCapacity(data.count)
        for item in data {
            guard let object = item as? [String: Any] else {
                throw .missingField(command: "meetings list", field: "data[].id")
            }
            guard let id = object["id"] as? String else {
                throw .missingField(command: "meetings list", field: "data[].id")
            }
            guard let rawCreatedAt = object["created_at"] as? String,
                  let createdAt = AnarlogTimestampParser.parse(rawCreatedAt) else {
                throw .missingField(command: "meetings list", field: "data[].created_at")
            }
            entries.append(AnarlogSessionListEntry(id: id.lowercased(), createdAt: createdAt))
        }

        guard let pagination = root["pagination"] as? [String: Any] else {
            throw .missingField(command: "meetings list", field: "pagination")
        }
        guard let rawNextOffset = pagination["next_offset"] else {
            throw .missingField(command: "meetings list", field: "pagination.next_offset")
        }
        if rawNextOffset is NSNull {
            return (entries, nil)
        }
        guard let number = rawNextOffset as? NSNumber,
              CFGetTypeID(number) != CFBooleanGetTypeID(),
              ["c", "s", "i", "l", "q"].contains(String(cString: number.objCType)),
              let nextOffset = Int(number.stringValue) else {
            throw .missingField(command: "meetings list", field: "pagination.next_offset")
        }
        return (entries, nextOffset)
    }

    static func session(_ stdout: Data) throws(AnarlogCLIFailure) -> AnarlogSessionRecord {
        let root = try envelope(stdout, command: "meetings get")
        guard let data = root["data"] as? [String: Any] else {
            throw .missingField(command: "meetings get", field: "data")
        }
        guard let id = data["id"] as? String else {
            throw .missingField(command: "meetings get", field: "data.id")
        }
        guard let rawTitle = data["title"] else {
            throw .missingField(command: "meetings get", field: "data.title")
        }
        let title: String?
        if rawTitle is NSNull {
            title = nil
        } else if let value = rawTitle as? String {
            title = value
        } else {
            throw .missingField(command: "meetings get", field: "data.title")
        }
        guard let rawCreatedAt = data["created_at"] as? String,
              let createdAt = AnarlogTimestampParser.parse(rawCreatedAt) else {
            throw .missingField(command: "meetings get", field: "data.created_at")
        }
        guard let rawNote = data["note"] else {
            throw .missingField(command: "meetings get", field: "data.note")
        }
        let memo: String?
        if rawNote is NSNull {
            memo = nil
        } else if let note = rawNote as? [String: Any] {
            guard let markdown = note["markdown"] as? String else {
                throw .missingField(command: "meetings get", field: "data.note.markdown")
            }
            memo = markdown
        } else {
            throw .missingField(command: "meetings get", field: "data.note")
        }
        guard let rawSummaries = data["summaries"] as? [Any] else {
            throw .missingField(command: "meetings get", field: "data.summaries")
        }
        var summaries: [String] = []
        summaries.reserveCapacity(rawSummaries.count)
        for item in rawSummaries {
            guard let summary = item as? [String: Any],
                  let markdown = summary["markdown"] as? String else {
                throw .missingField(command: "meetings get", field: "data.summaries[].markdown")
            }
            summaries.append(markdown)
        }
        guard let rawParticipants = data["participants"] as? [Any] else {
            throw .missingField(command: "meetings get", field: "data.participants")
        }
        var participants: [AnarlogParticipant] = []
        participants.reserveCapacity(rawParticipants.count)
        for item in rawParticipants {
            guard let participant = item as? [String: Any] else {
                throw .missingField(command: "meetings get", field: "data.participants[].human_id")
            }
            guard let personID = participant["human_id"] as? String else {
                throw .missingField(command: "meetings get", field: "data.participants[].human_id")
            }
            let displayName = try optionalString(
                participant["display_name"],
                command: "meetings get",
                field: "data.participants[].display_name")
            let email = try optionalString(
                participant["email"],
                command: "meetings get",
                field: "data.participants[].email")
            let jobTitle = try optionalString(
                participant["job_title"],
                command: "meetings get",
                field: "data.participants[].job_title")
            participants.append(AnarlogParticipant(
                personID: personID.lowercased(),
                displayName: displayName,
                email: email,
                jobTitle: jobTitle))
        }

        return AnarlogSessionRecord(
            id: id.lowercased(),
            title: title,
            createdAt: createdAt,
            memo: memo,
            summaries: summaries,
            participants: participants)
    }

    static func errorCode(stderr: Data, command: String) throws(AnarlogCLIFailure) -> String? {
        guard object(from: stderr) != nil else { return nil }
        let root = try envelope(stderr, command: command)
        guard let error = root["error"] as? [String: Any] else {
            throw .missingField(command: command, field: "error")
        }
        guard let code = error["code"] as? String else {
            throw .missingField(command: command, field: "error.code")
        }
        return code
    }

    private static func optionalString(
        _ raw: Any?,
        command: String,
        field: String
    ) throws(AnarlogCLIFailure) -> String? {
        guard let raw else { throw .missingField(command: command, field: field) }
        if raw is NSNull { return nil }
        guard let value = raw as? String else {
            throw .missingField(command: command, field: field)
        }
        return value
    }

    private static func object(from data: Data) -> [String: Any]? {
        guard String(data: data, encoding: .utf8) != nil,
              let value = try? JSONSerialization.jsonObject(
                with: data, options: [.fragmentsAllowed]),
              let object = value as? [String: Any] else {
            return nil
        }
        return object
    }
}
