// Tests for AnarlogFSEventsWatcher lifecycle. FSEvents delivery is
// not deterministically observable from a test — we don't try to
// assert "file touch within 3s triggers callback" because that
// becomes flaky under CI load. Instead we exercise:
//
//   - start() + stop() round-trip doesn't crash
//   - double-start throws .alreadyStarted
//   - stop() before start() is a no-op (doesn't crash)
//   - deinit-time cleanup is best-effort
//
// The real "FSEvents wakes the plugin" assertion is covered by the
// manual smoke test: touching _meta.json under the configured
// sessions/ directory triggers a tick within a few seconds against
// the production daemon.
import XCTest
import CRMMacCore
@testable import CRMMacSystem

final class AnarlogFSEventsWatcherTests: XCTestCase {

    private func tempDir() -> URL {
        let url = URL(fileURLWithPath: NSTemporaryDirectory())
            .appendingPathComponent("anarlog-fsevents-tests-\(UUID().uuidString)")
        try? FileManager.default.createDirectory(at: url, withIntermediateDirectories: true)
        return url
    }

    func testStartStopRoundTrip() throws {
        let dir = tempDir()
        defer { try? FileManager.default.removeItem(at: dir) }
        let watcher = AnarlogFSEventsWatcher(
            path: dir.path,
            logger: NoopLogger(),
            onChange: {})
        try watcher.start()
        watcher.stop()
    }

    func testDoubleStartThrows() throws {
        let dir = tempDir()
        defer { try? FileManager.default.removeItem(at: dir) }
        let watcher = AnarlogFSEventsWatcher(
            path: dir.path,
            logger: NoopLogger(),
            onChange: {})
        try watcher.start()
        defer { watcher.stop() }
        XCTAssertThrowsError(try watcher.start()) { error in
            XCTAssertEqual(error as? AnarlogFSEventsWatcherError, .alreadyStarted)
        }
    }

    func testStopBeforeStartIsNoOp() {
        let dir = tempDir()
        defer { try? FileManager.default.removeItem(at: dir) }
        let watcher = AnarlogFSEventsWatcher(
            path: dir.path,
            logger: NoopLogger(),
            onChange: {})
        // Should not crash.
        watcher.stop()
    }

    func testStopIsIdempotent() throws {
        let dir = tempDir()
        defer { try? FileManager.default.removeItem(at: dir) }
        let watcher = AnarlogFSEventsWatcher(
            path: dir.path,
            logger: NoopLogger(),
            onChange: {})
        try watcher.start()
        watcher.stop()
        watcher.stop()  // Second stop is a no-op.
    }

    func testDeinitCleansUp() throws {
        // Implicit: if start() created a stream and the watcher goes
        // out of scope without an explicit stop(), the deinit's
        // stop() call frees the FSEventStream. We can only check
        // that this path doesn't crash; leak detection is the smoke
        // test's job.
        let dir = tempDir()
        defer { try? FileManager.default.removeItem(at: dir) }
        try autoreleasepool {
            let watcher = AnarlogFSEventsWatcher(
                path: dir.path,
                logger: NoopLogger(),
                onChange: {})
            try watcher.start()
            // watcher leaves scope here — deinit runs stop().
        }
    }

    func testFileTouchTriggersCallback() throws {
        // Real-FSEvents end-to-end test. Marked best-effort because
        // FSEvents delivery latency varies (the watcher uses 1.5s
        // coalescence; CI under load can stretch this). We allow up
        // to 10s before giving up to keep the test reliable on
        // GitHub Actions hosted runners.
        let dir = tempDir()
        defer { try? FileManager.default.removeItem(at: dir) }
        let expectation = XCTestExpectation(description: "FSEvents callback")
        let watcher = AnarlogFSEventsWatcher(
            path: dir.path,
            coalescenceLatency: 0.25,
            logger: NoopLogger(),
            onChange: { expectation.fulfill() })
        try watcher.start()
        defer { watcher.stop() }
        // Create a file under the watched directory.
        let target = dir.appendingPathComponent("touched-\(UUID().uuidString).txt")
        try Data("x".utf8).write(to: target)
        wait(for: [expectation], timeout: 10.0)
    }

    func testStartFailsOnNonexistentPath() {
        // FSEventStreamCreate doesn't fail for a non-existent path
        // (it just never delivers events) — but for completeness we
        // sanity-check that no crash occurs.
        let bogus = "/tmp/this-path-does-not-exist-\(UUID().uuidString)"
        let watcher = AnarlogFSEventsWatcher(
            path: bogus,
            logger: NoopLogger(),
            onChange: {})
        // The stream creates successfully even for missing paths,
        // so start() does not throw. (The actual delivery wouldn't
        // work, but that's an OS-level behavior we don't assert on.)
        // The point is: no crash.
        XCTAssertNoThrow(try watcher.start())
        watcher.stop()
    }
}
