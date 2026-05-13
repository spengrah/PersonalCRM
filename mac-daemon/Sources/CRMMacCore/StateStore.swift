// StateStore persists DaemonState atomically (write to .tmp, then
// rename(2)). All reads return the parsed struct or a typed error.
//
// The atomic-rename pattern means a partial-write crash leaves the
// prior committed state intact rather than producing a half-written
// file. PR7/PR8 mutate state after every source-poll tick, so this
// is the durability boundary we rely on for cursor-correctness.
import Foundation

public enum StateStoreError: Error, Equatable, CustomStringConvertible {
    case fileNotFound(URL)
    case decode(String)
    case encode(String)
    case write(String)
    case schemaMismatch(found: Int, expected: Int)

    public var description: String {
        switch self {
        case .fileNotFound(let url):
            return "state file not found at \(url.path)"
        case .decode(let reason):
            return "decode state.json: \(reason)"
        case .encode(let reason):
            return "encode state.json: \(reason)"
        case .write(let reason):
            return "write state.json: \(reason)"
        case .schemaMismatch(let found, let expected):
            return "state.json schemaVersion=\(found); daemon expects \(expected). " +
                "An incompatible upgrade has happened — see the upgrade notes."
        }
    }
}

/// Pure read/write wrapper. Atomic-rename on write; strict schema
/// version check on read.
public struct StateStore {
    private let fileURL: URL
    private let fileManager: FileManager

    public init(fileURL: URL, fileManager: FileManager = .default) {
        self.fileURL = fileURL
        self.fileManager = fileManager
    }

    /// Load and decode. Throws StateStoreError on any failure.
    public func load() throws -> DaemonState {
        guard fileManager.fileExists(atPath: fileURL.path) else {
            throw StateStoreError.fileNotFound(fileURL)
        }
        let data: Data
        do {
            data = try Data(contentsOf: fileURL)
        } catch {
            throw StateStoreError.decode("read \(fileURL.path): \(error.localizedDescription)")
        }
        let decoder = JSONDecoder()
        decoder.dateDecodingStrategy = .iso8601
        let state: DaemonState
        do {
            state = try decoder.decode(DaemonState.self, from: data)
        } catch {
            throw StateStoreError.decode(String(describing: error))
        }
        guard state.schemaVersion == DaemonState.currentSchemaVersion else {
            throw StateStoreError.schemaMismatch(
                found: state.schemaVersion,
                expected: DaemonState.currentSchemaVersion)
        }
        return state
    }

    /// Encode + write via temp-and-rename. Creates the parent
    /// directory if missing. Throws StateStoreError on any failure.
    public func save(_ state: DaemonState) throws {
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
        encoder.dateEncodingStrategy = .iso8601
        let data: Data
        do {
            data = try encoder.encode(state)
        } catch {
            throw StateStoreError.encode(String(describing: error))
        }

        let dir = fileURL.deletingLastPathComponent()
        do {
            try fileManager.createDirectory(
                at: dir,
                withIntermediateDirectories: true)
        } catch {
            throw StateStoreError.write(
                "mkdir \(dir.path): \(error.localizedDescription)")
        }

        let tmpURL = fileURL
            .deletingLastPathComponent()
            .appendingPathComponent(fileURL.lastPathComponent + ".tmp.\(ProcessInfo.processInfo.processIdentifier)")
        do {
            try data.write(to: tmpURL, options: .atomic)
        } catch {
            throw StateStoreError.write("write tmp: \(error.localizedDescription)")
        }
        do {
            // rename(2) is atomic on the same filesystem. Foundation
            // wraps this and additionally removes any existing
            // destination, which is the semantic we want.
            _ = try fileManager.replaceItemAt(fileURL, withItemAt: tmpURL)
        } catch {
            // Best-effort cleanup of the tmp file before surfacing the
            // failure.
            try? fileManager.removeItem(at: tmpURL)
            throw StateStoreError.write("rename to \(fileURL.path): \(error.localizedDescription)")
        }
    }

    /// Convenience: write an empty `DaemonState` if no file exists,
    /// otherwise no-op. Used by Installer step 8.
    @discardableResult
    public func initializeIfMissing(hostID: UUID? = nil) throws -> Bool {
        if fileManager.fileExists(atPath: fileURL.path) {
            return false
        }
        let initial = DaemonState(
            schemaVersion: DaemonState.currentSchemaVersion,
            hostID: hostID,
            lastHeartbeatAt: nil,
            sources: [:])
        try save(initial)
        return true
    }
}
