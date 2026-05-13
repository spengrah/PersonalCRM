import XCTest
import CRMMacCore
@testable import CRMMacLifecycle
@testable import CRMMacPiClient

final class HeartbeatLoopTests: XCTestCase {
    private let auth = PiAuth(
        hostID: UUID(uuidString: "11111111-2222-3333-4444-555555555555")!,
        apiKey: "k")

    func test200RecordsHeartbeatAndContinues() async throws {
        let writer = RecordingHeartbeatStateWriter()
        let exitHandler = CapturingExitHandler()
        let client = PiClient(
            baseURL: URL(string: "https://pi.example.test")!,
            transport: LifecycleMockTransport([.respond(status: 200, data: heartbeat200JSON)]).asTransport(),
            sleep: noopSleep)
        let loop = HeartbeatLoop(
            piClient: client,
            auth: auth,
            stateWriter: writer,
            exitHandler: exitHandler,
            logger: NoopLogger(),
            clock: FixedClock())
        try await loop.tick()
        XCTAssertEqual(writer.records.count, 1)
        XCTAssertEqual(writer.records.first?.cursorEpoch, 1)
        XCTAssertTrue(exitHandler.capturedCodes.isEmpty)
    }

    func test401RequestsExitOne() async {
        let writer = RecordingHeartbeatStateWriter()
        let exitHandler = CapturingExitHandler()
        let client = PiClient(
            baseURL: URL(string: "https://pi.example.test")!,
            transport: LifecycleMockTransport([.respond(status: 401, data: heartbeat401JSON)]).asTransport(),
            sleep: noopSleep)
        let loop = HeartbeatLoop(
            piClient: client,
            auth: auth,
            stateWriter: writer,
            exitHandler: exitHandler,
            logger: NoopLogger(),
            clock: FixedClock())
        do {
            try await loop.tick()
            XCTFail("expected exit thrown")
        } catch let exit as ExitRequested {
            XCTAssertEqual(exit.code, 1)
        } catch {
            XCTFail("got \(error)")
        }
        XCTAssertEqual(exitHandler.capturedCodes, [1])
        XCTAssertTrue(writer.records.isEmpty)
    }

    func test412RequestsExitTwo() async {
        let writer = RecordingHeartbeatStateWriter()
        let exitHandler = CapturingExitHandler()
        let client = PiClient(
            baseURL: URL(string: "https://pi.example.test")!,
            transport: LifecycleMockTransport([.respond(status: 412, data: heartbeat412JSON)]).asTransport(),
            sleep: noopSleep)
        let loop = HeartbeatLoop(
            piClient: client,
            auth: auth,
            stateWriter: writer,
            exitHandler: exitHandler,
            logger: NoopLogger(),
            clock: FixedClock())
        do {
            try await loop.tick()
            XCTFail("expected exit thrown")
        } catch let exit as ExitRequested {
            XCTAssertEqual(exit.code, 2)
        } catch {
            XCTFail("got \(error)")
        }
        XCTAssertEqual(exitHandler.capturedCodes, [2])
    }

    func testTransientErrorContinues() async throws {
        let writer = RecordingHeartbeatStateWriter()
        let exitHandler = CapturingExitHandler()
        // 6 retries worth of 500s then a network failure to trigger
        // serverError surfacing — heartbeat must NOT exit on this.
        let steps: [LifecycleMockTransport.Step] = Array(repeating: .respond(status: 500, data: Data("{}".utf8)), count: 6)
        let client = PiClient(
            baseURL: URL(string: "https://pi.example.test")!,
            transport: LifecycleMockTransport(steps).asTransport(),
            sleep: noopSleep)
        let loop = HeartbeatLoop(
            piClient: client,
            auth: auth,
            stateWriter: writer,
            exitHandler: exitHandler,
            logger: NoopLogger(),
            clock: FixedClock())
        try await loop.tick()
        XCTAssertTrue(writer.records.isEmpty)
        XCTAssertTrue(exitHandler.capturedCodes.isEmpty, "transient errors must not exit")
    }
}

final class RecordingHeartbeatStateWriter: HeartbeatStateWriter {
    struct Record: Equatable {
        let at: Date
        let cursorEpoch: Int64
    }
    private(set) var records: [Record] = []
    func recordSuccessfulHeartbeat(at: Date, cursorEpoch: Int64) {
        records.append(Record(at: at, cursorEpoch: cursorEpoch))
    }
}
