// AnarlogConfig is the anarlog reader sources' slice of the daemon's
// config file. The two anarlog source plugins (anarlog_humans and
// anarlog_sessions) share a single root directory ("the Anarlog
// notes folder") and each has its own enable flag — both default false
// so the operator opts in deliberately via
// `crm-mac configure anarlog --path <abs> --enable {humans|sessions|both}`.
//
// Persisted under `sources.anarlog` in `config.json` so the existing
// config format stays backward-compatible — a daemon running an older
// config (no `sources.anarlog` key) loads with no anarlog readers and
// the plugins mark themselves "not_configured" until the operator runs
// `crm-mac configure anarlog`.
import Foundation

public enum AnarlogConfigError: Error, Equatable, Sendable {
    case operatorPersonIDNotUUID(String)
    case cliPathNotAbsolute(String)
    case negativeDeletionCap(Int)
}

public struct AnarlogConfig: Codable, Equatable, Sendable {
    public static let defaultDeletionCap = 5

    /// Absolute path to the Anarlog notes root. Conventionally
    /// `~/Documents/notes/meetings`, but the operator chooses.
    /// Subdirectories `humans/` and `sessions/` are read by the
    /// respective plugins.
    public var rootPath: String
    /// Master switch for the anarlog_humans source plugin.
    public var humansEnabled: Bool
    /// Master switch for the anarlog_sessions source plugin.
    public var sessionsEnabled: Bool
    /// Anarlog person ID for the operator, excluded from participant sync.
    public private(set) var operatorPersonID: String?
    /// Absolute path to the Anarlog CLI, or nil to use the default search.
    public private(set) var cliPath: String?
    /// Maximum number of session deletions permitted in one tick.
    public private(set) var deletionCap: Int

    public init(
        rootPath: String,
        humansEnabled: Bool = false,
        sessionsEnabled: Bool = false
    ) {
        self.rootPath = rootPath
        self.humansEnabled = humansEnabled
        self.sessionsEnabled = sessionsEnabled
        self.operatorPersonID = nil
        self.cliPath = nil
        self.deletionCap = Self.defaultDeletionCap
    }

    public mutating func setOperatorPersonID(_ raw: String) throws(AnarlogConfigError) {
        guard let uuid = UUID(uuidString: raw) else {
            throw .operatorPersonIDNotUUID(raw)
        }
        operatorPersonID = uuid.uuidString.lowercased()
    }

    public mutating func setCLIPath(_ raw: String) throws(AnarlogConfigError) {
        guard raw.hasPrefix("/") else {
            throw .cliPathNotAbsolute(raw)
        }
        cliPath = raw
    }

    public mutating func setDeletionCap(_ value: Int) throws(AnarlogConfigError) {
        guard value >= 0 else {
            throw .negativeDeletionCap(value)
        }
        deletionCap = value
    }

    public init(from decoder: any Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        rootPath = try container.decode(String.self, forKey: .rootPath)
        humansEnabled = try container.decode(Bool.self, forKey: .humansEnabled)
        sessionsEnabled = try container.decode(Bool.self, forKey: .sessionsEnabled)
        operatorPersonID = try container.decodeIfPresent(String.self, forKey: .operatorPersonID)
        cliPath = try container.decodeIfPresent(String.self, forKey: .cliPath)
        deletionCap = try container.decodeIfPresent(Int.self, forKey: .deletionCap)
            ?? Self.defaultDeletionCap
    }

    public func encode(to encoder: any Encoder) throws {
        var container = encoder.container(keyedBy: CodingKeys.self)
        try container.encode(rootPath, forKey: .rootPath)
        try container.encode(humansEnabled, forKey: .humansEnabled)
        try container.encode(sessionsEnabled, forKey: .sessionsEnabled)
        try container.encodeIfPresent(operatorPersonID, forKey: .operatorPersonID)
        try container.encodeIfPresent(cliPath, forKey: .cliPath)
        try container.encode(deletionCap, forKey: .deletionCap)
    }

    private enum CodingKeys: String, CodingKey {
        case rootPath        = "root_path"
        case humansEnabled   = "humans_enabled"
        case sessionsEnabled = "sessions_enabled"
        case operatorPersonID = "operator_person_id"
        case cliPath          = "cli_path"
        case deletionCap      = "deletion_cap"
    }
}

extension ConfigStore {
    /// Load the anarlog config if present. Returns nil when (a) the
    /// config file has no `sources` key, or (b) the `sources` key has
    /// no `anarlog` entry. Mirrors `loadICloudContactsConfig()`.
    public func loadAnarlogConfig() throws -> AnarlogConfig? {
        let cfg = try load()
        return cfg.sources?.anarlog
    }

    /// Persist the anarlog config. Idempotent — re-writes the full
    /// config file atomically. Preserves all other top-level keys and
    /// every other per-source config under `sources`.
    public func saveAnarlogConfig(_ anarlog: AnarlogConfig) throws {
        var cfg = try load()
        var sources = cfg.sources ?? DaemonSourcesConfig()
        sources.anarlog = anarlog
        cfg.sources = sources
        try save(cfg)
    }
}
