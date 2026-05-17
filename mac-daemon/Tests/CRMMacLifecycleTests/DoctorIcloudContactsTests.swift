// DoctorIcloudContactsTests cover the three icloud_contacts checks
// PR8b adds to Doctor:
//   1. Contacts permission (via ContactsAuthorizationAdapter stub).
//   2. Allowlist sanity (via ContactContainerEnumerator stub).
//   3. Last-tick age (max(lastScheduledAt, lastPushedAt) vs 2× tickInterval).
//
// gcontacts overlap is NOT tested — deferred to a follow-up PR (see
// plan Known v1 limitations item 6).
import XCTest
import CRMMacCore
@testable import CRMMacLifecycle
@testable import CRMMacPiClient

final class DoctorIcloudContactsTests: XCTestCase {

    // MARK: - permission

    func testPermissionAuthorizedPasses() async throws {
        let r = await runDoctor(authStatus: .authorized)
        let check = r.results.first(where: { $0.name == "icloud_contacts.permission" })!
        XCTAssertEqual(check.status, .pass)
    }

    func testPermissionLimitedPasses() async throws {
        let r = await runDoctor(authStatus: .limited)
        let check = r.results.first(where: { $0.name == "icloud_contacts.permission" })!
        XCTAssertEqual(check.status, .pass)
    }

    func testPermissionDeniedFails() async throws {
        let r = await runDoctor(authStatus: .denied)
        let check = r.results.first(where: { $0.name == "icloud_contacts.permission" })!
        XCTAssertEqual(check.status, .fail)
    }

    func testPermissionRestrictedFails() async throws {
        let r = await runDoctor(authStatus: .restricted)
        let check = r.results.first(where: { $0.name == "icloud_contacts.permission" })!
        XCTAssertEqual(check.status, .fail)
    }

    func testPermissionNotDeterminedWarns() async throws {
        let r = await runDoctor(authStatus: .notDetermined)
        let check = r.results.first(where: { $0.name == "icloud_contacts.permission" })!
        XCTAssertEqual(check.status, .warn)
    }

    // MARK: - allowlist

    func testAllowlistEmptyWarns() async throws {
        let r = await runDoctor()
        let check = r.results.first(where: { $0.name == "icloud_contacts.allowlist" })!
        XCTAssertEqual(check.status, .warn)
    }

    func testAllowlistAllVisiblePasses() async throws {
        let containers = [
            ContainerInfo(identifier: "c1", name: "iCloud", type: .cardDAV, defaultIncluded: true),
            ContainerInfo(identifier: "c2", name: "On My Mac", type: .local, defaultIncluded: true),
        ]
        let r = await runDoctor(
            allowlist: ["c1", "c2"],
            enumerator: StubContactContainerEnumerator(containers: containers))
        let check = r.results.first(where: { $0.name == "icloud_contacts.allowlist" })!
        XCTAssertEqual(check.status, .pass)
    }

    func testAllowlistOrphanWarns() async throws {
        let containers = [
            ContainerInfo(identifier: "c1", name: "iCloud", type: .cardDAV, defaultIncluded: true),
        ]
        // c2 is configured but not in the visible enumerator list.
        let r = await runDoctor(
            allowlist: ["c1", "c2"],
            enumerator: StubContactContainerEnumerator(containers: containers))
        let check = r.results.first(where: { $0.name == "icloud_contacts.allowlist" })!
        XCTAssertEqual(check.status, .warn)
        XCTAssertTrue(check.details.contains("c2"))
    }

    // MARK: - last_tick

    func testLastTickAgeWithinBoundsPasses() async throws {
        let now = Date(timeIntervalSince1970: 2_000_000_000)
        let recent = now.addingTimeInterval(-30) // 30s ago, tickInterval=60 → 2x=120
        let r = await runDoctor(
            sourceState: SourceState(lastScheduledAt: recent),
            clock: FixedClock(now))
        let check = r.results.first(where: { $0.name == "icloud_contacts.last_tick" })!
        XCTAssertEqual(check.status, .pass)
    }

    func testLastTickAgeBeyondThresholdWarns() async throws {
        let now = Date(timeIntervalSince1970: 2_000_000_000)
        let stale = now.addingTimeInterval(-300) // 5 minutes ago > 2*60s
        let r = await runDoctor(
            sourceState: SourceState(lastScheduledAt: stale),
            clock: FixedClock(now))
        let check = r.results.first(where: { $0.name == "icloud_contacts.last_tick" })!
        XCTAssertEqual(check.status, .warn)
    }

    func testLastTickUsesMaxOfScheduledAndPushed() async throws {
        let now = Date(timeIntervalSince1970: 2_000_000_000)
        let staleScheduled = now.addingTimeInterval(-300)
        let recentPushed = now.addingTimeInterval(-30)
        let r = await runDoctor(
            sourceState: SourceState(
                lastScheduledAt: staleScheduled,
                lastPushedAt: recentPushed),
            clock: FixedClock(now))
        let check = r.results.first(where: { $0.name == "icloud_contacts.last_tick" })!
        XCTAssertEqual(check.status, .pass,
                       "max(lastScheduledAt, lastPushedAt) picks recent value")
    }

    func testLastTickNoSourceStateWarns() async throws {
        let r = await runDoctor(sourceState: nil)
        let check = r.results.first(where: { $0.name == "icloud_contacts.last_tick" })!
        XCTAssertEqual(check.status, .warn)
    }

    // MARK: - test rig

    private func runDoctor(
        authStatus: ContactsAuthorizationStatus = .authorized,
        allowlist: [String] = [],
        enumerator: ContactContainerEnumerator? = nil,
        sourceState: SourceState? = nil,
        clock: ClockAdapter? = nil
    ) async -> DoctorReport {
        let paths = TestPaths.make()
        let fs = InMemoryFilesystem()
        let config = DaemonConfig(
            piURL: URL(string: "https://pi.example.test")!,
            hostID: UUID(uuidString: "11111111-2222-3333-4444-555555555555")!,
            hostname: "mac-1",
            installedAt: Date(timeIntervalSince1970: 1_700_000_000),
            sources: allowlist.isEmpty ? nil :
                DaemonSourcesConfig(icloudContacts: ICloudContactsConfig(containers: allowlist)))
        var state = DaemonState(schemaVersion: 1, hostID: config.hostID)
        if let s = sourceState {
            state.sources["icloud_contacts"] = s
        }
        let encoder = JSONEncoder()
        encoder.dateEncodingStrategy = .iso8601
        try! fs.write(try! encoder.encode(config), to: paths.configFilePath)
        try! fs.write(try! encoder.encode(state), to: paths.stateFilePath)
        var script = FakeLaunchctlRunner.Script()
        script.printService = [0]
        let deps = DoctorDependencies(
            paths: paths,
            filesystem: fs,
            keychain: InMemoryKeychainStore(initial: "key"),
            launchctl: FakeLaunchctlRunner(script: script),
            piClientFactory: { url in
                PiClient(
                    baseURL: url,
                    transport: LifecycleMockTransport([.respond(status: 200, data: known200JSON)]).asTransport(),
                    sleep: noopSleep)
            },
            contactsAuth: StubContactsAuthorizationAdapter(status: authStatus),
            containerEnumerator: enumerator ?? StubContactContainerEnumerator(),
            tickInterval: 60,
            clock: clock ?? FixedClock(),
            logger: NoopLogger())
        return await Doctor(deps).run()
    }
}
