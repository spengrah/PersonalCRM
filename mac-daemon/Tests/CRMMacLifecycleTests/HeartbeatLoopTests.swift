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
        let records = await writer.records
        XCTAssertEqual(records.count, 1)
        XCTAssertEqual(records.first?.cursorEpoch, 1)
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
        let records = await writer.records
        XCTAssertTrue(records.isEmpty)
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
        let records = await writer.records
        XCTAssertTrue(records.isEmpty)
        XCTAssertTrue(exitHandler.capturedCodes.isEmpty, "transient errors must not exit")
    }
}

/// Test double for HeartbeatStateWriter that records each call.
///
/// `actor` because `HeartbeatStateWriter` requires `Sendable` and the
/// stored `records` array is mutable across the async call. Callers
/// read `await writer.records` to inspect captured invocations.
actor RecordingHeartbeatStateWriter: HeartbeatStateWriter {
    struct Record: Equatable {
        let at: Date
        let cursorEpoch: Int64
        let protocolVersion: Int32
    }
    private(set) var records: [Record] = []
    func recordSuccessfulHeartbeat(
        at: Date,
        cursorEpoch: Int64,
        protocolVersion: Int32
    ) async throws {
        records.append(Record(at: at, cursorEpoch: cursorEpoch, protocolVersion: protocolVersion))
    }
}
