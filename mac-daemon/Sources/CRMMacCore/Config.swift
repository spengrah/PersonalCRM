// DaemonConfig is the non-secret install-time configuration. The API
// key plaintext lives in the Keychain (CRMMacLifecycle.KeychainStore);
// this file is the operator-visible part of "what is this daemon
// configured to talk to."
//
// Loaded once at daemon start; not refreshed mid-process.
import Foundation

public enum ConfigStoreError: Error, Equatable, CustomStringConvertible {
    case fileNotFound(URL)
    case decode(String)
    case encode(String)
    case write(String)
    case invalidPiURL(String)

    public var description: String {
        switch self {
        case .fileNotFound(let url):
            return "config file not found at \(url.path)"
        case .decode(let reason):
            return "decode config.json: \(reason)"
        case .encode(let reason):
            return "encode config.json: \(reason)"
        case .write(let reason):
            return "write config.json: \(reason)"
        case .invalidPiURL(let raw):
            return "invalid pi_url \(raw): must be http(s)://… and parse as URL"
        }
    }
}

/// Persisted at `~/Library/Application Support/crm-mac/config.json`.
public struct DaemonConfig: Codable, Equatable {
    /// Base URL of the Pi-side API. Includes scheme + host (+ optional
    /// port). Example: `https://pi.example.ts.net`. No trailing slash;
    /// no path component.
    public var piURL: URL
    /// Server-assigned mac_host UUID from the pair response. Authoritative.
    public var hostID: UUID
    /// Operator-supplied label (`mac-1`, `work-mac`, etc.). PII-safe by
    /// convention — `crm-mac install --hostname` is required.
    public var hostname: String
    /// Wall-clock time of install, for support / debugging.
    public var installedAt: Date
    /// Per-source config blocks. Optional + additive — older
    /// `config.json` files written before this key landed decode with
    /// `sources == nil` and continue to work. The icloud_contacts
    /// source plugin reads `sources?.icloudContacts?.containers` to
    /// build its CNContainer allowlist.
    public var sources: DaemonSourcesConfig?

    public init(
        piURL: URL,
        hostID: UUID,
        hostname: String,
        installedAt: Date,
        sources: DaemonSourcesConfig? = nil
    ) {
        self.piURL = piURL
        self.hostID = hostID
        self.hostname = hostname
        self.installedAt = installedAt
        self.sources = sources
    }

    private enum CodingKeys: String, CodingKey {
        case piURL = "pi_url"
        case hostID = "host_id"
        case hostname
        case installedAt = "installed_at"
        case sources
    }
}

/// Atomic-write JSON wrapper around `config.json`.
public struct ConfigStore {
    private let fileURL: URL
    private let fileManager: FileManager

    public init(fileURL: URL, fileManager: FileManager = .default) {
        self.fileURL = fileURL
        self.fileManager = fileManager
    }

    public var path: String { fileURL.path }

    public func load() throws -> DaemonConfig {
        guard fileManager.fileExists(atPath: fileURL.path) else {
            throw ConfigStoreError.fileNotFound(fileURL)
        }
        let data: Data
        do {
            data = try Data(contentsOf: fileURL)
        } catch {
            throw ConfigStoreError.decode("read: \(error.localizedDescription)")
        }
        let decoder = JSONDecoder()
        decoder.dateDecodingStrategy = .iso8601
        let cfg: DaemonConfig
        do {
            cfg = try decoder.decode(DaemonConfig.self, from: data)
        } catch {
            throw ConfigStoreError.decode(String(describing: error))
        }
        try Self.validatePiURL(cfg.piURL)
        return cfg
    }

    public func save(_ cfg: DaemonConfig) throws {
        try Self.validatePiURL(cfg.piURL)
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
        encoder.dateEncodingStrategy = .iso8601
        let data: Data
        do {
            data = try encoder.encode(cfg)
        } catch {
            throw ConfigStoreError.encode(String(describing: error))
        }

        let dir = fileURL.deletingLastPathComponent()
        do {
            try fileManager.createDirectory(
                at: dir,
                withIntermediateDirectories: true)
        } catch {
            throw ConfigStoreError.write(
                "mkdir \(dir.path): \(error.localizedDescription)")
        }

        let tmpURL = fileURL
            .deletingLastPathComponent()
            .appendingPathComponent(fileURL.lastPathComponent + ".tmp.\(ProcessInfo.processInfo.processIdentifier)")
        do {
            try data.write(to: tmpURL, options: .atomic)
        } catch {
            throw ConfigStoreError.write("write tmp: \(error.localizedDescription)")
        }
        do {
            _ = try fileManager.replaceItemAt(fileURL, withItemAt: tmpURL)
        } catch {
            try? fileManager.removeItem(at: tmpURL)
            throw ConfigStoreError.write("rename: \(error.localizedDescription)")
        }
    }

    public func exists() -> Bool {
        fileManager.fileExists(atPath: fileURL.path)
    }

    /// Strict URL shape validation: requires an http(s) scheme and a
    /// non-empty host. Rejects relative paths and `file://`.
    public static func validatePiURL(_ url: URL) throws {
        guard let scheme = url.scheme?.lowercased(),
              scheme == "http" || scheme == "https",
              let host = url.host, !host.isEmpty else {
            throw ConfigStoreError.invalidPiURL(url.absoluteString)
        }
    }
}
