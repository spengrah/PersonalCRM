// CallHistoryDBSchema — whitelist + schema-drift detection for the
// CallHistoryDB ZCALLRECORD table.
//
// macOS's CallHistoryDB schema is private (Apple has no public stability
// commitment). Empirically Apple has changed ZCALLRECORD columns across
// macOS releases (ZSERVICE_PROVIDER was added in Sequoia) but has not
// reliably updated Z_METADATA.version when doing so. We therefore
// detect drift via PRAGMA table_info, not the metadata version.
//
// On drift, the reader marks itself unhealthy in source_health and the
// daemon stays up (other plugins are unaffected). The unhealthy reason
// surfaces in the Mac settings page so the operator knows to update.
import Foundation
import GRDB

public enum CallHistoryDBSchema {
    /// Schema-version label included in heartbeat source_health when
    /// the schema is healthy. On drift, replaced with
    /// "call_history_db_drift:<table>.<column>".
    public static let okVersion = "call_history_db_v1"

    /// Columns the reader expects to find. Updating this list requires
    /// bumping the schema_version constant above.
    public static let requiredColumns: [String: Set<String>] = [
        "ZCALLRECORD": [
            "Z_PK",
            "ZUNIQUE_ID",
            "ZDATE",
            "ZADDRESS",
            "ZORIGINATED",
            "ZANSWERED",
            "ZDURATION",
            "ZSERVICE_PROVIDER",
            "ZCALLTYPE",
            "ZHASMESSAGE",
        ],
    ]
}

/// Result of a schema-validation call.
public enum CallHistorySchemaHealth: Equatable, Sendable {
    case ok
    /// One or more required columns missing. The reader refuses to emit
    /// events; the plugin marks itself unhealthy in source_health with
    /// this reason.
    case drift(table: String, missing: Set<String>)

    /// Stable label suitable for heartbeat source_health surfaces.
    public var label: String {
        switch self {
        case .ok:
            return CallHistoryDBSchema.okVersion
        case .drift(let table, let missing):
            // Deterministic missing-column pick: alphabetical first so
            // the same drift always renders the same label.
            let first = missing.sorted().first ?? "?"
            return "call_history_db_drift:\(table).\(first)"
        }
    }
}

/// Pure validator over a GRDB Database handle. Tested via an in-memory
/// CallHistoryDB fixture.
public enum CallHistoryDBSchemaValidator {
    /// Runs `PRAGMA table_info(...)` over every required table and
    /// returns `.ok` if every required column is present. Returns
    /// `.drift` for the FIRST table (alphabetical) found to be missing
    /// columns.
    public static func validate(db: Database) throws -> CallHistorySchemaHealth {
        for table in CallHistoryDBSchema.requiredColumns.keys.sorted() {
            let required = CallHistoryDBSchema.requiredColumns[table] ?? []
            let actual = try fetchColumns(in: table, db: db)
            let missing = required.subtracting(actual)
            if !missing.isEmpty {
                return .drift(table: table, missing: missing)
            }
        }
        return .ok
    }

    private static func fetchColumns(in table: String, db: Database) throws -> Set<String> {
        // PRAGMA table_info doesn't accept bound parameters; quote the
        // identifier defensively. The table list is hard-coded so SQL
        // injection isn't a real risk, but escape any embedded quotes.
        let escaped = table.replacingOccurrences(of: "\"", with: "\"\"")
        let rows = try Row.fetchAll(db, sql: "PRAGMA table_info(\"\(escaped)\")")
        var columns: Set<String> = []
        for row in rows {
            if let name: String = row["name"] {
                columns.insert(name)
            }
        }
        return columns
    }
}

// Case-insensitive subtraction. CallHistoryDB columns are
// case-insensitive in SQLite; we lower both sides to compare.
private extension Set where Element == String {
    func subtracting(_ other: Set<String>) -> Set<String> {
        let lowerOther: Set<String> = Set(other.map { $0.lowercased() })
        return Set(self.filter { !lowerOther.contains($0.lowercased()) })
    }
}
