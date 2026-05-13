import XCTest
@testable import CRMMacCore

final class StateStoreTests: XCTestCase {
    private var tempDir: URL!
    private var stateURL: URL!

    override func setUpWithError() throws {
        try super.setUpWithError()
        tempDir = FileManager.default.temporaryDirectory
            .appendingPathComponent("crm-mac-state-tests-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: tempDir, withIntermediateDirectories: true)
        stateURL = tempDir.appendingPathComponent("state.json")
    }

    override func tearDownWithError() throws {
        try? FileManager.default.removeItem(at: tempDir)
        try super.tearDownWithError()
    }

    func testLoadMissingFileThrowsFileNotFound() {
        let store = StateStore(fileURL: stateURL)
        XCTAssertThrowsError(try store.load()) { error in
            guard case StateStoreError.fileNotFound = error else {
                XCTFail("expected fileNotFound, got \(error)")
                return
            }
        }
    }

    func testSaveThenLoadRoundtrip() throws {
        let store = StateStore(fileURL: stateURL)
        let original = DaemonState(
            schemaVersion: 1,
            hostID: UUID(),
            lastHeartbeatAt: Date(timeIntervalSince1970: 1_700_000_000),
            sources: ["messages": SourceState(cursor: "x", cursorEpoch: 7)])
        try store.save(original)
        let loaded = try store.load()
        XCTAssertEqual(loaded, original)
    }

    func testSaveCreatesParentDirectory() throws {
        let nested = tempDir
            .appendingPathComponent("a/b/c")
            .appendingPathComponent("state.json")
        let store = StateStore(fileURL: nested)
        try store.save(DaemonState())
        XCTAssertTrue(FileManager.default.fileExists(atPath: nested.path))
    }

    func testSaveDoesNotLeaveTempFile() throws {
        let store = StateStore(fileURL: stateURL)
        try store.save(DaemonState())
        let contents = try FileManager.default.contentsOfDirectory(atPath: tempDir.path)
        let tmpFiles = contents.filter { $0.contains(".tmp.") }
        XCTAssertEqual(tmpFiles, [], "atomic-rename should remove tmp file")
    }

    func testLoadDecodeFailureSurfacesAsDecodeError() throws {
        try Data("{not json".utf8).write(to: stateURL)
        let store = StateStore(fileURL: stateURL)
        XCTAssertThrowsError(try store.load()) { error in
            guard case StateStoreError.decode = error else {
                XCTFail("expected decode error, got \(error)")
                return
            }
        }
    }

    func testLoadSchemaMismatchSurfaces() throws {
        let badJSON = """
        {
            "schemaVersion": 99,
            "sources": {}
        }
        """
        try Data(badJSON.utf8).write(to: stateURL)
        let store = StateStore(fileURL: stateURL)
        XCTAssertThrowsError(try store.load()) { error in
            guard case let StateStoreError.schemaMismatch(found, expected) = error else {
                XCTFail("expected schemaMismatch, got \(error)")
                return
            }
            XCTAssertEqual(found, 99)
            XCTAssertEqual(expected, DaemonState.currentSchemaVersion)
        }
    }

    func testInitializeIfMissingWritesOnFirstCall() throws {
        let store = StateStore(fileURL: stateURL)
        let hostID = UUID()
        XCTAssertTrue(try store.initializeIfMissing(hostID: hostID))
        let loaded = try store.load()
        XCTAssertEqual(loaded.hostID, hostID)
    }

    func testInitializeIfMissingNoopWhenPresent() throws {
        let store = StateStore(fileURL: stateURL)
        try store.save(DaemonState(hostID: UUID(uuidString: "deadbeef-dead-beef-dead-beefdeadbeef")))
        XCTAssertFalse(try store.initializeIfMissing(hostID: UUID()))
        // Confirm the prior host ID is preserved.
        let loaded = try store.load()
        XCTAssertEqual(loaded.hostID, UUID(uuidString: "deadbeef-dead-beef-dead-beefdeadbeef"))
    }
}
