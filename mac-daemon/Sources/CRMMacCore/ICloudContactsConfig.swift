// ICloudContactsConfig is the icloud_contacts source's slice of the
// daemon's config file. The CNContainer allowlist is the single
// non-secret configuration the source needs; everything else (pi_url,
// host_id, hostname) is shared with the rest of the daemon and lives
// at the top level of DaemonConfig.
//
// Persisted under `sources.icloud_contacts` in `config.json` so the
// existing config file format stays backward-compatible — a daemon
// running an old config (no `sources` key) loads with no icloud
// allowlist, and the icloud plugin treats that as "not configured"
// and marks itself unhealthy until `crm-mac configure containers` is
// run.
import Foundation

public struct ICloudContactsConfig: Codable, Equatable, Sendable {
    /// `CNContainer.identifier` strings that the daemon should sync.
    /// Order is preserved for status display. Empty list disables the
    /// source.
    public var containers: [String]

    public init(containers: [String] = []) {
        self.containers = containers
    }
}

/// The `sources` top-level object in `config.json`. Optional — when
/// absent, `ConfigStore.load()` returns a `DaemonConfig` whose
/// `sources` field is nil and `loadICloudContactsConfig()` returns
/// nil. Future per-source configs live as additional fields here.
public struct DaemonSourcesConfig: Codable, Equatable, Sendable {
    public var icloudContacts: ICloudContactsConfig?

    public init(icloudContacts: ICloudContactsConfig? = nil) {
        self.icloudContacts = icloudContacts
    }

    private enum CodingKeys: String, CodingKey {
        case icloudContacts = "icloud_contacts"
    }
}

extension ConfigStore {
    /// Load the icloud_contacts allowlist if present. Returns nil
    /// when (a) the config file has no `sources` key, or (b) the
    /// `sources` key has no `icloud_contacts` entry. Mirrors the
    /// load semantics of DaemonConfig — strict decode, throws on
    /// malformed bytes.
    public func loadICloudContactsConfig() throws -> ICloudContactsConfig? {
        let cfg = try load()
        return cfg.sources?.icloudContacts
    }

    /// Persist the icloud_contacts allowlist. Idempotent — re-writes
    /// the full config file atomically. Preserves all other top-level
    /// keys (pi_url, host_id, hostname, installed_at) AND any other
    /// future per-source configs under the `sources` object.
    ///
    /// NOTE: callers that mutate the allowlist (e.g. `crm-mac configure
    /// containers`) MUST set the icloud_contacts recovery flag in
    /// `state.json` BEFORE invoking this method so a crash between the
    /// state-write and the config-write doesn't leave the daemon in
    /// a wrong-allowlist + no-recovery state. See plan D-JC3 (revised
    /// post-Codex-r3 P1-2 for crash-safety ordering).
    public func saveICloudContactsConfig(_ icloud: ICloudContactsConfig) throws {
        var cfg = try load()
        var sources = cfg.sources ?? DaemonSourcesConfig()
        sources.icloudContacts = icloud
        cfg.sources = sources
        try save(cfg)
    }
}
