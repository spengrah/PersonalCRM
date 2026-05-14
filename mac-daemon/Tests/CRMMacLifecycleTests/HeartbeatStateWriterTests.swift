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

    func testRecordsLastHeartbeatAt() async throws {
        let store = StateStore(fileURL: stateURL)
        try store.save(DaemonState(schemaVersion: 1))
        let mutator = StateMutator(store: store)
        let writer = OnDiskHeartbeatStateWriter(mutator: mutator, logger: NoopLogger())
        let when = Date(timeIntervalSince1970: 1_700_500_000)
        try await writer.recordSuccessfulHeartbeat(at: when, cursorEpoch: 7)
        let loaded = try store.load()
        XCTAssertEqual(loaded.lastHeartbeatAt, when)
    }

    func testReplaceExistingHeartbeatTimestamp() async throws {
        let store = StateStore(fileURL: stateURL)
        try store.save(DaemonState(
            schemaVersion: 1,
            lastHeartbeatAt: Date(timeIntervalSince1970: 1_700_000_000)))
        let mutator = StateMutator(store: store)
        let writer = OnDiskHeartbeatStateWriter(mutator: mutator, logger: NoopLogger())
        let when = Date(timeIntervalSince1970: 1_700_500_000)
        try await writer.recordSuccessfulHeartbeat(at: when, cursorEpoch: 7)
        let loaded = try store.load()
        XCTAssertEqual(loaded.lastHeartbeatAt, when)
    }

    func testMissingStateLogsAndThrows() async {
        // No state file written. recordSuccessfulHeartbeat must surface
        // the error to the caller so the heartbeat loop knows the state
        // write failed (it remains best-effort at the caller level but
        // the protocol now returns the underlying StateStoreError).
        let store = StateStore(fileURL: stateURL)
        let mutator = StateMutator(store: store)
        let writer = OnDiskHeartbeatStateWriter(mutator: mutator, logger: NoopLogger())
        do {
            try await writer.recordSuccessfulHeartbeat(at: Date(), cursorEpoch: 0)
            XCTFail("expected throw when state file is missing")
        } catch {
            // Expected — StateStoreError.missing or similar.
        }
    }
}
