// ChatDBSchema — whitelist + schema-drift detection.
//
// macOS Messages.app's `chat.db` schema is private (Apple has no
// public commitment to stability). To detect drift between macOS
// versions, we whitelist the columns we read and validate at first
// open. On drift, the messages source marks itself unhealthy in
// source_health and the daemon stays up — the rest of the daemon
// (heartbeat + other plugins) is unaffected.
//
// The check uses PRAGMA table_info, which lists every column in a
// table including hidden ones. We compare against the required set
// and report missing columns; extra columns are allowed (Apple may
// add columns without breaking us).
import Foundation
import GRDB

/// Required columns per table that the reader touches. Updating this
/// list requires bumping the schema_version constant below.
public enum ChatDBSchema {
    /// Schema version label included in heartbeat source_health.
    /// On drift: replaced with "chat_db_drift:<table>.<column>" so the
    /// operator can see which column went missing.
    public static let okVersion = "chat_db_v2"

    public static let requiredColumns: [String: Set<String>] = [
        "message": [
            "ROWID", "guid", "text", "handle_id",
            "date", "is_from_me", "item_type", "cache_has_attachments",
            "associated_message_guid",
        ],
        "handle": ["ROWID", "id", "service"],
        "chat": ["ROWID", "guid", "style", "chat_identifier"],
        "chat_handle_join": ["chat_id", "handle_id"],
        "chat_message_join": ["chat_id", "message_id"],
        "attachment": [
            "ROWID", "guid", "uti", "mime_type",
            "transfer_name", "total_bytes",
        ],
        "message_attachment_join": ["message_id", "attachment_id"],
    ]
}

/// Result of a schema validation call.
public enum SchemaHealth: Equatable, Sendable {
    case ok
    /// One or more required columns missing. The reader should refuse
    /// to emit events; the plugin marks itself unhealthy with this
    /// reason in source_health.
    case drift(table: String, missing: Set<String>)

    /// Stable label suitable for heartbeat source_health surfaces.
    public var label: String {
        switch self {
        case .ok:
            return ChatDBSchema.okVersion
        case .drift(let table, let missing):
            // Pick a deterministic missing column for the label (sorted
            // alphabetically) so the same drift always produces the
            // same label string.
            let first = missing.sorted().first ?? "?"
            return "chat_db_drift:\(table).\(first)"
        }
    }
}

/// Pure validator over a GRDB Database handle. Tested via the
/// in-memory chat.db fixture.
public enum ChatDBSchemaValidator {
    /// Runs `PRAGMA table_info(...)` over every required table and
    /// returns `.ok` if every required column is present, otherwise
    /// `.drift` for the FIRST table (alphabetical order) found to be
    /// missing columns.
    public static func validate(db: Database) throws -> SchemaHealth {
        for table in ChatDBSchema.requiredColumns.keys.sorted() {
            let required = ChatDBSchema.requiredColumns[table] ?? []
            let actual = try fetchColumns(in: table, db: db)
            let missing = required.subtracting(actual)
            if !missing.isEmpty {
                return .drift(table: table, missing: missing)
            }
        }
        return .ok
    }

    /// Lowercases column names because SQLite identifiers are
    /// case-insensitive but our required set is mixed-case (e.g.,
    /// "ROWID"). We normalize both sides to lowercase before comparing.
    private static func fetchColumns(in table: String, db: Database) throws -> Set<String> {
        // PRAGMA table_info doesn't accept bound parameters; quote the
        // identifier defensively. The table list is hard-coded so SQL
        // injection isn't a real risk, but we still escape any
        // embedded quotes.
        let escapedTable = table.replacingOccurrences(of: "\"", with: "\"\"")
        let rows = try Row.fetchAll(db, sql: "PRAGMA table_info(\"\(escapedTable)\")")
        var columns: Set<String> = []
        for row in rows {
            if let name: String = row["name"] {
                columns.insert(name)
            }
        }
        // Case-insensitive comparison: also include lowercased copies.
        // Simpler: caller compares case-insensitively. We just return
        // the raw set and let `validate` lowercase both sides.
        return columns
    }
}

// Extend Set<String> for case-insensitive subtraction (used above).
private extension Set where Element == String {
    func subtracting(_ other: Set<String>) -> Set<String> {
        let lowerOther: Set<String> = Set(other.map { $0.lowercased() })
        return Set(self.filter { !lowerOther.contains($0.lowercased()) })
    }
}
