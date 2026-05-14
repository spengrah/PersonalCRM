import XCTest
import Darwin
@testable import CRMMacCore

final class PidfileLockTests: XCTestCase {
    private var tempDir: URL!
    private var pidfileURL: URL!

    override func setUpWithError() throws {
        try super.setUpWithError()
        tempDir = FileManager.default.temporaryDirectory
            .appendingPathComponent("crm-mac-pidfile-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: tempDir, withIntermediateDirectories: true)
        pidfileURL = tempDir.appendingPathComponent("daemon.pid")
    }

    override func tearDownWithError() throws {
        try? FileManager.default.removeItem(at: tempDir)
        try super.tearDownWithError()
    }

    func testAcquireWritesPID() throws {
        let lock = PidfileLock(path: pidfileURL)
        try lock.acquire()
        defer { lock.release() }
        let contents = try String(contentsOf: pidfileURL, encoding: .utf8)
        XCTAssertEqual(Int(contents.trimmingCharacters(in: .whitespacesAndNewlines)),
                       Int(getpid()))
    }

    func testReleaseRemovesPIDFile() throws {
        let lock = PidfileLock(path: pidfileURL)
        try lock.acquire()
        lock.release()
        XCTAssertFalse(FileManager.default.fileExists(atPath: pidfileURL.path))
    }

    func testReleaseIdempotent() {
        let lock = PidfileLock(path: pidfileURL)
        // Never acquired; release should be a no-op.
        lock.release()
        lock.release()
    }

    func testStalePIDRecovery() throws {
        // Pre-create the pidfile with a PID that does NOT correspond
        // to a running process (use a very high number unlikely to
        // exist).
        try "999999\n".write(to: pidfileURL, atomically: true, encoding: .utf8)
        let lock = PidfileLock(path: pidfileURL)
        try lock.acquire()
        defer { lock.release() }
        let contents = try String(contentsOf: pidfileURL, encoding: .utf8)
        XCTAssertEqual(Int(contents.trimmingCharacters(in: .whitespacesAndNewlines)),
                       Int(getpid()),
                       "stale PID should have been replaced with our PID")
    }

    func testSecondAcquireWithinProcessFails() throws {
        let firstLock = PidfileLock(path: pidfileURL)
        try firstLock.acquire()
        defer { firstLock.release() }

        let secondLock = PidfileLock(path: pidfileURL)
        do {
            try secondLock.acquire()
            XCTFail("second acquire should have thrown")
            secondLock.release()
        } catch let err as PidfileError {
            switch err {
            case .alreadyHeld(let pid):
                XCTAssertEqual(pid, getpid())
            default:
                XCTFail("expected .alreadyHeld, got \(err)")
            }
        }
    }

    func testMalformedPidfileTreatedAsRecoverable() throws {
        try "not-a-number\n".write(to: pidfileURL, atomically: true, encoding: .utf8)
        let lock = PidfileLock(path: pidfileURL)
        // The acquire should succeed — readPID() returns nil so the
        // stale-check is skipped; the open+flock proceeds and our PID
        // overwrites the garbage.
        try lock.acquire()
        defer { lock.release() }
        let contents = try String(contentsOf: pidfileURL, encoding: .utf8)
        XCTAssertEqual(Int(contents.trimmingCharacters(in: .whitespacesAndNewlines)),
                       Int(getpid()))
    }

    func testAcquireCreatesParentDirectory() throws {
        let nestedURL = tempDir.appendingPathComponent("nonexistent/nested/dir/daemon.pid")
        let lock = PidfileLock(path: nestedURL)
        try lock.acquire()
        defer { lock.release() }
        XCTAssertTrue(FileManager.default.fileExists(atPath: nestedURL.path))
    }
}
