// FilesystemAdapter wraps the filesystem operations the installer +
// uninstaller need. Most callers can stick to Foundation — this
// protocol exists so the installer's branchy logic can be exercised
// against an in-memory filesystem without touching the dev machine's
// disk.
import Foundation

public enum FilesystemError: Error, Equatable, CustomStringConvertible {
    case notFound(String)
    case ioError(String)

    public var description: String {
        switch self {
        case .notFound(let p): return "filesystem: not found: \(p)"
        case .ioError(let m): return "filesystem: io: \(m)"
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
}
