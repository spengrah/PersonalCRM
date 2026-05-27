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
    /// not reach the open path on gate-blocked runs, and on gate-passed
    /// runs the bogus path makes openFreshPool fail so the plugin
    /// observably marks itself unhealthy. Returns both the plugin and
    /// the shared health registry so callers can read post-tick state.
    private func makePlugin(
        cache: KnownIdentifiersCache,
        provider: HeartbeatStateProvider,
        mutator: StateMutator
    ) -> (PhoneCallsSourcePlugin, SourceHealthRegistry) {
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
        let registry = SourceHealthRegistry()
        let plugin = PhoneCallsSourcePlugin(
            tickInterval: 60,
            config: config,
            piClient: piClient,
            auth: auth,
            mutator: mutator,
            publisher: publisher,
            cache: cache,
            canonicalizer: { $0 },
            heartbeatStateProvider: provider,
            healthRegistry: registry,
            logger: NoopLogger())
        return (plugin, registry)
    }

    // MARK: - protocol-version gate

    func testProtocolGateBlocksWhenPiVersionTooLow() async throws {
        // Pi reports protocol_version 1; plugin requires >= 2. The
        // tick must short-circuit BEFORE touching the (invalid)
        // CallHistoryDB path. Observable: the registry snapshot
        // recorded at tick start carries enabled=true (plugin is wired,
        // just gated) with no lastPushedAt and no lastError. If the
        // gate failed to short-circuit, the bogus path would have
        // marked the plugin unhealthy (enabled=false + lastError set).
        let cache = KnownIdentifiersCache()
        let provider = InMemoryHeartbeatStateProvider(initial: 1)
        let mutator = try makeMutator()
        let (plugin, registry) = makePlugin(cache: cache, provider: provider, mutator: mutator)
        try await plugin.tick()
        let snap = await registry.read(.phoneCalls)
        XCTAssertNotNil(snap, "gate-blocked tick should still write the initial enabled snapshot")
        XCTAssertEqual(snap?.enabled, true, "gate-blocked is silent: enabled stays true")
        XCTAssertNil(snap?.lastPushedAt, "gate-blocked tick must not record a push")
        XCTAssertNil(snap?.lastError, "gate-blocked is not an error state")
    }

    func testProtocolGateBlocksWhenPiVersionUnknown() async throws {
        // nil = no heartbeat recorded yet -> wait. Same observable
        // contract as the version-too-low case.
        let cache = KnownIdentifiersCache()
        let provider = InMemoryHeartbeatStateProvider(initial: nil)
        let mutator = try makeMutator()
        let (plugin, registry) = makePlugin(cache: cache, provider: provider, mutator: mutator)
        try await plugin.tick()
        let snap = await registry.read(.phoneCalls)
        XCTAssertNotNil(snap)
        XCTAssertEqual(snap?.enabled, true)
        XCTAssertNil(snap?.lastPushedAt)
        XCTAssertNil(snap?.lastError)
    }

    func testProtocolGatePassesAtRequiredVersion() async throws {
        // Pi reports protocol_version 2. The gate passes; the tick
        // proceeds to openFreshPool, which fails because the
        // CallHistoryDB path is bogus. The exact failure reason
        // depends on the GRDB / SQLite error code SQLite returns for
        // `/dev/null/nope`: CANTOPEN/AUTH/PERM map to `fda_required`
        // via isFDAError, schema validation maps to `schema_drift:*`,
        // anything else maps to `open_failed: ...`. All three are
        // post-gate failure signals; we just assert the plugin
        // observably marked itself unhealthy (enabled=false +
        // lastError populated), distinguishing this from the
        // gate-blocked branch above which leaves enabled=true and
        // lastError=nil.
        let cache = KnownIdentifiersCache()
        let provider = InMemoryHeartbeatStateProvider(initial: 2)
        let mutator = try makeMutator()
        let (plugin, registry) = makePlugin(cache: cache, provider: provider, mutator: mutator)
        try await plugin.tick()
        let snap = await registry.read(.phoneCalls)
        XCTAssertNotNil(snap, "gate-passed tick must have written a snapshot")
        XCTAssertEqual(snap?.enabled, false, "gate-passed + open-failed must mark unhealthy")
        let err = snap?.lastError ?? ""
        XCTAssertFalse(err.isEmpty, "lastError must be populated for the gate-passed failure path")
        let acceptable = err.contains("open_failed") ||
                         err.contains("schema_drift") ||
                         err.contains("fda_required")
        XCTAssertTrue(acceptable,
                      "lastError should encode a known open-failure cause, got: \(err)")
        XCTAssertNotNil(snap?.lastErrorAt, "lastErrorAt must be set when lastError is set")
    }

    // MARK: - lastScheduledAt persistence

    func testTickPersistsLastScheduledAtToState() async throws {
        // The plugin's tick() writes `lastScheduledAt` to state.json on
        // every invocation, BEFORE the protocol-version gate, so even a
        // gate-blocked tick must persist the field. Without this
        // persistence, Doctor / debugging tools have no reliable
        // cross-source liveness signal — the in-memory
        // SourceHealthRegistry is heartbeat-payload-only.
        let cache = KnownIdentifiersCache()
        let provider = InMemoryHeartbeatStateProvider(initial: 1) // gate-blocked
        let mutator = try makeMutator()
        let (plugin, _) = makePlugin(cache: cache, provider: provider, mutator: mutator)
        let beforeTick = Date()
        try await plugin.tick()
        let state = try await mutator.read()
        let scheduled = state.sources[SourceID.phoneCalls.rawValue]?.lastScheduledAt
        XCTAssertNotNil(scheduled, "tick() must persist lastScheduledAt to state.json")
        if let scheduled {
            XCTAssertGreaterThanOrEqual(
                scheduled, beforeTick.addingTimeInterval(-1),
                "lastScheduledAt must be set to a clock value near the tick start")
        }
    }

    // MARK: - minimum-protocol-version constant

    func testMinPiProtocolVersionIsExposed() {
        // Anchored test: the constant is the contract between the
        // daemon and the Pi (the Pi must bump its protocol_version to
        // this value before phone_calls becomes active).
        XCTAssertEqual(CRMMacPhoneCallsSource.minPiProtocolVersion, 2)
    }
}
