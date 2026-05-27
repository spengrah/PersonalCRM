// FilesystemAdapter wraps the filesystem operations the installer +
// uninstaller need. Most callers can stick to Foundation — this
// protocol exists so the installer's branchy logic can be exercised
// against an in-memory filesystem without touching the dev machine's
// disk.
import Foundation

public enum FilesystemError: Error, Equatable, CustomStringConvertible {
    case notFound(String)
    case ioError(String)
    /// EACCES on a read attempt. Distinct from ioError so callers
    /// can surface a "permission denied" branch (Doctor uses this to
    /// produce `anarlog:files_folders_permission_denied`).
    case permissionDenied(String)

    public var description: String {
        switch self {
        case .notFound(let p): return "filesystem: not found: \(p)"
        case .ioError(let m): return "filesystem: io: \(m)"
        case .permissionDenied(let p): return "filesystem: permission denied: \(p)"
        }
    }
}

public protocol FilesystemAdapter {
    /// `mkdir -p`. Idempotent.
    func createDirectory(at path: String) throws
    /// Copy a file. Throws notFound if source missing.
    func copy(from: String, to: String) throws
    /// Atomic rename (rename(2)). Both paths must be on the same
    /// filesystem.
    func rename(from: String, to: String) throws
    /// Remove a file or directory. Idempotent — no error when absent.
    func remove(at path: String) throws
    /// `chmod +x`.
    func makeExecutable(at path: String) throws
    func fileExists(at path: String) -> Bool
    /// Write Data to a file, replacing any existing content. Used by
    /// the plist + config + state writers.
    func write(_ data: Data, to path: String) throws
    /// Read bytes from a file. Throws notFound if missing.
    func read(from path: String) throws -> Data
    /// List children of a directory (filenames only). Throws
    /// `permissionDenied` on EACCES so the Doctor can distinguish
    /// "path missing" from "path present but unreadable". Default
    /// impl provided for backward-compat with existing test fakes.
    func listDirectory(at path: String) throws -> [String]
}

public extension FilesystemAdapter {
    /// Default impl returns an empty list so existing FilesystemAdapter
    /// conformers (in-memory installer fakes, etc.) don't break. The
    /// production impl + Doctor's anarlog probes use the real
    /// implementation; the installer doesn't touch listDirectory at all.
    func listDirectory(at path: String) throws -> [String] {
        _ = path
        return []
    }
}
