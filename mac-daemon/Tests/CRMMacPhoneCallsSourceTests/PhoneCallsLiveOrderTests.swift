// PhoneCallsLiveOrderTests — the live batch must emit every row
// inserted after the cursor, whatever its call time.
//
// iCloud call-history sync writes an iPhone call to the Mac's
// CallHistoryDB hours after it happened, so a row's ZDATE (call start)
// can be older than rows already inserted. These tests drive real
// plugin ticks over an on-disk CallHistoryDB.
//
// Synthetic handles only (+15550000001); no real PII.
import XCTest
import Foundation
import GRDB
import CRMMacCore
import CRMMacPiClient
@testable import CRMMacPhoneCallsSource

final class PhoneCallsLiveOrderTests: XCTestCase {
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
            .appendingPathComponent("crm-mac-phone-live-order-\(UUID().uuidString)")
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

    /// Insert one call for `handle` at `Z_PK = zPK`, starting
    /// `offset` seconds after `baseUnix`.
    private func insertCall(zPK: Int64, offset: TimeInterval) throws {
        let zdate = InMemoryCallHistoryDB.appleEpochSeconds(unix: baseUnix + offset)
        try DatabaseQueue(path: dbURL.path).write { db in
            try db.execute(sql: """
                INSERT INTO ZCALLRECORD (
                    Z_PK, ZUNIQUE_ID, ZDATE, ZADDRESS,
                    ZORIGINATED, ZANSWERED, ZDURATION,
                    ZSERVICE_PROVIDER, ZCALLTYPE, ZHASMESSAGE)
                VALUES (?, ?, ?, ?, 1, NULL, 30, 'com.apple.Telephony', 0, 0)
                """,
                arguments: [zPK, "u\(zPK)", zdate, handle])
        }
    }

    private func makePlugin(
        transport: PhoneStatefulCursorTransport,
        sink: PhonePublisherSink
    ) async throws -> PhoneCallsSourcePlugin {
        let store = StateStore(fileURL: tempDir.appendingPathComponent("state.json"))
        try store.save(DaemonState(schemaVersion: 1))
        let cache = KnownIdentifiersCache(
            baselines: [.phoneCalls: [handle]], consumers: [.phoneCalls])
        await cache.replace(with: [handle])
        let publisher = PhoneCallsPublisher(
            sender: { _, body in
                await sink.record(body.events)
                return IngestEventsData(
                    accepted: body.events.count, duplicate: 0, rejected: 0, errors: [])
            },
            auth: auth, logger: NoopLogger())
        let now = fixedNow
        return PhoneCallsSourcePlugin(
            tickInterval: 60,
            config: PhoneCallsSourceConfig(callHistoryDBPath: dbURL, backfillFloor: backfillFloor),
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

    func testLiveBatchEmitsRowsInsertedAfterCursorWithOlderCallTimes() async throws {
        // Two calls already emitted; the live cursor sits on the newer
        // one (Z_PK 101 at +100s). Backfill is complete.
        try insertCall(zPK: 100, offset: 0)
        try insertCall(zPK: 101, offset: 100)
        let cursorZDate = InMemoryCallHistoryDB.appleEpochSeconds(unix: baseUnix + 100)
        let seeded = PhoneCallsCursor(
            backfillCursorZDate: 0,
            backfillCursorZPK: 0,
            liveCursorZDate: cursorZDate,
            liveCursorZPK: 101,
            installMaxZDate: InMemoryCallHistoryDB.appleEpochSeconds(unix: baseUnix),
            installMaxZPK: 100,
            backfillFloorSentAt: backfillFloor,
            backfillComplete: true)

        // iCloud sync lands three calls after the cursor:
        //  - 102 started BEFORE the cursor's call (+50s),
        //  - 103 and 104 inserted out of call-time order (+200s, +150s).
        try insertCall(zPK: 102, offset: 50)
        try insertCall(zPK: 103, offset: 200)
        try insertCall(zPK: 104, offset: 150)

        let transport = PhoneStatefulCursorTransport(
            initialCursor: try PhoneCallsCursorCodec.encode(seeded))
        let sink = PhonePublisherSink()
        let plugin = try await makePlugin(transport: transport, sink: sink)

        try await plugin.tick()

        let firstTick = await sink.allEvents().map(\.sourceID)
        XCTAssertEqual(Set(firstTick), ["u102", "u103", "u104"],
                       "every row inserted after the cursor is emitted")
        XCTAssertEqual(transport.currentDecodedCursor()?.liveCursorZPK, 104,
                       "live cursor advances to the last inserted row")

        // A second tick re-emits nothing: the cursor did not land on a
        // row below the highest Z_PK it read.
        try await plugin.tick()
        let afterSecondTick = await sink.allEvents().map(\.sourceID)
        XCTAssertEqual(afterSecondTick.count, firstTick.count,
                       "no row is emitted twice")
    }
}
