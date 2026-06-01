// CallHistoryOpsScanTests — focused tests on the call-history scan
// cursor-mutation logic (CAS commit, pidfile guard, identifier
// normalization, cap), mirroring MessagesCLIOpsTests' scan coverage on
// the phone_calls source.
//
// Tests pass through a real PidfileLock + a URLProtocol-mocked PiClient.
//
// Synthetic handles only (+15550000001); no real PII.
import XCTest
import Foundation
import CRMMacCore
@testable import CRMMacLifecycle
@testable import CRMMacPiClient

final class CallHistoryOpsScanTests: XCTestCase {
    private let auth = PiAuth(
        hostID: UUID(uuidString: "11111111-2222-3333-4444-555555555555")!,
        apiKey: "k")
    private let backfillFloor = PhoneCallsCursorWire.defaultBackfillFloor
    private var tempDir: URL!
    private var pidfileURL: URL!

    override func setUpWithError() throws {
        try super.setUpWithError()
        tempDir = FileManager.default.temporaryDirectory
            .appendingPathComponent("crm-mac-callhistory-cliops-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: tempDir, withIntermediateDirectories: true)
        pidfileURL = tempDir.appendingPathComponent("daemon.pid")
    }

    override func tearDownWithError() throws {
        try? FileManager.default.removeItem(at: tempDir)
        try super.tearDownWithError()
    }

    private func makeOps(_ transport: LifecycleMockTransport) -> CallHistoryOps {
        let piClient = PiClient(baseURL: URL(string: "https://pi.example.test")!,
                                transport: transport.asTransport(), sleep: noopSleep)
        return CallHistoryOps(
            piClient: piClient,
            auth: auth,
            pidfileLock: PidfileLock(path: pidfileURL),
            logger: NoopLogger(),
            backfillFloor: backfillFloor)
    }

    /// A GET-cursor response carrying `cursor` verbatim.
    private func getCursorResponse(_ cursor: String, epoch: Int64 = 7) -> Data {
        let encoded = String(decoding: try! JSONEncoder().encode(cursor), as: UTF8.self)
        return Data("""
            {"success":true,"data":{"cursor":\(encoded),"cursor_epoch":\(epoch),"backfill_complete":false}}
            """.utf8)
    }

    /// Decode the committed cursor from the LAST POST /cursor invocation.
    private func committedCursor(_ transport: LifecycleMockTransport) throws -> PhoneCallsCursorWire? {
        let posts = transport.invocations.filter {
            $0.httpMethod == "POST" && ($0.url?.path.hasSuffix("/cursor") ?? false)
        }
        guard let last = posts.last, let body = last.httpBody,
              let obj = try JSONSerialization.jsonObject(with: body) as? [String: Any],
              let cursorStr = obj["cursor"] as? String else {
            return nil
        }
        return try PhoneCallsCursorWireCodec.decode(cursorStr)
    }

    // MARK: - happy path

    func testScanAppendsPendingScan() async throws {
        let emptyCursor = "{\"backfill_floor_sent_at\":\"2026-01-01T00:00:00Z\"}"
        let transport = LifecycleMockTransport([
            .respond(status: 200, data: getCursorResponse(emptyCursor)),
            .respond(status: 200, data: Data(#"{"success":true,"data":{"ok":true}}"#.utf8)),
        ])
        let ops = makeOps(transport)
        try await ops.scan(identifier: "+1-555-000-0001", since: 30 * 86400)

        let committed = try committedCursor(transport)
        XCTAssertEqual(committed?.pendingScans.count, 1)
        XCTAssertEqual(committed?.pendingScans.first?.normalizedHandle, "+15550000001")
    }

    // MARK: - invalid identifier

    func testScanInvalidIdentifier() async throws {
        let ops = makeOps(LifecycleMockTransport([]))
        do {
            try await ops.scan(identifier: "   ", since: 86400)
            XCTFail("expected invalidIdentifier")
        } catch CallHistoryOpsError.invalidIdentifier {
            // OK
        } catch {
            XCTFail("got \(error)")
        }
    }

    // MARK: - daemon-running guard

    func testScanRefusedWhenDaemonRunning() async throws {
        // Hold the pidfile lock to simulate the daemon being up.
        let daemonLock = PidfileLock(path: pidfileURL)
        try daemonLock.acquire()
        defer { daemonLock.release() }

        let ops = makeOps(LifecycleMockTransport([]))
        do {
            try await ops.scan(identifier: "+15550000001", since: 86400)
            XCTFail("expected daemonRunning")
        } catch let CallHistoryOpsError.daemonRunning(pid) {
            XCTAssertEqual(pid, getpid())
        } catch {
            XCTFail("got \(error)")
        }
    }

    // MARK: - cursor conflict

    func testScanConflictPropagates() async throws {
        let emptyCursor = "{\"backfill_floor_sent_at\":\"2026-01-01T00:00:00Z\"}"
        let conflictJSON = Data(#"""
            {"success":false,
             "error":{"code":"EPOCH_MISMATCH","message":"epoch mismatch"},
             "data":{"current_cursor":"x","current_epoch":9}}
            """#.utf8)
        let transport = LifecycleMockTransport([
            .respond(status: 200, data: getCursorResponse(emptyCursor)),
            .respond(status: 409, data: conflictJSON),
        ])
        let ops = makeOps(transport)
        do {
            try await ops.scan(identifier: "+15550000001", since: 86400)
            XCTFail("expected cursorConflict")
        } catch CallHistoryOpsError.cursorConflict {
            // OK
        } catch {
            XCTFail("got \(error)")
        }
    }

    // MARK: - Pi unreachable

    func testScanPiUnreachablePropagates() async throws {
        let emptyCursor = "{\"backfill_floor_sent_at\":\"2026-01-01T00:00:00Z\"}"
        let transport = LifecycleMockTransport([
            .respond(status: 200, data: getCursorResponse(emptyCursor)),
            .fail(URLError(.notConnectedToInternet)),
        ])
        let ops = makeOps(transport)
        do {
            try await ops.scan(identifier: "+15550000001", since: 86400)
            XCTFail("expected piUnreachable")
        } catch CallHistoryOpsError.piUnreachable {
            // OK
        } catch {
            XCTFail("got \(error)")
        }
    }

    // MARK: - cap drops oldest

    func testScanCapDropsOldest() async throws {
        // Pre-fill the GET cursor with pendingScansCap entries; the new
        // scan must drop the oldest so the queue stays at cap.
        let cap = PhoneCallsCursorWire.pendingScansCap
        var seeded = PhoneCallsCursorWire(backfillFloorSentAt: backfillFloor)
        for i in 0..<cap {
            seeded.pendingScans.append(PhoneCallsCursorPendingScan(
                normalizedHandle: "+1555000\(String(format: "%04d", i))",
                since: Date(timeIntervalSince1970: 1_700_000_000)))
        }
        let seededJSON = try PhoneCallsCursorWireCodec.encode(seeded)
        let transport = LifecycleMockTransport([
            .respond(status: 200, data: getCursorResponse(seededJSON)),
            .respond(status: 200, data: Data(#"{"success":true,"data":{"ok":true}}"#.utf8)),
        ])
        let ops = makeOps(transport)
        try await ops.scan(identifier: "+15559999999", since: 86400)

        let committed = try committedCursor(transport)
        XCTAssertEqual(committed?.pendingScans.count, cap, "queue stays at cap")
        // The oldest (index 0) was dropped; the new handle is at the tail.
        XCTAssertEqual(committed?.pendingScans.last?.normalizedHandle, "+15559999999")
        XCTAssertFalse(
            committed?.pendingScans.contains { $0.normalizedHandle == "+15550000000" } ?? true,
            "oldest entry dropped")
    }

    // MARK: - floor clamp on an over-wide --since

    func testScanClampsOverWideSinceToFloor() async throws {
        // A 100-year --since would reach far below the 2026-01-01 floor;
        // the queued entry's `since` must be clamped to the floor.
        let emptyCursor = "{\"backfill_floor_sent_at\":\"2026-01-01T00:00:00Z\"}"
        let transport = LifecycleMockTransport([
            .respond(status: 200, data: getCursorResponse(emptyCursor)),
            .respond(status: 200, data: Data(#"{"success":true,"data":{"ok":true}}"#.utf8)),
        ])
        let ops = makeOps(transport)
        try await ops.scan(identifier: "+15550000001", since: 100 * 365 * 86400)

        let committed = try committedCursor(transport)
        XCTAssertEqual(committed?.pendingScans.first?.since, backfillFloor,
                       "over-wide --since clamped to the backfill floor")
    }

    // MARK: - coverage-dedup (wider widens + resets, narrower preserved)

    func testScanCoverageDedupWidensAndResetsProgress() async throws {
        // Seed a NARROW entry (since = now − 1 day) with progress
        // advanced, anchored to the real clock so a wider --since always
        // widens regardless of the absolute test date.
        let narrowSince = Date().addingTimeInterval(-86400) // now − 1 day
        var seeded = PhoneCallsCursorWire(backfillFloorSentAt: backfillFloor)
        seeded.pendingScans.append(PhoneCallsCursorPendingScan(
            normalizedHandle: "+15550000001",
            since: narrowSince,
            progressBelowZDate: 800_000_000,
            progressBelowZPK: 42))
        let seededJSON = try PhoneCallsCursorWireCodec.encode(seeded)
        let transport = LifecycleMockTransport([
            .respond(status: 200, data: getCursorResponse(seededJSON)),
            .respond(status: 200, data: Data(#"{"success":true,"data":{"ok":true}}"#.utf8)),
        ])
        let ops = makeOps(transport)
        // A wider 60-day scan (now − 60d < now − 1d) widens + resets.
        try await ops.scan(identifier: "+15550000001", since: 60 * 86400)

        let committed = try committedCursor(transport)
        XCTAssertEqual(committed?.pendingScans.count, 1, "one merged entry, no duplicate")
        let entry = try XCTUnwrap(committed?.pendingScans.first)
        XCTAssertLessThan(entry.since, narrowSince, "window widened to the earlier since")
        XCTAssertNil(entry.progressBelowZDate, "wider window resets progress")
        XCTAssertNil(entry.progressBelowZPK, "wider window resets progress")
    }

    func testScanCoverageDedupNarrowerPreservesExistingProgress() async throws {
        // Seed a WIDE entry (since at the floor) with progress advanced.
        var seeded = PhoneCallsCursorWire(backfillFloorSentAt: backfillFloor)
        seeded.pendingScans.append(PhoneCallsCursorPendingScan(
            normalizedHandle: "+15550000001",
            since: backfillFloor,
            progressBelowZDate: 800_000_000,
            progressBelowZPK: 42))
        let seededJSON = try PhoneCallsCursorWireCodec.encode(seeded)
        let transport = LifecycleMockTransport([
            .respond(status: 200, data: getCursorResponse(seededJSON)),
            .respond(status: 200, data: Data(#"{"success":true,"data":{"ok":true}}"#.utf8)),
        ])
        let ops = makeOps(transport)
        // A NARROWER 2-day scan must NOT shrink the window or reset
        // progress.
        try await ops.scan(identifier: "+15550000001", since: 2 * 86400)

        let committed = try committedCursor(transport)
        XCTAssertEqual(committed?.pendingScans.count, 1, "one entry, no duplicate")
        let entry = try XCTUnwrap(committed?.pendingScans.first)
        XCTAssertEqual(entry.since, backfillFloor, "narrower window does not shrink the entry")
        XCTAssertEqual(entry.progressBelowZDate, 800_000_000, "existing progress preserved")
        XCTAssertEqual(entry.progressBelowZPK, 42, "existing progress preserved")
    }
}
