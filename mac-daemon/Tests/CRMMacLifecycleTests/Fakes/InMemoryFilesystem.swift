import Foundation
@testable import CRMMacLifecycle

/// String-keyed in-memory filesystem. Models directories as paths
/// ending with a "/" sentinel suffix; files as path -> Data entries.
public final class InMemoryFilesystem: FilesystemAdapter, @unchecked Sendable {
    private var entries: [String: Data] = [:]
    private var dirs: Set<String> = []
    public private(set) var madeExecutable: Set<String> = []
    /// If non-nil, write() throws an ioError with this reason when the
    /// target path matches `failWritesAtPath`. Used to exercise the
    /// plist-write-failure branch of the installer.
    public var failWritesAtPath: String?
    public var failWritesReason: String = "injected failure"
    /// If non-nil, `listDirectory(at:)` throws `.permissionDenied` for
    /// the given path. Used to exercise Doctor's anarlog permission
    /// branch.
    public var permissionDeniedDirs: Set<String> = []

    public init() {}

    public func createDirectory(at path: String) throws {
        dirs.insert(path)
        // Implicitly create parents.
        var current = path
        while let slash = current.lastIndex(of: "/"), slash != current.startIndex {
            current = String(current[..<slash])
            if !current.isEmpty {
                dirs.insert(current)
            }
        }
    }

    public func copy(from: String, to: String) throws {
        guard let data = entries[from] else {
            throw FilesystemError.notFound(from)
        }
        entries[to] = data
    }

    /// Atomic rename. Supports both file paths and directory paths.
    /// A directory rename moves the directory entry AND every entry
    /// (file or sub-directory) whose path begins with the source path
    /// followed by `/`.
    ///
    /// Mimics POSIX `rename(2)` semantics on a NON-EMPTY destination
    /// directory by throwing `FilesystemError.ioError("destination
    /// not empty")` — this catches a class of production mistakes
    /// where the installer accidentally calls rename with a non-empty
    /// destination instead of using the backup-rename-then-swap
    /// pattern. For files, an existing destination is
    /// overwritten (the production adapter uses replaceItemAt for
    /// files, which IS atomic-replace).
    public func rename(from: String, to: String) throws {
        // File case: simple atomic replace.
        if let data = entries[from] {
            entries[to] = data
            entries.removeValue(forKey: from)
            return
        }
        // Directory case.
        guard dirs.contains(from) else {
            throw FilesystemError.notFound(from)
        }
        // Mimic ENOTEMPTY: if the destination directory exists and
        // has any children, refuse. Empty destination directories are
        // tolerated (the production `FileManager.moveItem` to an
        // empty existing dir on the same filesystem behaves similarly
        // under APFS; we keep the fake strict to surface bugs).
        if dirs.contains(to) {
            let prefix = "\(to)/"
            let hasChildFiles = entries.keys.contains(where: { $0.hasPrefix(prefix) })
            let hasChildDirs = dirs.contains(where: { $0 != to && $0.hasPrefix(prefix) })
            if hasChildFiles || hasChildDirs {
                throw FilesystemError.ioError(
                    "destination not empty: \(to)")
            }
        }
        let fromPrefix = "\(from)/"
        // Move all sub-dir entries.
        let movedDirs = dirs.filter { $0.hasPrefix(fromPrefix) }
        for d in movedDirs {
            let suffix = String(d.dropFirst(from.count))  // includes leading "/"
            dirs.insert("\(to)\(suffix)")
            dirs.remove(d)
        }
        // Move all file entries.
        let movedFiles = entries.keys.filter { $0.hasPrefix(fromPrefix) }
        for f in movedFiles {
            let suffix = String(f.dropFirst(from.count))  // includes leading "/"
            entries["\(to)\(suffix)"] = entries[f]
            entries.removeValue(forKey: f)
        }
        // Move the dir entry itself.
        dirs.insert(to)
        dirs.remove(from)
    }

    public func remove(at path: String) throws {
        // Always remove the named entry (file or dir).
        entries.removeValue(forKey: path)
        dirs.remove(path)
        // Recursive on directories: drop every descendant path.
        let prefix = "\(path)/"
        let childFiles = entries.keys.filter { $0.hasPrefix(prefix) }
        for f in childFiles { entries.removeValue(forKey: f) }
        let childDirs = dirs.filter { $0.hasPrefix(prefix) }
        for d in childDirs { dirs.remove(d) }
    }

    public func makeExecutable(at path: String) throws {
        guard entries[path] != nil else {
            throw FilesystemError.notFound(path)
        }
        madeExecutable.insert(path)
    }

    public func fileExists(at path: String) -> Bool {
        entries[path] != nil || dirs.contains(path)
    }

    public func write(_ data: Data, to path: String) throws {
        if let failPath = failWritesAtPath, failPath == path {
            throw FilesystemError.ioError(failWritesReason)
        }
        entries[path] = data
    }

    public func read(from path: String) throws -> Data {
        guard let data = entries[path] else {
            throw FilesystemError.notFound(path)
        }
        return data
    }

    public func listDirectory(at path: String) throws -> [String] {
        if permissionDeniedDirs.contains(path) {
            throw FilesystemError.permissionDenied(path)
        }
        guard dirs.contains(path) else {
            // Treat missing as empty rather than throwing — matches
            // production FileManager behavior on a missing dir (which
            // raises NSFileNoSuchFileError → we wrap to ioError, but
            // the in-memory fake doesn't model the distinction since
            // callers check exists() first).
            return []
        }
        let prefix = path.hasSuffix("/") ? path : path + "/"
        var children: Set<String> = []
        for entry in entries.keys where entry.hasPrefix(prefix) {
            let tail = String(entry.dropFirst(prefix.count))
            if let slash = tail.firstIndex(of: "/") {
                children.insert(String(tail[..<slash]))
            } else {
                children.insert(tail)
            }
        }
        for d in dirs where d.hasPrefix(prefix) && d != path {
            let tail = String(d.dropFirst(prefix.count))
            if let slash = tail.firstIndex(of: "/") {
                children.insert(String(tail[..<slash]))
            } else {
                children.insert(tail)
            }
        }
        return Array(children).sorted()
    }

    public var allPaths: [String] { Array(entries.keys).sorted() }
    public var allDirs: [String] { Array(dirs).sorted() }

    /// Seed a file (e.g., the "currently running" binary at the source
    /// path before install).
    public func seedFile(at path: String, data: Data = Data("binary".utf8)) {
        entries[path] = data
    }
}
