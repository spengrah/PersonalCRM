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
        // upgrade). Surface removal failures explicitly — otherwise
        // copyItem fails with a cryptic "file already exists" error.
        if fm.fileExists(atPath: to) {
            do {
                try fm.removeItem(atPath: to)
            } catch {
                throw FilesystemError.ioError(
                    "remove pre-existing \(to): \(error.localizedDescription)")
            }
        }
        do {
            try fm.copyItem(atPath: from, toPath: to)
        } catch {
            throw FilesystemError.ioError("copy \(from) -> \(to): \(error.localizedDescription)")
        }
    }

    public func rename(from: String, to: String) throws {
        let src = URL(fileURLWithPath: from)
        if !fm.fileExists(atPath: from) {
            throw FilesystemError.notFound(from)
        }
        let dst = URL(fileURLWithPath: to)
        do {
            if fm.fileExists(atPath: to) {
                // replaceItemAt performs an atomic-rename: on a same-fs
                // target it uses renamex_np(2) (or rename(2)), so a
                // crash mid-call cannot leave both the old and new
                // file simultaneously absent.
                _ = try fm.replaceItemAt(dst, withItemAt: src)
            } else {
                try fm.moveItem(at: src, to: dst)
            }
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

    public func listDirectory(at path: String) throws -> [String] {
        do {
            return try fm.contentsOfDirectory(atPath: path)
        } catch let nsErr as NSError where nsErr.domain == NSCocoaErrorDomain &&
            (nsErr.code == NSFileReadNoPermissionError ||
             nsErr.code == NSFileReadCorruptFileError) {
            throw FilesystemError.permissionDenied(path)
        } catch let posixErr as POSIXError where posixErr.code == .EACCES {
            throw FilesystemError.permissionDenied(path)
        } catch {
            throw FilesystemError.ioError("listDirectory \(path): \(error.localizedDescription)")
        }
    }

    public func isDirectory(at path: String) -> Bool {
        var isDir: ObjCBool = false
        let exists = fm.fileExists(atPath: path, isDirectory: &isDir)
        return exists && isDir.boolValue
    }
}
