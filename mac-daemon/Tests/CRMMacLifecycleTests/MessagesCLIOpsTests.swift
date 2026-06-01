// MessagesCLIOpsTests — focused tests on the cursor-mutation logic
// (CAS commit, pidfile guard, identifier normalization).
//
// Tests pass through real PidfileLock + a URLProtocol-mocked PiClient
// + a fake stdin reader.
import XCTest
import Foundation
import CRMMacCore
@testable import CRMMacLifecycle
@testable import CRMMacPiClient

final class MessagesCLIOpsTests: XCTestCase {
    private let auth = PiAuth(
        hostID: UUID(uuidString: "11111111-2222-3333-4444-555555555555")!,
        apiKey: "k")
    private let backfillFloor = MessagesCursorWire.defaultBackfillFloor
    private var tempDir: URL!
    private var pidfileURL: URL!

    override func setUpWithError() throws {
        try super.setUpWithError()
        tempDir = FileManager.default.temporaryDirectory
            .appendingPathComponent("crm-mac-cliops-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: tempDir, withIntermediateDirectories: true)
        pidfileURL = tempDir.appendingPathComponent("daemon.pid")
    }

    override func tearDownWithError() throws {
        try? FileManager.default.removeItem(at: tempDir)
        try super.tearDownWithError()
    }

    final class ScriptedReader: MessagesOpsStdinReader, @unchecked Sendable {
        var response: String?
        func readLine() -> String? { response }
    }

    /// Build a PiClient that returns the supplied scripted responses
    /// in order. Each invocation consumes one step.
    private func client(_ steps: [LifecycleMockTransport.Step]) -> PiClient {
        let transport = LifecycleMockTransport(steps).asTransport()
        return PiClient(baseURL: URL(string: "https://pi.example.test")!,
                        transport: transport, sleep: noopSleep)
    }

    /// Like `client(_:)` but returns the transport too, so a test can
    /// inspect the committed cursor body.
    private func clientWithTransport(
        _ steps: [LifecycleMockTransport.Step]
    ) -> (PiClient, LifecycleMockTransport) {
        let transport = LifecycleMockTransport(steps)
        let pi = PiClient(baseURL: URL(string: "https://pi.example.test")!,
                          transport: transport.asTransport(), sleep: noopSleep)
        return (pi, transport)
    }

    /// A GET-cursor response carrying `cursor` verbatim.
    private func getCursorResponse(_ cursor: String, epoch: Int64 = 7) -> Data {
        let encoded = String(decoding: try! JSONEncoder().encode(cursor), as: UTF8.self)
        return Data("""
            {"success":true,"data":{"cursor":\(encoded),"cursor_epoch":\(epoch),"backfill_complete":false}}
            """.utf8)
    }

    /// Decode the committed cursor from the LAST POST /cursor invocation.
    private func committedCursor(_ transport: LifecycleMockTransport) throws -> MessagesCursorWire? {
        let posts = transport.invocations.filter {
            $0.httpMethod == "POST" && ($0.url?.path.hasSuffix("/cursor") ?? false)
        }
        guard let last = posts.last, let body = last.httpBody,
              let obj = try JSONSerialization.jsonObject(with: body) as? [String: Any],
              let cursorStr = obj["cursor"] as? String else {
            return nil
        }
        return try MessagesCursorWireCodec.decode(cursorStr)
    }

    // MARK: - backfill --restart

    func testBackfillRestartHappyPathWithYes() async throws {
        let getCursorJSON = Data(#"""
            {"success":true,
             "data":{"cursor":"{\"backfill_floor_sent_at\":\"2026-01-01T00:00:00Z\"}",
                     "cursor_epoch":7,
                     "backfill_complete":false}}
            """#.utf8)
        let commitJSON = Data(#"{"success":true,"data":{"ok":true}}"#.utf8)
        let piClient = client([
            .respond(status: 200, data: getCursorJSON),
            .respond(status: 200, data: commitJSON),
        ])
        let ops = MessagesOps(
            piClient: piClient,
            auth: auth,
            pidfileLock: PidfileLock(path: pidfileURL),
            logger: NoopLogger(),
            backfillFloor: backfillFloor)
        try await ops.backfillRestart(yes: true)
    }

    func testBackfillRestartDeclinedByUser() async throws {
        let reader = ScriptedReader()
        reader.response = "no"
        let ops = MessagesOps(
            piClient: client([]),
            auth: auth,
            pidfileLock: PidfileLock(path: pidfileURL),
            logger: NoopLogger(),
            backfillFloor: backfillFloor,
            stdin: reader)
        do {
            try await ops.backfillRestart(yes: false)
            XCTFail("expected userDeclined")
        } catch MessagesOpsError.userDeclined {
            // OK
        } catch {
            XCTFail("got \(error)")
        }
    }

    func testBackfillRestartRefusedWhenDaemonRunning() async throws {
        // Hold the pidfile lock to simulate the daemon being up.
        let daemonLock = PidfileLock(path: pidfileURL)
        try daemonLock.acquire()
        defer { daemonLock.release() }

        let ops = MessagesOps(
            piClient: client([]),
            auth: auth,
            pidfileLock: PidfileLock(path: pidfileURL),
            logger: NoopLogger(),
            backfillFloor: backfillFloor)
        do {
            try await ops.backfillRestart(yes: true)
            XCTFail("expected daemonRunning")
        } catch let MessagesOpsError.daemonRunning(pid) {
            XCTAssertEqual(pid, getpid())
        } catch {
            XCTFail("got \(error)")
        }
    }

    func testBackfillRestartConflictPropagates() async throws {
        let getCursorJSON = Data(#"{"success":true,"data":{"cursor":"","cursor_epoch":7,"backfill_complete":false}}"#.utf8)
        let conflictJSON = Data(#"""
            {"success":false,
             "error":{"code":"EPOCH_MISMATCH","message":"epoch mismatch"},
             "data":{"current_cursor":"x","current_epoch":9}}
            """#.utf8)
        let piClient = client([
            .respond(status: 200, data: getCursorJSON),
            .respond(status: 409, data: conflictJSON),
        ])
        let ops = MessagesOps(
            piClient: piClient,
            auth: auth,
            pidfileLock: PidfileLock(path: pidfileURL),
            logger: NoopLogger(),
            backfillFloor: backfillFloor)
        do {
            try await ops.backfillRestart(yes: true)
            XCTFail("expected cursorConflict")
        } catch MessagesOpsError.cursorConflict {
            // OK
        } catch {
            XCTFail("got \(error)")
        }
    }

    // MARK: - scan

    private func makeOps(_ piClient: PiClient) -> MessagesOps {
        MessagesOps(
            piClient: piClient,
            auth: auth,
            pidfileLock: PidfileLock(path: pidfileURL),
            logger: NoopLogger(),
            backfillFloor: backfillFloor)
    }

    private let emptyCursorJSON = "{\"backfill_floor_sent_at\":\"2026-01-01T00:00:00Z\"}"

    func testScanAppendsPendingScan() async throws {
        let (pi, transport) = clientWithTransport([
            .respond(status: 200, data: getCursorResponse(emptyCursorJSON)),
            .respond(status: 200, data: Data(#"{"success":true,"data":{"ok":true}}"#.utf8)),
        ])
        try await makeOps(pi).scan(identifier: "+1-555-123-4567", since: 30 * 86400)

        let committed = try committedCursor(transport)
        XCTAssertEqual(committed?.pendingScans.count, 1)
        XCTAssertEqual(committed?.pendingScans.first?.normalizedHandle, "+15551234567")
    }

    func testScanClampsOverWideSinceToFloor() async throws {
        // A 100-year --since would reach far below the 2026-01-01 floor;
        // the queued entry's `since` must be clamped to the floor.
        let (pi, transport) = clientWithTransport([
            .respond(status: 200, data: getCursorResponse(emptyCursorJSON)),
            .respond(status: 200, data: Data(#"{"success":true,"data":{"ok":true}}"#.utf8)),
        ])
        try await makeOps(pi).scan(identifier: "+15551234567", since: 100 * 365 * 86400)

        let committed = try committedCursor(transport)
        XCTAssertEqual(committed?.pendingScans.first?.since, backfillFloor,
                       "over-wide --since clamped to the backfill floor")
    }

    func testScanCoverageDedupWidensAndResetsProgress() async throws {
        // Seed a NARROW entry (since = now − 1 day) with progress
        // advanced; a wider --since must widen + reset progress.
        let narrowSince = Date().addingTimeInterval(-86400)
        var seeded = MessagesCursorWire(backfillFloorSentAt: backfillFloor)
        seeded.pendingScans.append(MessagesCursorPendingScan(
            normalizedHandle: "+15551234567", since: narrowSince, progressBelowRowID: 42))
        let (pi, transport) = clientWithTransport([
            .respond(status: 200, data: getCursorResponse(try MessagesCursorWireCodec.encode(seeded))),
            .respond(status: 200, data: Data(#"{"success":true,"data":{"ok":true}}"#.utf8)),
        ])
        try await makeOps(pi).scan(identifier: "+15551234567", since: 60 * 86400)

        let committed = try committedCursor(transport)
        XCTAssertEqual(committed?.pendingScans.count, 1, "one merged entry, no duplicate")
        let entry = try XCTUnwrap(committed?.pendingScans.first)
        XCTAssertLessThan(entry.since, narrowSince, "window widened to the earlier since")
        XCTAssertNil(entry.progressBelowRowID, "wider window resets progress")
    }

    func testScanCoverageDedupNarrowerPreservesExistingProgress() async throws {
        // Seed a WIDE entry (since at the floor) with progress advanced;
        // a narrower --since must NOT shrink the window or reset progress.
        var seeded = MessagesCursorWire(backfillFloorSentAt: backfillFloor)
        seeded.pendingScans.append(MessagesCursorPendingScan(
            normalizedHandle: "+15551234567", since: backfillFloor, progressBelowRowID: 42))
        let (pi, transport) = clientWithTransport([
            .respond(status: 200, data: getCursorResponse(try MessagesCursorWireCodec.encode(seeded))),
            .respond(status: 200, data: Data(#"{"success":true,"data":{"ok":true}}"#.utf8)),
        ])
        try await makeOps(pi).scan(identifier: "+15551234567", since: 2 * 86400)

        let committed = try committedCursor(transport)
        XCTAssertEqual(committed?.pendingScans.count, 1, "one entry, no duplicate")
        let entry = try XCTUnwrap(committed?.pendingScans.first)
        XCTAssertEqual(entry.since, backfillFloor, "narrower window does not shrink the entry")
        XCTAssertEqual(entry.progressBelowRowID, 42, "existing progress preserved")
    }

    func testScanInvalidIdentifier() async throws {
        let ops = MessagesOps(
            piClient: client([]),
            auth: auth,
            pidfileLock: PidfileLock(path: pidfileURL),
            logger: NoopLogger(),
            backfillFloor: backfillFloor)
        do {
            try await ops.scan(identifier: "   ", since: 86400)
            XCTFail("expected invalidIdentifier")
        } catch MessagesOpsError.invalidIdentifier {
            // OK
        } catch {
            XCTFail("got \(error)")
        }
    }
}
