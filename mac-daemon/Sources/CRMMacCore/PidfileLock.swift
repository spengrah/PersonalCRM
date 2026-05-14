// PidfileLock — POSIX advisory-lock wrapper for the daemon's runtime
// directory.
//
// Used to:
//   1. Refuse a second daemon startup if one is already running
//      (defense-in-depth; launchd is the primary gate).
//   2. Refuse `crm-mac messages backfill --restart` / `messages scan`
//      while the daemon is up (the daemon would re-fetch the Pi-side
//      cursor and clobber the operator's edits).
//
// Mechanism: open the pidfile with O_CREAT|O_RDWR, acquire an
// exclusive lock via flock(LOCK_EX | LOCK_NB). On EWOULDBLOCK, throw
// alreadyHeld(byPID:). Lock is released on `release()` or process
// exit (the kernel releases flock'd descriptors on close).
//
// Stale-PID recovery: on acquire(), if the file already exists AND
// the recorded PID does NOT correspond to a running process, the
// existing pidfile is removed and a fresh one is created. Covers the
// "daemon crashed without unlinking pidfile" case.
//
// Cross-process correctness: flock is process-scoped; a second
// process trying to lock the same file gets EWOULDBLOCK. Within a
// single process, two PidfileLock instances using the same path
// will collide — the second acquire fails (POSIX flock on the same
// inode is per-fd, but our impl explicitly checks the recorded PID
// first).
//
// References: flock(2), fcntl(2), open(2). On Darwin, flock and
// fcntl(F_SETLK) both work; we use flock for simplicity.
import Foundation
import Darwin

public enum PidfileError: Error, Equatable, Sendable, CustomStringConvertible {
    case alreadyHeld(byPID: pid_t)
    case openFailed(path: String, errno: Int32)
    case lockFailed(path: String, errno: Int32)
    case writeFailed(path: String, errno: Int32)
    case removeFailed(path: String, errno: Int32)

    public var description: String {
        switch self {
        case .alreadyHeld(let pid):
            return "pidfile already held by PID \(pid)"
        case .openFailed(let path, let err):
            return "open(\(path)) failed: errno=\(err) (\(strerror(err)))"
        case .lockFailed(let path, let err):
            return "flock(\(path)) failed: errno=\(err) (\(strerror(err)))"
        case .writeFailed(let path, let err):
            return "write(\(path)) failed: errno=\(err) (\(strerror(err)))"
        case .removeFailed(let path, let err):
            return "remove(\(path)) failed: errno=\(err) (\(strerror(err)))"
        }
    }

    private func strerror(_ err: Int32) -> String {
        if let cs = Darwin.strerror(err) {
            return String(cString: cs)
        }
        return "?"
    }
}

public final class PidfileLock: @unchecked Sendable {
    private let path: String
    private let lockQueue = DispatchQueue(label: "crm-mac.pidfile-lock")
    private var fd: Int32 = -1

    public init(path: URL) {
        self.path = path.path
    }

    /// Acquire an exclusive lock on the pidfile. On success, writes
    /// the current PID into the file (defense — the file's contents
    /// are advisory; the lock itself is the authority).
    ///
    /// If the pidfile exists and the recorded PID still has a live
    /// process, throws `.alreadyHeld(byPID:)`. If the PID is stale
    /// (process gone), the existing pidfile is reused (we'll
    /// overwrite the PID after taking the lock).
    public func acquire() throws {
        try lockQueue.sync {
            try acquireUnsafe()
        }
    }

    /// Release the lock (close the fd and remove the pidfile).
    /// Idempotent. Safe to call on a never-acquired lock.
    public func release() {
        lockQueue.sync {
            releaseUnsafe()
        }
    }

    // MARK: - implementation

    private func acquireUnsafe() throws {
        // Make sure the parent directory exists. The caller (DaemonRunner /
        // CLI ops) is responsible for choosing a path under the daemon
        // runtime dir; create the directory if missing.
        let dir = (path as NSString).deletingLastPathComponent
        try FileManager.default.createDirectory(
            atPath: dir,
            withIntermediateDirectories: true)

        // Stale-PID pre-check: if the file exists and contains a
        // numeric PID for a non-running process, remove it.
        if let stalePID = readStalePID() {
            // The PID is stale; remove the old file so we can recreate
            // it cleanly. If remove fails, we'll still attempt to open
            // and lock — flock works on the existing file too.
            _ = unlink(path)
            _ = stalePID // unused outside the read; logged by caller
        }

        let openFlags = O_CREAT | O_RDWR
        let mode: mode_t = 0o644
        let openedFD = open(path, openFlags, mode)
        if openedFD < 0 {
            throw PidfileError.openFailed(path: path, errno: errno)
        }

        // Take an exclusive non-blocking advisory lock.
        let lockResult = flock(openedFD, LOCK_EX | LOCK_NB)
        if lockResult != 0 {
            let err = errno
            close(openedFD)
            if err == EWOULDBLOCK {
                // Read the current PID from the file (if any) for the
                // error message.
                let pid = readCurrentPIDOnLockFailure()
                throw PidfileError.alreadyHeld(byPID: pid)
            }
            throw PidfileError.lockFailed(path: path, errno: err)
        }

        // Truncate and write our PID.
        if ftruncate(openedFD, 0) != 0 {
            let err = errno
            close(openedFD)
            throw PidfileError.writeFailed(path: path, errno: err)
        }
        let pidString = "\(getpid())\n"
        let bytes = Array(pidString.utf8)
        let written = bytes.withUnsafeBufferPointer { buf in
            write(openedFD, buf.baseAddress, buf.count)
        }
        if written != bytes.count {
            let err = errno
            close(openedFD)
            throw PidfileError.writeFailed(path: path, errno: err)
        }
        self.fd = openedFD
    }

    private func releaseUnsafe() {
        if fd < 0 { return }
        // Best-effort unlink BEFORE close; flock is released on close.
        // Order is critical: if we close first, another process could
        // race in between close and unlink and the file would persist.
        _ = unlink(path)
        // Release the lock + close.
        _ = flock(fd, LOCK_UN)
        close(fd)
        fd = -1
    }

    /// Read the recorded PID from the pidfile; return nil if the file
    /// doesn't exist or the contents aren't a numeric PID.
    private func readPID() -> pid_t? {
        guard let data = try? Data(contentsOf: URL(fileURLWithPath: path)) else {
            return nil
        }
        let s = String(decoding: data, as: UTF8.self)
            .trimmingCharacters(in: .whitespacesAndNewlines)
        return pid_t(s)
    }

    /// Returns the stale PID if the pidfile exists, contains a PID,
    /// and that PID does NOT correspond to a running process. Returns
    /// nil if the file is missing, malformed, or owned by a live
    /// process.
    private func readStalePID() -> pid_t? {
        guard let pid = readPID(), pid > 0 else { return nil }
        // kill(pid, 0) returns 0 if the process exists and we have
        // permission; -1 with errno=ESRCH if the process is gone.
        if kill(pid, 0) == 0 {
            return nil // live
        }
        if errno == ESRCH {
            return pid // stale
        }
        // EPERM: process exists, owned by another user (rare for our
        // user-context daemon). Treat as live to be safe.
        return nil
    }

    /// Read the PID currently in the pidfile, for lock-failure error
    /// reporting. Returns -1 if unknown.
    private func readCurrentPIDOnLockFailure() -> pid_t {
        readPID() ?? -1
    }
}
