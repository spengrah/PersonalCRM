// ProcessSignaller stops a running daemon process: send SIGTERM,
// then poll the daemon's pidfile until the flock releases (or until
// a timeout elapses). Used by Installer (upgrade path) and Uninstaller
// — SMAppService.unregister tells launchd "don't restart on next
// boot" but does NOT terminate an already-running process.
//
// The pidfile-release primitive matches the daemon's own
// `PidfileLock.acquire()` flock(LOCK_EX | LOCK_NB) call in
// `mac-daemon/Sources/CRMMacCore/PidfileLock.swift`. We open the
// pidfile and attempt the same lock; if it succeeds the daemon has
// exited and we release immediately. If it would block, the daemon
// is still holding it.
//
// This protocol lives in CRMMacLifecycle so the workflows that
// orchestrate stop-the-daemon are testable with the FakeProcessSignaller.
// Production impl `ProductionProcessSignaller` is in CRMMacSystem
// (the POSIX kill/flock calls).
import Foundation

public enum ProcessSignallerError: Error, CustomStringConvertible {
    /// `kill(pid, SIGTERM)` returned non-zero with an errno other than
    /// ESRCH (no-such-process — benign, the daemon already exited).
    case killFailed(errno: Int32)

    public var description: String {
        switch self {
        case .killFailed(let e):
            return "kill(SIGTERM) failed: errno=\(e)"
        }
    }
}

public protocol ProcessSignaller {
    /// Send SIGTERM to the given pid. Errors other than ESRCH (no
    /// such process — benign, daemon already exited) throw.
    func sendSIGTERM(pid: pid_t) throws

    /// Wait until the pidfile at `path` is no longer locked, or until
    /// `timeoutSeconds` elapses. Returns true when the lock was
    /// acquirable (daemon exited or pidfile absent); false on
    /// timeout (daemon still running after `timeoutSeconds`).
    ///
    /// Implementation contract (production): poll every 200ms by
    /// opening the pidfile and attempting `flock(fd, LOCK_EX |
    /// LOCK_NB)`. On success, release immediately + return true.
    /// On EWOULDBLOCK, sleep + retry. ENOENT (pidfile absent) is
    /// treated as released (returns true immediately).
    func waitForPidfileRelease(path: String, timeoutSeconds: TimeInterval) async -> Bool
}
