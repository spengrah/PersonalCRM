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

public struct AnarlogConfig: Codable, Equatable, Sendable {
    /// Absolute path to the Anarlog notes root. Conventionally
    /// `~/Documents/notes/meetings`, but the operator chooses.
    /// Subdirectories `humans/` and `sessions/` are read by the
    /// respective plugins.
    public var rootPath: String
    /// Master switch for the anarlog_humans source plugin.
    public var humansEnabled: Bool
    /// Master switch for the anarlog_sessions source plugin.
    public var sessionsEnabled: Bool

    public init(
        rootPath: String,
        humansEnabled: Bool = false,
        sessionsEnabled: Bool = false
    ) {
        self.rootPath = rootPath
        self.humansEnabled = humansEnabled
        self.sessionsEnabled = sessionsEnabled
    }

    private enum CodingKeys: String, CodingKey {
        case rootPath        = "root_path"
        case humansEnabled   = "humans_enabled"
        case sessionsEnabled = "sessions_enabled"
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
