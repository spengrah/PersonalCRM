// PhoneCallsCursorOrderTests — the live and backfill batches emit
// every row exactly once and advance their cursors correctly when
// Z_PK (insertion) order and ZDATE (call time) order disagree.
//
// iCloud call-history sync writes an iPhone call to the Mac's
// CallHistoryDB hours after it happened, so a row's ZDATE can be older
// than rows already inserted. These tests drive real plugin ticks over
// an on-disk CallHistoryDB.
//
// Synthetic handles only (+15550000001); no real PII.
import XCTest
import Foundation
import GRDB
import CRMMacCore
import CRMMacPiClient
@testable import CRMMacPhoneCallsSource

final class PhoneCallsCursorOrderTests: XCTestCase {
    private let auth = PiAuth(
        hostID: UUID(uuidString: "11111111-2222-3333-4444-555555555555")!,
        apiKey: "k")
    private let handle = "+15550000001"
    private let backfillFloor = PhoneCallsCursorWire.defaultBackfillFloor
    private let fixedNow = Date(timeIntervalSince1970: 1_779_000_000) // 2026-05-20
    private let baseUnix: TimeInterval = 1_777_680_000 // 2026-05-02
    private var tempDir: URL!
    private var dbURL: URL!

    override func setUpWithError() throws {
        try super.setUpWithError()
        tempDir = FileManager.default.temporaryDirectory
            .appendingPathComponent("crm-mac-phone-cursor-order-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: tempDir, withIntermediateDirectories: true)
        dbURL = tempDir.appendingPathComponent("CallHistory.storedata")
        guard let scriptURL = Bundle.module.url(forResource: "call_history_db_schema",
                                                withExtension: "sql",
                                                subdirectory: "Fixtures") else {
            throw XCTSkip("call_history_db_schema.sql not in test bundle")
        }
        let script = try String(contentsOf: scriptURL, encoding: .utf8)
        try DatabaseQueue(path: dbURL.path).write { try $0.execute(sql: script) }
    }

    override func tearDownWithError() throws {
        try? FileManager.default.removeItem(at: tempDir)
        try super.tearDownWithError()
    }

    private func zdate(_ offset: TimeInterval) -> Double {
        InMemoryCallHistoryDB.appleEpochSeconds(unix: baseUnix + offset)
    }

    /// Insert one call for `handle` at `Z_PK = zPK`, starting
    /// `offset` seconds after `baseUnix`.
    private func insertCall(zPK: Int64, offset: TimeInterval) throws {
        try DatabaseQueue(path: dbURL.path).write { db in
            try db.execute(sql: """
                INSERT INTO ZCALLRECORD (
                    Z_PK, ZUNIQUE_ID, ZDATE, ZADDRESS,
                    ZORIGINATED, ZANSWERED, ZDURATION,
                    ZSERVICE_PROVIDER, ZCALLTYPE, ZHASMESSAGE)
                VALUES (?, ?, ?, ?, 1, NULL, 30, 'com.apple.Telephony', 0, 0)
                """,
                arguments: [zPK, "u\(zPK)", zdate(offset), handle])
        }
    }

    /// Fake ingest endpoint. Records the events of every accepted
    /// batch; while `failing` is set it throws, as a transport error
    /// does, so the publish is unconfirmed.
    private actor Ingest {
        private(set) var events: [String] = []
        var failing = false
        func setFailing(_ value: Bool) { failing = value }
        func send(_ body: IngestEventsBody) throws -> IngestEventsData {
            if failing { throw URLError(.notConnectedToInternet) }
            events.append(contentsOf: body.events.map(\.sourceID))
            return IngestEventsData(
                accepted: body.events.count, duplicate: 0, rejected: 0, errors: [])
        }
    }

    private func makePlugin(
        transport: PhoneStatefulCursorTransport,
        ingest: Ingest,
        maxRowsPerTick: Int = 500
    ) async throws -> PhoneCallsSourcePlugin {
        let store = StateStore(fileURL: tempDir.appendingPathComponent("state.json"))
        try store.save(DaemonState(schemaVersion: 1))
        let cache = KnownIdentifiersCache(
            baselines: [.phoneCalls: [handle]], consumers: [.phoneCalls])
        await cache.replace(with: [handle])
        let publisher = PhoneCallsPublisher(
            sender: { _, body in try await ingest.send(body) },
            auth: auth, logger: NoopLogger())
        let now = fixedNow
        return PhoneCallsSourcePlugin(
            tickInterval: 60,
            config: PhoneCallsSourceConfig(
                callHistoryDBPath: dbURL,
                backfillFloor: backfillFloor,
                maxRowsPerTick: maxRowsPerTick),
            piClient: PiClient(
                baseURL: URL(string: "https://pi.example.test")!,
                transport: transport.asTransport(),
                sleep: { _ in }),
            auth: auth,
            mutator: StateMutator(store: store),
            publisher: publisher,
            cache: cache,
            canonicalizer: { NormalizationParity.normalizePhoneE164($0) },
            heartbeatStateProvider: InMemoryHeartbeatStateProvider(initial: 2),
            healthRegistry: SourceHealthRegistry(),
            logger: NoopLogger(),
            clock: { now })
    }

    /// Two calls already emitted, the live cursor on the newer one
    /// (Z_PK 101 at +100s), then three calls synced in after it:
    /// 102 started BEFORE the cursor's call (+50s), and 103/104 were
    /// inserted out of call-time order (+200s, +150s).
    private func seedLateSyncedCalls() throws -> PhoneCallsCursor {
        try insertCall(zPK: 100, offset: 0)
        try insertCall(zPK: 101, offset: 100)
        try insertCall(zPK: 102, offset: 50)
        try insertCall(zPK: 103, offset: 200)
        try insertCall(zPK: 104, offset: 150)
        return PhoneCallsCursor(
            backfillCursorZDate: 0,
            backfillCursorZPK: 0,
            liveCursorZDate: zdate(100),
            liveCursorZPK: 101,
            installMaxZDate: zdate(0),
            installMaxZPK: 100,
            backfillFloorSentAt: backfillFloor,
            backfillComplete: true)
    }

    func testLiveBatchEmitsRowsInsertedAfterCursorWithOlderCallTimes() async throws {
        let seeded = try seedLateSyncedCalls()
        let transport = PhoneStatefulCursorTransport(
            initialCursor: try PhoneCallsCursorCodec.encode(seeded))
        let ingest = Ingest()
        let plugin = try await makePlugin(transport: transport, ingest: ingest)

        try await plugin.tick()

        let firstTick = await ingest.events
        XCTAssertEqual(firstTick, ["u102", "u103", "u104"],
                       "every row inserted after the cursor is emitted once, in insertion order")
        XCTAssertEqual(transport.currentDecodedCursor()?.liveCursorZPK, 104,
                       "live cursor advances to the last inserted row")

        try await plugin.tick()
        let afterSecondTick = await ingest.events
        XCTAssertEqual(afterSecondTick, firstTick, "a second tick emits nothing")
    }

    func testLiveCursorHoldsWhenPublishIsUnconfirmed() async throws {
        let seeded = try seedLateSyncedCalls()
        let transport = PhoneStatefulCursorTransport(
            initialCursor: try PhoneCallsCursorCodec.encode(seeded))
        let ingest = Ingest()
        await ingest.setFailing(true)
        let plugin = try await makePlugin(transport: transport, ingest: ingest)

        try await plugin.tick()

        XCTAssertEqual(transport.currentDecodedCursor()?.liveCursorZPK, 101,
                       "an unconfirmed publish leaves the committed live cursor in place")

        await ingest.setFailing(false)
        try await plugin.tick()

        let events = await ingest.events
        XCTAssertEqual(events, ["u102", "u103", "u104"],
                       "the rows from the failed tick are emitted on the next tick")
        XCTAssertEqual(transport.currentDecodedCursor()?.liveCursorZPK, 104)
    }

    func testBackfillAdvancesToLowestRowOfEachPage() async throws {
        // install_max is Z_PK 105 (+100s). Below it, Z_PK order and
        // call-time order disagree; descending call time is
        // 100 (+40s), 102 (+30s), 104 (+20s), 101 (+10s), 103 (+0s).
        try insertCall(zPK: 100, offset: 40)
        try insertCall(zPK: 101, offset: 10)
        try insertCall(zPK: 102, offset: 30)
        try insertCall(zPK: 103, offset: 0)
        try insertCall(zPK: 104, offset: 20)
        try insertCall(zPK: 105, offset: 100)
        let seeded = PhoneCallsCursor(
            liveCursorZDate: zdate(100),
            liveCursorZPK: 1_000_000, // live reads nothing; isolates backfill
            installMaxZDate: zdate(100),
            installMaxZPK: 105,
            backfillFloorSentAt: backfillFloor)
        let transport = PhoneStatefulCursorTransport(
            initialCursor: try PhoneCallsCursorCodec.encode(seeded))
        let ingest = Ingest()
        let plugin = try await makePlugin(transport: transport, ingest: ingest, maxRowsPerTick: 2)

        try await plugin.tick()

        let afterFirst = transport.currentDecodedCursor()
        XCTAssertEqual(afterFirst?.backfillCursorZPK, 102,
                       "backfill cursor lands on the lowest row of the page, not the first")
        XCTAssertEqual(afterFirst?.backfillCursorZDate, zdate(30))

        try await plugin.tick()
        try await plugin.tick()

        let events = await ingest.events
        XCTAssertEqual(events, ["u100", "u102", "u104", "u101", "u103"],
                       "backfill emits each row once, in descending call time")
        XCTAssertEqual(transport.currentDecodedCursor()?.backfillComplete, true)
    }
}
