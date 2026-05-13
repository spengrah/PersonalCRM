import Foundation
@testable import CRMMacLifecycle

/// String-keyed in-memory filesystem. Models directories as paths
/// ending with a "/" sentinel suffix; files as path -> Data entries.
public final class InMemoryFilesystem: FilesystemAdapter {
    private var entries: [String: Data] = [:]
    private var dirs: Set<String> = []
    public private(set) var madeExecutable: Set<String> = []

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

    public func rename(from: String, to: String) throws {
        guard let data = entries[from] else {
            throw FilesystemError.notFound(from)
        }
        entries[to] = data
        entries.removeValue(forKey: from)
    }

    public func remove(at path: String) throws {
        entries.removeValue(forKey: path)
        dirs.remove(path)
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
        entries[path] = data
    }

    public func read(from path: String) throws -> Data {
        guard let data = entries[path] else {
            throw FilesystemError.notFound(path)
        }
        return data
    }

    public var allPaths: [String] { Array(entries.keys).sorted() }
    public var allDirs: [String] { Array(dirs).sorted() }

    /// Seed a file (e.g., the "currently running" binary at the source
    /// path before install).
    public func seedFile(at path: String, data: Data = Data("binary".utf8)) {
        entries[path] = data
    }
}
