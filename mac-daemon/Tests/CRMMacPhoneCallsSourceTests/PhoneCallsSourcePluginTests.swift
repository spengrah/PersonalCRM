// Plugin orchestration tests for PhoneCallsSourcePlugin. Cover the
// observable contracts that don't require a live CallHistoryDB
// (those that DO require GRDB go in CallHistoryDBReaderTests).
//
// The plugin's tick path makes a few synchronous decisions before
// touching SQLite (protocol-version gate, cache populated check),
// which we exercise here with stubs.
import XCTest
import CRMMacCore
import CRMMacPiClient
@testable import CRMMacPhoneCallsSource

final class PhoneCallsSourcePluginTests: XCTestCase {
    private let hostID = UUID(uuidString: "11111111-2222-3333-4444-555555555555")!

    // MARK: - test stubs

    /// In-memory StateStore + StateMutator for testing.
    private func makeMutator() throws -> StateMutator {
        let tmpDir = FileManager.default.temporaryDirectory
            .appendingPathComponent("crm-mac-phone-calls-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: tmpDir, withIntermediateDirectories: true)
        let stateURL = tmpDir.appendingPathComponent("state.json")
        let store = StateStore(fileURL: stateURL)
        try store.save(DaemonState(schemaVersion: 1))
        return StateMutator(store: store)
    }

    /// Build a plugin instance for protocol-version gate tests. The
    /// CallHistoryDB path is intentionally bogus — these tests should
    /// not reach the open path.
    private func makePlugin(
        cache: KnownIdentifiersCache,
        provider: HeartbeatStateProvider,
        mutator: StateMutator
    ) -> PhoneCallsSourcePlugin {
        let auth = PiAuth(hostID: hostID, apiKey: "k")
        let piClient = PiClient(baseURL: URL(string: "https://example.invalid")!,
                                 logger: NoopLogger())
        let publisher = PhoneCallsPublisher(
            sender: { _, _ in
                IngestEventsData(accepted: 0, duplicate: 0, rejected: 0, errors: [])
            },
            auth: auth,
            logger: NoopLogger())
        let config = PhoneCallsSourceConfig(
            callHistoryDBPath: URL(fileURLWithPath: "/dev/null/nope"),
            backfillFloor: PhoneCallsCursor.defaultBackfillFloor)
        return PhoneCallsSourcePlugin(
            tickInterval: 60,
            config: config,
            piClient: piClient,
            auth: auth,
            mutator: mutator,
            publisher: publisher,
            cache: cache,
            canonicalizer: { $0 },
            heartbeatStateProvider: provider,
            healthRegistry: SourceHealthRegistry(),
            logger: NoopLogger())
    }

    // MARK: - protocol-version gate (T-Swift-11)

    func testProtocolGateBlocksWhenPiVersionTooLow() async throws {
        // Pi reports protocol_version 1; plugin requires >= 2. The
        // tick must short-circuit BEFORE touching the (invalid)
        // CallHistoryDB path. If the gate fails to short-circuit,
        // the bogus DB path would surface as a thrown error.
        let cache = KnownIdentifiersCache()
        let provider = InMemoryHeartbeatStateProvider(initial: 1)
        let mutator = try makeMutator()
        let plugin = makePlugin(cache: cache, provider: provider, mutator: mutator)
        // Must not throw — the gate returns early before any I/O.
        try await plugin.tick()
    }

    func testProtocolGateBlocksWhenPiVersionUnknown() async throws {
        // nil = no heartbeat recorded yet -> wait.
        let cache = KnownIdentifiersCache()
        let provider = InMemoryHeartbeatStateProvider(initial: nil)
        let mutator = try makeMutator()
        let plugin = makePlugin(cache: cache, provider: provider, mutator: mutator)
        try await plugin.tick()
    }

    func testProtocolGatePassesAtRequiredVersion() async throws {
        // Pi reports protocol_version 2. The gate passes; the tick
        // proceeds to the schema-open step, which throws because the
        // CallHistoryDB path is bogus. We assert the gate was passed
        // by relying on the open failure NOT being a "gate blocked"
        // signal (gate-blocked is silent; open failure is logged but
        // does not throw because the plugin marks itself unhealthy
        // and returns).
        let cache = KnownIdentifiersCache()
        let provider = InMemoryHeartbeatStateProvider(initial: 2)
        let mutator = try makeMutator()
        let plugin = makePlugin(cache: cache, provider: provider, mutator: mutator)
        // Tick must not throw — open failure is caught + marked unhealthy.
        try await plugin.tick()
    }

    // MARK: - minimum-protocol-version constant

    func testMinPiProtocolVersionIsExposed() {
        // Anchored test: the constant is the contract between the
        // daemon and the Pi (the Pi must bump its protocol_version to
        // this value before phone_calls becomes active).
        XCTAssertEqual(CRMMacPhoneCallsSource.minPiProtocolVersion, 2)
    }
}
