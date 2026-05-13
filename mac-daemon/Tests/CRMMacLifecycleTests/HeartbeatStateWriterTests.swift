import XCTest
import CRMMacCore
@testable import CRMMacLifecycle

final class HeartbeatStateWriterTests: XCTestCase {
    private var tempDir: URL!
    private var stateURL: URL!

    override func setUpWithError() throws {
        try super.setUpWithError()
        tempDir = FileManager.default.temporaryDirectory
            .appendingPathComponent("crm-mac-heartbeat-state-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: tempDir, withIntermediateDirectories: true)
        stateURL = tempDir.appendingPathComponent("state.json")
    }

    override func tearDownWithError() throws {
        try? FileManager.default.removeItem(at: tempDir)
        try super.tearDownWithError()
    }

    func testRecordsLastHeartbeatAt() throws {
        let store = StateStore(fileURL: stateURL)
        try store.save(DaemonState(schemaVersion: 1))
        let writer = OnDiskHeartbeatStateWriter(stateStore: store, logger: NoopLogger())
        let when = Date(timeIntervalSince1970: 1_700_500_000)
        writer.recordSuccessfulHeartbeat(at: when, cursorEpoch: 7)
        let loaded = try store.load()
        XCTAssertEqual(loaded.lastHeartbeatAt, when)
    }

    func testReplaceExistingHeartbeatTimestamp() throws {
        let store = StateStore(fileURL: stateURL)
        try store.save(DaemonState(
            schemaVersion: 1,
            lastHeartbeatAt: Date(timeIntervalSince1970: 1_700_000_000)))
        let writer = OnDiskHeartbeatStateWriter(stateStore: store, logger: NoopLogger())
        let when = Date(timeIntervalSince1970: 1_700_500_000)
        writer.recordSuccessfulHeartbeat(at: when, cursorEpoch: 7)
        let loaded = try store.load()
        XCTAssertEqual(loaded.lastHeartbeatAt, when)
    }

    func testMissingStateLogsButDoesNotThrow() {
        // No state file written. recordSuccessfulHeartbeat should
        // log the failure but not throw (it's best-effort by contract).
        let store = StateStore(fileURL: stateURL)
        let writer = OnDiskHeartbeatStateWriter(stateStore: store, logger: NoopLogger())
        writer.recordSuccessfulHeartbeat(at: Date(), cursorEpoch: 0)
        // No assertion needed — we just verify no crash.
    }
}
