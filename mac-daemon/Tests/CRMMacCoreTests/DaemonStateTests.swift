import XCTest
@testable import CRMMacCore

final class DaemonStateTests: XCTestCase {
    func testRoundTripEmpty() throws {
        let original = DaemonState()
        let data = try JSONEncoder().encode(original)
        let decoder = JSONDecoder()
        let decoded = try decoder.decode(DaemonState.self, from: data)
        XCTAssertEqual(decoded, original)
    }

    func testRoundTripPopulated() throws {
        let now = Date(timeIntervalSince1970: 1_715_000_000)
        let original = DaemonState(
            schemaVersion: 1,
            hostID: UUID(uuidString: "11111111-2222-3333-4444-555555555555"),
            lastHeartbeatAt: now,
            sources: [
                "messages": SourceState(
                    cursor: "abc",
                    cursorEpoch: 5,
                    backfillComplete: true,
                    lastPushedAt: now,
                    lastErrorAt: nil,
                    lastError: nil),
            ])
        let encoder = JSONEncoder()
        encoder.dateEncodingStrategy = .iso8601
        let data = try encoder.encode(original)
        let decoder = JSONDecoder()
        decoder.dateDecodingStrategy = .iso8601
        let decoded = try decoder.decode(DaemonState.self, from: data)
        XCTAssertEqual(decoded, original)
    }

    func testCurrentSchemaVersionIsOne() {
        XCTAssertEqual(DaemonState.currentSchemaVersion, 1)
    }
}
