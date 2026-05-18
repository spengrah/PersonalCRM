// ProductionProcessSignaller: POSIX kill + flock primitives (plan
// D24). Lives in CRMMacSystem so the lifecycle target stays
// Foundation-only (Darwin imports + raw fcntl/flock here).
//
// `waitForPidfileRelease` matches the daemon's `PidfileLock.acquire()`
// primitive (mac-daemon/Sources/CRMMacCore/PidfileLock.swift): open
// the pidfile, attempt `flock(LOCK_EX | LOCK_NB)`, release
// immediately on success. If the lock acquire succeeds the daemon
// has exited; if it would block, the daemon is still holding it.
//
// Alternative considered: read pid from the file + kill(pid, 0) to
// test process existence. Rejected because it races with rapid pid
// reuse on long-running systems — the flock-acquire-and-release
// primitive matches what the daemon itself uses to detect stale
// pidfiles.
import Foundation
import Darwin
import CRMMacLifecycle

public struct ProductionProcessSignaller: ProcessSignaller {
    /// Poll interval for `waitForPidfileRelease`. Exposed for tests
    /// in CRMMacSystemTests that want a tighter loop than the
    /// production 200ms cadence.
    public let pollIntervalNs: UInt64

    public init(pollIntervalNs: UInt64 = 200_000_000) {
        self.pollIntervalNs = pollIntervalNs
    }

    public func sendSIGTERM(pid: pid_t) throws {
        let result = kill(pid, SIGTERM)
        if result != 0 {
            let err = errno
            if err == ESRCH {
                // No such process — daemon already exited. Benign.
                return
            }
            throw ProcessSignallerError.killFailed(errno: err)
        }
    }

    public func waitForPidfileRelease(
        path: String,
        timeoutSeconds: TimeInterval
    ) async -> Bool {
        let deadline = Date().addingTimeInterval(timeoutSeconds)
        while Date() < deadline {
            // Open pidfile readonly. If it's gone, the daemon
            // already cleaned up; treat as released.
            let fd = open(path, O_RDONLY)
            if fd < 0 {
                let err = errno
                if err == ENOENT { return true }
                // Other open errors: best-effort treat as still held,
                // sleep + retry until the deadline.
                try? await Task.sleep(nanoseconds: pollIntervalNs)
                continue
            }
            let lockResult = flock(fd, LOCK_EX | LOCK_NB)
            if lockResult == 0 {
                _ = flock(fd, LOCK_UN)
                close(fd)
                return true
            }
            close(fd)
            // EWOULDBLOCK (or any other lock failure) — daemon still
            // holds it; sleep + retry.
            try? await Task.sleep(nanoseconds: pollIntervalNs)
        }
        return false
    }
}
