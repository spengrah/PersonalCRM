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

    func testLastKnownPiProtocolVersionRoundTrip() throws {
        // Persisted field — phone_calls source plugin reads via
        // HeartbeatStateProvider to feature-gate against older Pi
        // instances.
        let original = DaemonState(lastKnownPiProtocolVersion: 2)
        let data = try JSONEncoder().encode(original)
        let decoded = try JSONDecoder().decode(DaemonState.self, from: data)
        XCTAssertEqual(decoded.lastKnownPiProtocolVersion, 2)
    }

    func testLastKnownPiProtocolVersionDecodesNilFromOlderState() throws {
        // Older state.json (written before this field existed) MUST
        // still decode. The field is optional and defaults to nil.
        let oldJSON = """
            {
                "schemaVersion": 1,
                "sources": {}
            }
            """
        let decoded = try JSONDecoder().decode(DaemonState.self,
                                               from: Data(oldJSON.utf8))
        XCTAssertNil(decoded.lastKnownPiProtocolVersion)
    }

    func testPendingOrphanNotificationsDefaultsEmptyFromLegacyState() throws {
        // Older state.json (written before pendingOrphanNotifications
        // landed) must still decode and yield an empty slice.
        let oldJSON = """
            {
                "schemaVersion": 1,
                "sources": {}
            }
            """
        let decoded = try JSONDecoder().decode(DaemonState.self,
                                               from: Data(oldJSON.utf8))
        XCTAssertTrue(decoded.pendingOrphanNotifications.isEmpty)
        XCTAssertEqual(decoded.notificationMutationSequence, 0)
    }

    func testPendingOrphanNotificationsRoundTrip() throws {
        let now = Date(timeIntervalSince1970: 1_716_000_000)
        let entry = PendingOrphanNotification(
            sessionUUID: "deadbeef-1111-2222-3333-444455556666",
            reason: "orphan",
            notifiedAt: now,
            deliveryState: "queued",
            mutationSequence: 42)
        let original = DaemonState(
            pendingOrphanNotifications: [entry],
            notificationMutationSequence: 42)
        let encoder = JSONEncoder()
        encoder.dateEncodingStrategy = .iso8601
        let data = try encoder.encode(original)
        let decoder = JSONDecoder()
        decoder.dateDecodingStrategy = .iso8601
        let decoded = try decoder.decode(DaemonState.self, from: data)
        XCTAssertEqual(decoded, original)
    }

    func testPendingOrphanNotificationLegacyEntryDefaultsToQueued() throws {
        // An entry persisted before the deliveryState +
        // mutationSequence fields landed must decode with sensible
        // defaults: deliveryState="queued" (conservative; assume the
        // raise succeeded), mutationSequence=0 (sorts earliest).
        let legacyJSON = """
            {
                "sessionUUID": "deadbeef-1111-2222-3333-444455556666",
                "reason": "orphan",
                "notifiedAt": "2026-05-27T14:00:00Z"
            }
            """
        let decoder = JSONDecoder()
        decoder.dateDecodingStrategy = .iso8601
        let decoded = try decoder.decode(PendingOrphanNotification.self,
                                         from: Data(legacyJSON.utf8))
        XCTAssertEqual(decoded.deliveryState, "queued")
        XCTAssertEqual(decoded.mutationSequence, 0)
    }
}
