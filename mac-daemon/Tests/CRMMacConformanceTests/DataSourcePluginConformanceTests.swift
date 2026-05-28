// DataSourcePluginConformanceTests — the cross-cutting guard that
// makes the DataSourcePlugin heartbeat contract un-forgettable for a
// future sixth data source.
//
// Four layers, because no single mechanism is sufficient on its own:
//
//   (a) Per-plugin behavioral assertion through an `any SourcePlugin`
//       reference. For each of the five concrete plugins, build a
//       minimal instance whose performTick() short-circuits fast +
//       harmlessly (gated / denied / not-configured / bogus path),
//       bind it to `let plugin: any SourcePlugin = <concrete>`, call
//       `plugin.tick()` through that protocol-typed reference (the SAME
//       witness the production scheduler dispatches to), and assert
//       BOTH writes: state.json `lastScheduledAt == T`, and the
//       SourceHealthRegistry snapshot's `lastScheduledAt != nil` (the
//       Pi /heartbeat-payload field). A plugin that (re)declares its
//       own concrete `tick()` and forgets the bump fails the state.json
//       assertion; a plugin that drops its healthRegistry.update(...)
//       fails the registry assertion.
//
//   (b) Source grep guard: no `func tick(` declaration may exist in any
//       of the four source-library-target directories (the witness must
//       come from the CRMMacCore extension default, never a shadowing
//       concrete tick()). Target-wide naming ban by design — `tick` is
//       reserved vocabulary in these modules; a future non-plugin
//       tick() must rename or tighten this guard to type-scoped
//       matching.
//
//   (c) Enumeration completeness: ONE `expectedDataSources` literal,
//       asserted simultaneously against (1) the SOURCE-derived set (a
//       filesystem grep for `: DataSourcePlugin` conformers + their
//       `id: SourceID = .<case>` literal) and (2) the BEHAVIOR-derived
//       set collected in layer (a). A sixth source must appear in
//       source, in the expected literal, AND in the behavioral loop, or
//       some pair is unequal and this fails. The single literal lives
//       ONLY here so it cannot be silenced by editing a duplicate copy.
//
//   (d) Extension-default + coalescing smoke tests using a tiny in-test
//       stub conforming to DataSourcePlugin (a final class, since
//       SourcePlugin: AnyObject forbids value types): assert the
//       extension tick() bumps lastScheduledAt and calls performTick()
//       exactly once, and that a coalescing-style stub that early-
//       returns still records the state.json bump.
import XCTest
import Foundation
import CRMMacCore
import CRMMacPiClient
@testable import CRMMacMessagesSource
@testable import CRMMacPhoneCallsSource
@testable import CRMMacIcloudContactsSource
@testable import CRMMacAnarlogSource

final class DataSourcePluginConformanceTests: XCTestCase {
    /// The authoritative set of data sources. There is exactly ONE
    /// copy of this literal in the whole test suite — do NOT duplicate
    /// it into another target (a duplicate would let a sixth source be
    /// silenced by editing only one copy while the other compares
    /// old==old). Layer (c) asserts this against both the source-derived
    /// and behavior-derived sets.
    private static let expectedDataSources: Set<SourceID> = [
        .messages, .phoneCalls, .icloudContacts, .anarlogHumans, .anarlogSessions,
    ]

    private let hostID = UUID(uuidString: "11111111-2222-3333-4444-555555555555")!
    private let fixedInstant = Date(timeIntervalSince1970: 1_750_000_000)

    // MARK: - per-test state wiring

    /// Fresh on-disk StateStore + StateMutator in a unique temp dir
    /// (mirrors PhoneCallsSourcePluginTests.makeMutator). Returns the
    /// store too so the caller can read state.json back.
    private func makeStateMutator() throws -> (StateMutator, StateStore) {
        let dir = FileManager.default.temporaryDirectory
            .appendingPathComponent("crm-mac-conformance-\(UUID().uuidString)", isDirectory: true)
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        let stateURL = dir.appendingPathComponent("state.json")
        let store = StateStore(fileURL: stateURL)
        try store.save(DaemonState(schemaVersion: 1))
        return (StateMutator(store: store), store)
    }

    private func clock() -> @Sendable () -> Date {
        let instant = fixedInstant
        return { instant }
    }

    private func auth() -> PiAuth { PiAuth(hostID: hostID, apiKey: "k") }

    private func unreachablePiClient() -> PiClient {
        PiClient(baseURL: URL(string: "https://example.invalid")!, logger: NoopLogger())
    }

    // MARK: - (a) per-plugin behavioral assertion

    func testEveryDataPluginBumpsHeartbeatThroughSourcePluginWitness() async throws {
        var behaviorallyTestedIDs: Set<SourceID> = []

        for spec in try makePluginSpecs() {
            // Bind to `any SourcePlugin` so we exercise the production
            // witness, not a concrete-typed call.
            let plugin: any SourcePlugin = spec.plugin
            try await plugin.tick()

            // (1) state.json — written by the extension's
            // persistScheduledHeartbeat before performTick() ran.
            let state = try await spec.mutator.read()
            let persisted = state.sources[plugin.id.rawValue]?.lastScheduledAt
            XCTAssertEqual(persisted, fixedInstant,
                           "\(plugin.id.rawValue): tick() must persist " +
                           "lastScheduledAt == T to state.json")

            // (2) heartbeat-payload registry — written by the plugin's
            // own healthRegistry.update(...) inside performTick(). Each
            // plugin's fast-path snapshot stamps lastScheduledAt from
            // the same injected (fixed) clock — either the tick-start
            // snapshot (gated PhoneCalls) or the markUnhealthy snapshot
            // (the other four) — so the heartbeat-payload timestamp must
            // be exactly T, not merely non-nil. A plugin that emits a
            // stale/wrong heartbeat timestamp fails here.
            let snap = await spec.registry.read(plugin.id)
            XCTAssertNotNil(snap,
                            "\(plugin.id.rawValue): performTick() must report a " +
                            "SourceHealthRegistry snapshot")
            XCTAssertEqual(snap?.lastScheduledAt, fixedInstant,
                           "\(plugin.id.rawValue): the heartbeat-payload snapshot must " +
                           "carry lastScheduledAt == T (feeds the Pi /heartbeat)")

            behaviorallyTestedIDs.insert(plugin.id)
        }

        // Behavior-derived completeness (half of layer (c)).
        XCTAssertEqual(behaviorallyTestedIDs, Self.expectedDataSources,
                       "every expected data source must be behaviorally exercised; " +
                       "a new conformer must be added to the instantiation loop")
    }

    // MARK: - (b) no `func tick(` in conformer modules

    func testNoConcreteTickInDataSourceModules() throws {
        let sourcesRoot = Self.resolveSourcesRoot()
        let moduleDirs = [
            "CRMMacMessagesSource", "CRMMacPhoneCallsSource",
            "CRMMacIcloudContactsSource", "CRMMacAnarlogSource",
        ].map { sourcesRoot.appendingPathComponent($0) }

        var offenders: [String] = []
        for dir in moduleDirs {
            for fileURL in try Self.swiftFiles(under: dir) {
                let code = try String(contentsOf: fileURL, encoding: .utf8)
                if code.range(of: #"func\s+tick\s*\("#, options: .regularExpression) != nil {
                    offenders.append(fileURL.lastPathComponent)
                }
            }
        }
        XCTAssertTrue(offenders.isEmpty,
                      "DataSourcePlugin conformers must implement performTick(), NOT " +
                      "tick() — a concrete tick() would shadow the CRMMacCore extension " +
                      "default and silently disable the heartbeat. Offending file(s): " +
                      offenders.joined(separator: ", "))
    }

    // MARK: - (c) source-derived enumeration completeness

    /// Scanner assumption (documented convention): a DataSourcePlugin
    /// conformer declares `: DataSourcePlugin` and its
    /// `id: SourceID = .<case>` on single lines within the same source
    /// file. All five current plugins satisfy this. A future conformer
    /// in a different shape must either keep this form OR upgrade this
    /// scanner to a real parse — flagged here so a regex miss surfaces
    /// as a deliberate decision, not a silent gap.
    func testSourceDerivedConformerSetMatchesExpected() throws {
        let sourcesRoot = Self.resolveSourcesRoot()
        var sourceDerived: Set<SourceID> = []
        // Track conformer files vs. successfully-extracted ids so a
        // conformer whose declaration shape defeats the regex (extension
        // conformance, comma-separated clause, computed id, id literal
        // in another file) fails LOUDLY via the count check below rather
        // than being silently invisible to the set comparison.
        var conformerFileCount = 0
        var extractedIDCount = 0
        for fileURL in try Self.swiftFiles(under: sourcesRoot) {
            let code = try String(contentsOf: fileURL, encoding: .utf8)
            guard code.range(of: #":\s*DataSourcePlugin"#, options: .regularExpression) != nil
            else { continue }
            conformerFileCount += 1
            let cases = Self.idCaseLiterals(in: code)
            for rawCase in cases {
                extractedIDCount += 1
                guard let id = Self.sourceID(forCaseName: rawCase) else {
                    XCTFail("\(fileURL.lastPathComponent): unknown SourceID case " +
                            "`.\(rawCase)` — update sourceID(forCaseName:) when adding a " +
                            "SourceID case")
                    continue
                }
                sourceDerived.insert(id)
            }
        }
        // One conformer file -> exactly one id literal -> one expected
        // entry. Any mismatch means the scanner missed (or double-counted)
        // a conformer; surface it instead of letting old==old pass.
        XCTAssertEqual(conformerFileCount, Self.expectedDataSources.count,
                       "found \(conformerFileCount) `: DataSourcePlugin` conformer " +
                       "file(s) but expected \(Self.expectedDataSources.count) — a new " +
                       "conformer must be wired into expectedDataSources + layer (a)'s loop")
        XCTAssertEqual(extractedIDCount, conformerFileCount,
                       "extracted \(extractedIDCount) id literal(s) from " +
                       "\(conformerFileCount) conformer file(s) — a conformer whose " +
                       "`id: SourceID = .<case>` form defeats the scanner must keep the " +
                       "documented single-line form OR the scanner must be upgraded")
        XCTAssertEqual(sourceDerived, Self.expectedDataSources,
                       "the set of `: DataSourcePlugin` conformers derived from Sources/ " +
                       "must equal expectedDataSources; a new conformer must be added to " +
                       "the expected literal (and to layer (a)'s loop)")
    }

    // MARK: - (d) extension-default + coalescing smoke

    func testExtensionDefaultBumpsAndCallsPerformTickOnce() async throws {
        let (mutator, _) = try makeStateMutator()
        let stub = StubDataSource(id: .messages, mutator: mutator, clock: clock())
        let plugin: any SourcePlugin = stub
        try await plugin.tick()

        let calls = stub.performTickCalls
        XCTAssertEqual(calls, 1, "extension tick() must call performTick() exactly once")
        let state = try await mutator.read()
        XCTAssertEqual(state.sources[SourceID.messages.rawValue]?.lastScheduledAt,
                       fixedInstant,
                       "extension tick() must bump lastScheduledAt before performTick()")
    }

    func testCoalescingEarlyReturnStillBumpsStateJSON() async throws {
        let (mutator, _) = try makeStateMutator()
        // Simulate the Sessions coalescing branch: performTick() early-
        // returns (tickInFlight). The extension's tick() still bumps
        // state.json BEFORE performTick() runs, so the bump persists
        // even on a coalesced fire.
        let stub = StubDataSource(id: .anarlogSessions, mutator: mutator,
                                  clock: clock(), earlyReturn: true)
        let plugin: any SourcePlugin = stub
        try await plugin.tick()

        let calls = stub.performTickCalls
        XCTAssertEqual(calls, 1, "performTick() is still entered (and early-returns)")
        let state = try await mutator.read()
        XCTAssertEqual(state.sources[SourceID.anarlogSessions.rawValue]?.lastScheduledAt,
                       fixedInstant,
                       "a coalesced fire that early-returns must still record a " +
                       "state.json lastScheduledAt bump")
    }

    // MARK: - plugin construction (layer a)

    private struct PluginSpec {
        let plugin: any SourcePlugin
        let mutator: StateMutator
        let registry: SourceHealthRegistry
    }

    /// Build one short-circuiting instance of each of the five plugins.
    /// Each reaches an early-return path right after the heartbeat bump:
    ///   - Messages: bogus chat.db path -> open fails -> markUnhealthy.
    ///   - PhoneCalls: Pi protocol_version too low -> gate returns.
    ///   - iCloud: auth denied -> markUnhealthy.
    ///   - Anarlog humans/sessions: not configured -> markUnhealthy.
    /// In every case the extension persisted state.json BEFORE
    /// performTick(), and performTick() reported a registry snapshot
    /// carrying lastScheduledAt.
    private func makePluginSpecs() throws -> [PluginSpec] {
        var specs: [PluginSpec] = []

        // Messages — bogus path.
        do {
            let (mutator, _) = try makeStateMutator()
            let registry = SourceHealthRegistry()
            let publisher = MessagesPublisher(
                sender: { _, _ in IngestEventsData(accepted: 0, duplicate: 0, rejected: 0, errors: []) },
                auth: auth(), logger: NoopLogger())
            let plugin = MessagesSourcePlugin(
                tickInterval: 60,
                config: MessagesSourceConfig(
                    chatDBPath: URL(fileURLWithPath: "/dev/null/nope"),
                    backfillFloor: Date(timeIntervalSince1970: 0)),
                piClient: unreachablePiClient(),
                auth: auth(),
                mutator: mutator,
                publisher: publisher,
                cache: KnownIdentifiersCache(),
                healthRegistry: registry,
                logger: NoopLogger(),
                clock: clock())
            specs.append(PluginSpec(plugin: plugin, mutator: mutator, registry: registry))
        }

        // PhoneCalls — protocol gate (Pi version below required).
        do {
            let (mutator, _) = try makeStateMutator()
            let registry = SourceHealthRegistry()
            let publisher = PhoneCallsPublisher(
                sender: { _, _ in IngestEventsData(accepted: 0, duplicate: 0, rejected: 0, errors: []) },
                auth: auth(), logger: NoopLogger())
            let plugin = PhoneCallsSourcePlugin(
                tickInterval: 60,
                config: PhoneCallsSourceConfig(
                    callHistoryDBPath: URL(fileURLWithPath: "/dev/null/nope"),
                    backfillFloor: Date(timeIntervalSince1970: 0)),
                piClient: unreachablePiClient(),
                auth: auth(),
                mutator: mutator,
                publisher: publisher,
                cache: KnownIdentifiersCache(),
                canonicalizer: { $0 },
                heartbeatStateProvider: InMemoryHeartbeatStateProvider(initial: 1),
                healthRegistry: registry,
                logger: NoopLogger(),
                clock: clock())
            specs.append(PluginSpec(plugin: plugin, mutator: mutator, registry: registry))
        }

        // iCloud — auth denied.
        do {
            let (mutator, _) = try makeStateMutator()
            let registry = SourceHealthRegistry()
            let dir = FileManager.default.temporaryDirectory
                .appendingPathComponent("crm-mac-conformance-icloud-\(UUID().uuidString)")
            try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
            let publisher = ICloudContactsPublisher(
                sender: { _, _ in IngestEventsData(accepted: 0, duplicate: 0, rejected: 0, errors: []) },
                auth: auth(), logger: NoopLogger())
            let plugin = ICloudContactsSourcePlugin(
                tickInterval: 900,
                piClient: unreachablePiClient(),
                auth: auth(),
                mutator: mutator,
                publisher: publisher,
                cache: ContactHashCache(fileURL: dir.appendingPathComponent("cache.json")),
                reader: NoopContactStoreReader(),
                authAdapter: StubAuthAdapter(.denied),
                configSource: NoopICloudConfigSource(),
                healthRegistry: registry,
                logger: NoopLogger(),
                clock: clock())
            specs.append(PluginSpec(plugin: plugin, mutator: mutator, registry: registry))
        }

        // Anarlog humans — not configured.
        do {
            let (mutator, _) = try makeStateMutator()
            let registry = SourceHealthRegistry()
            let publisher = AnarlogHumansPublisher(
                sender: { _, _ in IngestEventsData(accepted: 0, duplicate: 0, rejected: 0, errors: []) },
                auth: auth(), logger: NoopLogger())
            let plugin = AnarlogHumansSourcePlugin(
                tickInterval: 60,
                piClient: unreachablePiClient(),
                auth: auth(),
                mutator: mutator,
                publisher: publisher,
                filesystem: NoopAnarlogFilesystem(),
                configSource: NoopAnarlogConfigSource(),
                healthRegistry: registry,
                logger: NoopLogger(),
                clock: clock())
            specs.append(PluginSpec(plugin: plugin, mutator: mutator, registry: registry))
        }

        // Anarlog sessions — not configured.
        do {
            let (mutator, _) = try makeStateMutator()
            let registry = SourceHealthRegistry()
            let publisher = AnarlogSessionsPublisher(
                sender: { _, _ in IngestEventsData(accepted: 0, duplicate: 0, rejected: 0, errors: []) },
                auth: auth(), logger: NoopLogger())
            let plugin = AnarlogSessionsSourcePlugin(
                tickInterval: 60,
                piClient: unreachablePiClient(),
                auth: auth(),
                mutator: mutator,
                publisher: publisher,
                filesystem: NoopAnarlogFilesystem(),
                configSource: NoopAnarlogConfigSource(),
                healthRegistry: registry,
                logger: NoopLogger(),
                clock: clock())
            specs.append(PluginSpec(plugin: plugin, mutator: mutator, registry: registry))
        }

        return specs
    }

    // MARK: - filesystem-walk helpers

    /// Resolve `mac-daemon/Sources` from THIS file's compile-time path.
    /// File lives at mac-daemon/Tests/CRMMacConformanceTests/<file>.swift,
    /// so three `deletingLastPathComponent()` reach mac-daemon/.
    private static func resolveSourcesRoot() -> URL {
        URL(fileURLWithPath: #filePath)
            .deletingLastPathComponent() // CRMMacConformanceTests/
            .deletingLastPathComponent() // Tests/
            .deletingLastPathComponent() // mac-daemon/
            .appendingPathComponent("Sources")
    }

    private static func swiftFiles(under root: URL) throws -> [URL] {
        var result: [URL] = []
        guard let enumerator = FileManager.default.enumerator(
            at: root,
            includingPropertiesForKeys: [.isRegularFileKey],
            options: [.skipsHiddenFiles]) else {
            return result
        }
        for case let url as URL in enumerator where url.pathExtension == "swift" {
            result.append(url)
        }
        return result
    }

    /// Extract every `<x>` from `id: SourceID = .<x>` in `code`.
    private static func idCaseLiterals(in code: String) -> [String] {
        guard let regex = try? NSRegularExpression(
            pattern: #"id:\s*SourceID\s*=\s*\.([A-Za-z0-9_]+)"#) else { return [] }
        let ns = code as NSString
        let matches = regex.matches(in: code, range: NSRange(location: 0, length: ns.length))
        return matches.compactMap { m in
            guard m.numberOfRanges > 1 else { return nil }
            return ns.substring(with: m.range(at: 1))
        }
    }

    /// Map a `SourceID.<case>` case name to its SourceID. Exhaustive
    /// over the data-source cases; returns nil for an unknown case so
    /// the test fails loudly rather than silently dropping a conformer.
    private static func sourceID(forCaseName name: String) -> SourceID? {
        switch name {
        case "messages": return .messages
        case "phoneCalls": return .phoneCalls
        case "icloudContacts": return .icloudContacts
        case "anarlogHumans": return .anarlogHumans
        case "anarlogSessions": return .anarlogSessions
        default: return nil
        }
    }
}

// MARK: - in-test stubs

/// Minimal DataSourcePlugin for the extension-default smoke tests. A
/// `final class` because DataSourcePlugin: SourcePlugin and SourcePlugin
/// is `AnyObject` — a value type cannot conform. Mutable counter is
/// guarded by the actor-like isolation of an `@unchecked Sendable`
/// final class accessed only from a single test task; reads happen
/// after the awaited tick() completes.
private final class StubDataSource: DataSourcePlugin, @unchecked Sendable {
    let id: SourceID
    let tickInterval: TimeInterval = 60
    let mutator: StateMutator
    let clock: @Sendable () -> Date
    let logger: LoggerProtocol = NoopLogger()
    private let earlyReturn: Bool
    private var _performTickCalls = 0

    init(id: SourceID, mutator: StateMutator,
         clock: @escaping @Sendable () -> Date, earlyReturn: Bool = false) {
        self.id = id
        self.mutator = mutator
        self.clock = clock
        self.earlyReturn = earlyReturn
    }

    func performTick() async throws {
        _performTickCalls += 1
        if earlyReturn { return }
    }

    /// Read after the awaited tick() completes — no concurrent access.
    var performTickCalls: Int { _performTickCalls }
}

private struct NoopICloudConfigSource: ICloudContactsConfigSource {
    func load() throws -> ICloudContactsConfig? { nil }
}

private final class StubAuthAdapter: ContactsAuthorizationAdapter, @unchecked Sendable {
    private let status: ContactsAuthorizationStatus
    init(_ status: ContactsAuthorizationStatus) { self.status = status }
    func authorizationStatus() -> ContactsAuthorizationStatus { status }
    func requestAccess() async throws -> Bool { false }
}

private struct NoopContactStoreReader: ContactStoreReader {
    func listContainers() throws -> [ContainerInfo] { [] }
    func fullFetch(containerIdentifiers: [String]) throws -> [ContactRecord] { [] }
    func changeHistory(from token: Data?) throws -> ChangeHistoryResult {
        ChangeHistoryResult(events: [], newToken: Data())
    }
    func currentToken() throws -> Data { Data() }
}

private struct NoopAnarlogConfigSource: AnarlogConfigSource {
    func load() throws -> AnarlogConfig? { nil }
}

private struct NoopAnarlogFilesystem: AnarlogFilesystem {
    func exists(_ path: String) -> Bool { false }
    func isDirectory(_ path: String) -> Bool { false }
    func isReadableDirectory(_ path: String) -> Bool { false }
    func listDirectory(_ dir: String) throws -> [String] { [] }
    func readFile(_ path: String) throws -> Data { Data() }
    func mtime(_ path: String) -> Date? { nil }
}
