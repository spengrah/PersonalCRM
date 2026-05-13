// ProductionFilesystemAdapter wraps Foundation.FileManager + a few
// atomic operations the in-memory fake models. No global state; safe
// to instantiate per process or per call.
import Foundation
import CRMMacLifecycle

public struct ProductionFilesystemAdapter: FilesystemAdapter {
    private let fm: FileManager
    public init(fileManager: FileManager = .default) {
        self.fm = fileManager
    }

    public func createDirectory(at path: String) throws {
        do {
            try fm.createDirectory(
                atPath: path,
                withIntermediateDirectories: true,
                attributes: [.posixPermissions: 0o700])
        } catch {
            throw FilesystemError.ioError("mkdir \(path): \(error.localizedDescription)")
        }
    }

    public func copy(from: String, to: String) throws {
        if !fm.fileExists(atPath: from) {
            throw FilesystemError.notFound(from)
        }
        // FileManager.copyItem fails if destination exists; remove
        // first to match the production install pattern (re-copy on
        // upgrade).
        if fm.fileExists(atPath: to) {
            try? fm.removeItem(atPath: to)
        }
        do {
            try fm.copyItem(atPath: from, toPath: to)
        } catch {
            throw FilesystemError.ioError("copy \(from) -> \(to): \(error.localizedDescription)")
        }
    }

    public func rename(from: String, to: String) throws {
        if !fm.fileExists(atPath: from) {
            throw FilesystemError.notFound(from)
        }
        // Best-effort remove of any pre-existing destination so
        // rename(2) succeeds. Foundation's moveItem will otherwise
        // fail with NSFileWriteFileExistsError.
        if fm.fileExists(atPath: to) {
            try? fm.removeItem(atPath: to)
        }
        do {
            try fm.moveItem(atPath: from, toPath: to)
        } catch {
            throw FilesystemError.ioError("rename \(from) -> \(to): \(error.localizedDescription)")
        }
    }

    public func remove(at path: String) throws {
        if !fm.fileExists(atPath: path) { return }
        do {
            try fm.removeItem(atPath: path)
        } catch {
            throw FilesystemError.ioError("remove \(path): \(error.localizedDescription)")
        }
    }

    public func makeExecutable(at path: String) throws {
        do {
            try fm.setAttributes([.posixPermissions: 0o755], ofItemAtPath: path)
        } catch {
            throw FilesystemError.ioError("chmod \(path): \(error.localizedDescription)")
        }
    }

    public func fileExists(at path: String) -> Bool {
        fm.fileExists(atPath: path)
    }

    public func write(_ data: Data, to path: String) throws {
        let url = URL(fileURLWithPath: path)
        do {
            try data.write(to: url, options: .atomic)
        } catch {
            throw FilesystemError.ioError("write \(path): \(error.localizedDescription)")
        }
    }

    public func read(from path: String) throws -> Data {
        let url = URL(fileURLWithPath: path)
        guard fm.fileExists(atPath: path) else {
            throw FilesystemError.notFound(path)
        }
        do {
            return try Data(contentsOf: url)
        } catch {
            throw FilesystemError.ioError("read \(path): \(error.localizedDescription)")
        }
    }
}
