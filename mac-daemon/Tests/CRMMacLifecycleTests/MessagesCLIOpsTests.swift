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

    func testScanAppendsPendingScan() async throws {
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
        try await ops.scan(identifier: "+1-555-123-4567", since: 30 * 86400)
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
