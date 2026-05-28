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

// MARK: - FirstSuccessLatch wiring

extension HeartbeatLoopTests {

    /// Helper to wait for a fire-and-forget Task spawned inside
    /// the heartbeat tick to settle. Without an explicit await
    /// the test's assertion can race the spawned Task.
    private func yieldUntilLatchFires(_ latch: FirstSuccessLatch,
                                      maxIterations: Int = 100) async {
        for _ in 0..<maxIterations {
            if await latch.hasFired() { return }
            await Task.yield()
            try? await Task.sleep(nanoseconds: 1_000_000)  // 1ms
        }
    }

    func testFirstSuccessLatchFiresOnce() async throws {
        let writer = RecordingHeartbeatStateWriter()
        let exitHandler = CapturingExitHandler()
        let client = PiClient(
            baseURL: URL(string: "https://pi.example.test")!,
            transport: LifecycleMockTransport([
                .respond(status: 200, data: heartbeat200JSON),
                .respond(status: 200, data: heartbeat200JSON),
                .respond(status: 200, data: heartbeat200JSON),
            ]).asTransport(),
            sleep: noopSleep)
        let counter = LatchCallCounter()
        let latch = FirstSuccessLatch { await counter.bump() }
        let loop = HeartbeatLoop(
            piClient: client,
            auth: auth,
            stateWriter: writer,
            exitHandler: exitHandler,
            logger: NoopLogger(),
            clock: FixedClock(),
            firstSuccessLatch: latch)
        try await loop.tick()
        await yieldUntilLatchFires(latch)
        try await loop.tick()
        try await loop.tick()
        await Task.yield()
        // The latch's callback fires exactly once even though
        // fireOnce() was invoked three times.
        let count = await counter.value
        XCTAssertEqual(count, 1)
    }

    func testFirstSuccessLatchDoesNotFireOn4xx() async throws {
        let writer = RecordingHeartbeatStateWriter()
        let exitHandler = CapturingExitHandler()
        let client = PiClient(
            baseURL: URL(string: "https://pi.example.test")!,
            transport: LifecycleMockTransport([
                .respond(status: 401, data: heartbeat401JSON),
            ]).asTransport(),
            sleep: noopSleep)
        let counter = LatchCallCounter()
        let latch = FirstSuccessLatch { await counter.bump() }
        let loop = HeartbeatLoop(
            piClient: client,
            auth: auth,
            stateWriter: writer,
            exitHandler: exitHandler,
            logger: NoopLogger(),
            clock: FixedClock(),
            firstSuccessLatch: latch)
        do {
            try await loop.tick()
        } catch is ExitRequested {
            // Expected — 401 → requestExit(1).
        }
        await Task.yield()
        let count = await counter.value
        XCTAssertEqual(count, 0, "latch must not fire on 401")
    }

    func testFirstSuccessLatchFiresOnlyAfterTransientThenSuccess() async throws {
        let writer = RecordingHeartbeatStateWriter()
        let exitHandler = CapturingExitHandler()
        // RetryingTransport retries 5xx 5 times (6 total attempts).
        // Tick 1: exhaust all 6 attempts as 503 → transient surfaces,
        //         latch unfired.
        // Tick 2: 200 → latch fires.
        let script: [LifecycleMockTransport.Step] = Array(
            repeating: .respond(status: 503, data: Data("{}".utf8)),
            count: 6) + [
                .respond(status: 200, data: heartbeat200JSON),
            ]
        let client = PiClient(
            baseURL: URL(string: "https://pi.example.test")!,
            transport: LifecycleMockTransport(script).asTransport(),
            sleep: noopSleep)
        let counter = LatchCallCounter()
        let latch = FirstSuccessLatch { await counter.bump() }
        let loop = HeartbeatLoop(
            piClient: client,
            auth: auth,
            stateWriter: writer,
            exitHandler: exitHandler,
            logger: NoopLogger(),
            clock: FixedClock(),
            firstSuccessLatch: latch)
        // First tick: 503 (post-retry exhaustion) → transient — latch unfired.
        try await loop.tick()
        await Task.yield()
        var count = await counter.value
        XCTAssertEqual(count, 0, "503 must not fire latch")
        // Second tick: 200 → latch fires.
        try await loop.tick()
        await yieldUntilLatchFires(latch)
        count = await counter.value
        XCTAssertEqual(count, 1, "200 must fire latch after prior 503")
    }
}

/// Counts how many times the latch's callback was invoked. Used
/// to assert the latch fires exactly once.
actor LatchCallCounter {
    private(set) var value: Int = 0
    func bump() { value += 1 }
}
