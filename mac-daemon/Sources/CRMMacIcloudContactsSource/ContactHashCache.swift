// ContactHashCache is the per-Mac persistent map from CNContact
// identifier → content hash hex. The daemon uses the cache to build
// deterministic delete source_ids (`<entity>@deleted@<prior_hash>`)
// when CNChangeHistoryFetchRequest emits a delete event.
//
// Two-phase commit semantics:
//
//   - applyUpdates(_:) writes additions / replacements IMMEDIATELY to
//     the live in-memory map AND atomically rewrites the file.
//     Replays are idempotent: re-applying the same (id, hash) pair
//     produces the same on-disk state, and the Pi dedups the
//     accompanying event-log row.
//
//   - stagePendingRemovals(_:) records identifiers slated for removal
//     in an in-memory `pendingRemovals` set. The live map is NOT
//     mutated; `get(_:)` after staging still returns the prior hash
//     so subsequent dispatch code can construct the @deleted@<hash>
//     source_id correctly.
//
//   - commitPendingRemovals() actually removes the staged identifiers
//     from the live map + rewrites the file. Called by the plugin ONLY
//     after the Pi cursor commit succeeds.
//
//   - discardPendingRemovals() drops the staged set without
//     mutating the file. Called on cursor-commit failure OR any
//     tick abort so pendingRemovals cannot accumulate across ticks.
//
// Same-tick `.delete → .update` for the same identifier: applyUpdates
// ALSO removes the identifier from pendingRemovals so the subsequent
// commit step only finalizes identifiers whose final state in this
// tick is genuinely "removed". Without this, a `.delete → .update`
// sequence would stage X, then write the new hash, then
// commit-removals would drop X from the live map — next tick would
// emit `.deleted@unknown` for an entity the Pi already has.
//
// The actor serializes all calls so concurrent ticks (or the
// composition root's read-on-startup) can't interleave file writes.
//
// File format (atomic write):
// ```
// {
//   "schema_version": 1,
//   "hashes": {
//     "<CNContact.identifier>": "<sha256-hex>"
//   }
// }
// ```
// Schema bump: when the file shape changes incompatibly. The plugin
// treats a higher schema_version as "wipe + re-bootstrap" on load.
import Foundation

public enum ContactHashCacheError: Error, Equatable, CustomStringConvertible {
    case malformedFile(String)
    case write(String)

    public var description: String {
        switch self {
        case .malformedFile(let s):
            return "ContactHashCache: malformed cache file (\(s))"
        case .write(let s):
            return "ContactHashCache: write failed (\(s))"
        }
    }
}

public actor ContactHashCache {

    public static let schemaVersion: Int = 1

    private struct OnDisk: Codable {
        var schemaVersion: Int
        var hashes: [String: String]

        enum CodingKeys: String, CodingKey {
            case schemaVersion = "schema_version"
            case hashes
        }
    }

    private let fileURL: URL
    private let fileManager: FileManager
    private var hashes: [String: String] = [:]
    private var pendingRemovals: Set<String> = []

    public init(fileURL: URL, fileManager: FileManager = .default) {
        self.fileURL = fileURL
        self.fileManager = fileManager
    }

    /// Load the cache from disk. Absent file → empty map (no error).
    /// Malformed file → throws (operator must intervene).
    public func load() throws {
        if !fileManager.fileExists(atPath: fileURL.path) {
            hashes = [:]
            return
        }
        let data: Data
        do {
            data = try Data(contentsOf: fileURL)
        } catch {
            throw ContactHashCacheError.malformedFile("read: \(error)")
        }
        let decoded: OnDisk
        do {
            decoded = try JSONDecoder().decode(OnDisk.self, from: data)
        } catch {
            throw ContactHashCacheError.malformedFile("decode: \(error)")
        }
        // A higher schema_version means the on-disk shape has
        // changed in a way the daemon doesn't understand. The plugin
        // can detect this via load() throwing and respond by deleting
        // + re-bootstrapping; for v1 we just throw with a clear
        // message so the operator sees the issue in `crm-mac doctor`.
        if decoded.schemaVersion > Self.schemaVersion {
            throw ContactHashCacheError.malformedFile(
                "schema_version=\(decoded.schemaVersion) is newer than supported \(Self.schemaVersion)")
        }
        hashes = decoded.hashes
        pendingRemovals = []
    }

    /// Number of hashes in the live map. Debugging / status.
    public func size() -> Int { hashes.count }

    /// Read the prior hash for an identifier. Returns nil when the
    /// identifier is unknown OR the cache hasn't been populated yet.
    /// IMPORTANT: entries staged for removal still return their
    /// prior hash — the staged state is in-memory-only until
    /// commitPendingRemovals runs.
    public func get(_ identifier: String) -> String? {
        hashes[identifier]
    }

    /// Snapshot of all (identifier, hash) pairs. Used by the
    /// recovery path's diff against `/known-ids`.
    public func snapshot() -> [String: String] {
        hashes
    }

    /// Apply additions / replacements to the live map AND rewrite
    /// the file atomically. ALSO cancels any matching identifier
    /// from the pendingRemovals set — supports the same-tick
    /// `.delete → .update` case where the final state is "upserted",
    /// not "deleted".
    public func applyUpdates(_ updates: [String: String]) throws {
        if updates.isEmpty { return }
        for (k, v) in updates {
            hashes[k] = v
            pendingRemovals.remove(k)
        }
        try writeFile()
    }

    /// Stage identifiers for removal. No file write. The live map is
    /// unchanged so `get(_:)` continues to return the prior hash
    /// until commitPendingRemovals runs.
    public func stagePendingRemovals(_ identifiers: Set<String>) {
        pendingRemovals.formUnion(identifiers)
    }

    /// Number of identifiers currently staged for removal. Debug /
    /// status surface.
    public func pendingRemovalCount() -> Int { pendingRemovals.count }

    /// Finalize the staged removals: drop them from the live map +
    /// rewrite the file atomically. Called by the plugin only after
    /// the Pi cursor commit succeeds.
    public func commitPendingRemovals() throws {
        if pendingRemovals.isEmpty { return }
        for id in pendingRemovals {
            hashes.removeValue(forKey: id)
        }
        pendingRemovals.removeAll()
        try writeFile()
    }

    /// Drop the pendingRemovals set without mutating the live map
    /// or the file. Called by the plugin on cursor-commit failure
    /// (or any tick abort).
    public func discardPendingRemovals() {
        pendingRemovals.removeAll()
    }

    // MARK: - private

    private func writeFile() throws {
        let onDisk = OnDisk(schemaVersion: Self.schemaVersion, hashes: hashes)
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
        let data: Data
        do {
            data = try encoder.encode(onDisk)
        } catch {
            throw ContactHashCacheError.write("encode: \(error)")
        }
        let dir = fileURL.deletingLastPathComponent()
        do {
            try fileManager.createDirectory(at: dir, withIntermediateDirectories: true)
        } catch {
            throw ContactHashCacheError.write("mkdir \(dir.path): \(error)")
        }
        let tmpURL = dir.appendingPathComponent(
            fileURL.lastPathComponent + ".tmp.\(ProcessInfo.processInfo.processIdentifier)")
        do {
            try data.write(to: tmpURL, options: .atomic)
        } catch {
            throw ContactHashCacheError.write("write tmp: \(error)")
        }
        do {
            _ = try fileManager.replaceItemAt(fileURL, withItemAt: tmpURL)
        } catch {
            try? fileManager.removeItem(at: tmpURL)
            throw ContactHashCacheError.write("rename: \(error)")
        }
    }
}
