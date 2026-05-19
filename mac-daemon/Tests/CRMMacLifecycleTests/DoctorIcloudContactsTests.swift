// DoctorIcloudContactsTests cover the three icloud_contacts checks
// Doctor runs:
//   1. Contacts permission (via ContactsAuthorizationAdapter stub).
//   2. Allowlist sanity (via ContactContainerEnumerator stub).
//   3. Last-tick age (max(lastScheduledAt, lastPushedAt) vs
//      2× tickInterval).
//
// gcontacts overlap is NOT tested — that check requires a Pi-side
// active-providers endpoint the daemon doesn't yet talk to and is
// out of scope for the v1 icloud source.
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

    func testPermissionDeniedWarnsAsIndeterminate() async throws {
        // `.denied` from a shell-spawned doctor reflects only the
        // parent terminal's TCC state, not the daemon's — so the
        // doctor reports it as WARN with a "daemon is authoritative"
        // hint instead of FAIL.
        let r = await runDoctor(authStatus: .denied)
        let check = r.results.first(where: { $0.name == "icloud_contacts.permission" })!
        XCTAssertEqual(check.status, .warn)
        XCTAssertTrue(
            check.details.contains("indeterminate from shell context"),
            "expected indeterminate-from-shell-context wording, got: \(check.details)")
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
        XCTAssertTrue(
            check.details.contains("indeterminate from shell context"),
            "expected indeterminate-from-shell-context wording, got: \(check.details)")
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

    func testAllowlistEnumerationNotAuthorizedEmitsShellContextWarning() async throws {
        // Regression for #321: a shell-spawned doctor previously
        // caught `.notAuthorized` and set `visible = []`, turning
        // every configured ID into a phantom "orphan". The new code
        // reports the enumeration as unavailable instead, AND falls
        // through to the last-tick check.
        let now = Date(timeIntervalSince1970: 2_000_000_000)
        let r = await runDoctor(
            allowlist: ["c1", "c2"],
            enumerator: StubContactContainerEnumerator(
                thrownError: ContactContainerEnumeratorError.notAuthorized),
            sourceState: SourceState(lastScheduledAt: now.addingTimeInterval(-30)),
            clock: FixedClock(now))

        let check = r.results.first(where: { $0.name == "icloud_contacts.allowlist" })!
        XCTAssertEqual(check.status, .warn)
        XCTAssertTrue(
            check.details.contains("2 configured"),
            "expected count of configured IDs, got: \(check.details)")
        XCTAssertTrue(
            check.details.contains("visibility check unavailable from shell context"),
            "expected shell-context wording, got: \(check.details)")
        XCTAssertFalse(
            check.details.contains("orphaned"),
            "must NOT report orphans when enumeration is unavailable")

        // Regression assertion: the legacy `return results` path
        // suppressed last_tick when enumeration failed. The
        // refactored flow must emit it.
        XCTAssertNotNil(
            r.results.first(where: { $0.name == "icloud_contacts.last_tick" }),
            "icloud_contacts.last_tick must be present even when enumeration is unavailable")
    }

    func testAllowlistUnderlyingEnumeratorErrorStillWarnsGenerically() async throws {
        // Regression coverage for the previously-latent `.underlying`
        // early-return bug: a generic enumeration failure used to
        // short-circuit out of checkICloudContacts before the
        // last-tick block. The refactored flow must surface BOTH the
        // generic-enumeration WARN and the last-tick result.
        let now = Date(timeIntervalSince1970: 2_000_000_000)
        let r = await runDoctor(
            allowlist: ["c1"],
            enumerator: StubContactContainerEnumerator(
                thrownError: ContactContainerEnumeratorError.underlying("disk gremlins")),
            sourceState: SourceState(lastScheduledAt: now.addingTimeInterval(-30)),
            clock: FixedClock(now))

        let check = r.results.first(where: { $0.name == "icloud_contacts.allowlist" })!
        XCTAssertEqual(check.status, .warn)
        XCTAssertTrue(
            check.details.contains("container enumeration failed"),
            "expected generic enumeration-failed wording, got: \(check.details)")

        XCTAssertNotNil(
            r.results.first(where: { $0.name == "icloud_contacts.last_tick" }),
            "icloud_contacts.last_tick must be present even on generic enumeration failure")
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
        var script = FakeAgentService.Script()
        script.statusSequence = [.enabled]
        let deps = DoctorDependencies(
            paths: paths,
            filesystem: fs,
            keychain: InMemoryKeychainStore(initial: "key"),
            agentService: FakeAgentService(script: script),
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
