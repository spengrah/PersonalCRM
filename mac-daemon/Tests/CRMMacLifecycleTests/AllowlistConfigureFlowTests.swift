// Tests for `AllowlistConfigureFlow` — the dispatcher that decides,
// per Mode, whether to invoke the Contacts framework adapters
// before writing the allowlist. The control-flow boundary
// assertion is the regression guard for issue #322: non-interactive
// modes must make ZERO calls to the auth adapter or container
// enumerator.
//
// Coverage matrix (11 rows total):
//
//   | Mode                                | Picked vs existing | Auth | Enum | Outcome                  |
//   |-------------------------------------|--------------------|------|------|--------------------------|
//   | freshInstallNonInteractive          | differ             | 0    | 0    | .wrote                   |
//   | reRequestPermissionNonInteractive   | differ             | 0    | 0    | .wrote                   |
//   | configureNonInteractive             | differ             | 0    | 0    | .wrote                   |
//   | configureNonInteractive             | equal (no-op)      | 0    | 0    | .noOp                    |
//   | freshInstallInteractive             | (no existing)      | 1    | 1    | .completedInteractive    |
//   | reRequestPermissionInteractive      | differ             | 1    | 1    | .completedInteractive    |
//   | reRequestPermissionInteractive      | equal (no-op)      | 1    | 1    | .noOp                    |
//   | configureInteractive                | differ             | 1    | 1    | .completedInteractive    |
//   | configureInteractive                | equal (no-op)      | 1    | 1    | .noOp                    |
//   | configureList                       | n/a                | 0    | 1    | .listed                  |
//   | configureInteractive (denied)       | n/a                | 1    | 0    | throws .permissionDenied |
import XCTest
import Foundation
import CRMMacCore
@testable import CRMMacLifecycle

final class AllowlistConfigureFlowTests: XCTestCase {
    private var tempDir: URL!
    private var stateURL: URL!
    private var configURL: URL!

    override func setUpWithError() throws {
        try super.setUpWithError()
        tempDir = FileManager.default.temporaryDirectory
            .appendingPathComponent("allowlist-flow-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: tempDir, withIntermediateDirectories: true)
        stateURL = tempDir.appendingPathComponent("state.json")
        configURL = tempDir.appendingPathComponent("config.json")
    }

    override func tearDownWithError() throws {
        try? FileManager.default.removeItem(at: tempDir)
        try super.tearDownWithError()
    }

    // MARK: - helpers

    private func seedConfig(containers: [String]?) throws {
        let sources: DaemonSourcesConfig? = containers.map {
            DaemonSourcesConfig(icloudContacts: ICloudContactsConfig(containers: $0))
        }
        let cfg = DaemonConfig(
            piURL: URL(string: "https://test.invalid")!,
            hostID: UUID(uuidString: "11111111-2222-3333-4444-555555555555")!,
            hostname: "host",
            installedAt: Date(timeIntervalSince1970: 1_700_000_000),
            sources: sources)
        try ConfigStore(fileURL: configURL).save(cfg)
    }

    private func seedEmptyState() throws {
        try StateStore(fileURL: stateURL).initializeIfMissing()
    }

    private func makeFlow(
        mode: AllowlistConfigureFlow.Mode,
        authSpy: CallCountingContactsAuth,
        enumSpy: CallCountingContactEnumerator,
        picker: (@Sendable ([ContainerInfo]) throws -> [String])? = nil
    ) -> AllowlistConfigureFlow {
        let defaultPicker: @Sendable ([ContainerInfo]) throws -> [String] = { visible in
            visible.map(\.identifier)
        }
        return AllowlistConfigureFlow(
            mode: mode,
            configStore: ConfigStore(fileURL: configURL),
            stateStore: StateStore(fileURL: stateURL),
            authAdapter: { authSpy },
            enumerator: { enumSpy },
            interactivePicker: picker ?? defaultPicker)
    }

    private func readState() throws -> DaemonState {
        try StateStore(fileURL: stateURL).load()
    }

    // MARK: - non-interactive: zero Contacts calls (issue #322 regression guard)

    func testFreshInstallNonInteractiveMakesZeroContactsCalls() async throws {
        try seedConfig(containers: nil)
        try seedEmptyState()
        let authSpy = CallCountingContactsAuth()
        let enumSpy = CallCountingContactEnumerator()
        let flow = makeFlow(
            mode: .freshInstallNonInteractive(rawContainers: "uuid-1"),
            authSpy: authSpy, enumSpy: enumSpy)

        let outcome = try await flow.run()

        XCTAssertEqual(outcome, .wrote(pickedIDs: ["uuid-1"]))
        XCTAssertEqual(authSpy.authStatusCalls, 0)
        XCTAssertEqual(authSpy.requestAccessCalls, 0,
                       "non-interactive must not call requestAccess()")
        XCTAssertEqual(enumSpy.listContainersCalls, 0,
                       "non-interactive must not call listContainers()")
    }

    func testReRequestPermissionNonInteractiveMakesZeroContactsCalls() async throws {
        try seedConfig(containers: ["old-1"])
        try seedEmptyState()
        let authSpy = CallCountingContactsAuth()
        let enumSpy = CallCountingContactEnumerator()
        let flow = makeFlow(
            mode: .reRequestPermissionNonInteractive(rawContainers: "new-1"),
            authSpy: authSpy, enumSpy: enumSpy)

        let outcome = try await flow.run()

        XCTAssertEqual(outcome, .wrote(pickedIDs: ["new-1"]))
        XCTAssertEqual(authSpy.requestAccessCalls, 0)
        XCTAssertEqual(enumSpy.listContainersCalls, 0)
    }

    func testConfigureNonInteractiveMakesZeroContactsCalls() async throws {
        try seedConfig(containers: ["old-1"])
        try seedEmptyState()
        let authSpy = CallCountingContactsAuth()
        let enumSpy = CallCountingContactEnumerator()
        let flow = makeFlow(
            mode: .configureNonInteractive(rawContainers: "new-1"),
            authSpy: authSpy, enumSpy: enumSpy)

        let outcome = try await flow.run()

        XCTAssertEqual(outcome, .wrote(pickedIDs: ["new-1"]))
        XCTAssertEqual(authSpy.requestAccessCalls, 0)
        XCTAssertEqual(enumSpy.listContainersCalls, 0)
    }

    func testConfigureNonInteractiveEqualSetReturnsNoOp() async throws {
        try seedConfig(containers: ["X"])
        try seedEmptyState()
        let authSpy = CallCountingContactsAuth()
        let enumSpy = CallCountingContactEnumerator()
        let flow = makeFlow(
            mode: .configureNonInteractive(rawContainers: "X"),
            authSpy: authSpy, enumSpy: enumSpy)

        let outcome = try await flow.run()

        XCTAssertEqual(outcome, .noOp)
        XCTAssertEqual(authSpy.requestAccessCalls, 0)
        XCTAssertEqual(enumSpy.listContainersCalls, 0)
        let state = try readState()
        XCTAssertNil(state.sources["icloud_contacts"]?.lastError,
                     "no-op must not bump recovery flag")
    }

    // MARK: - interactive: must call auth + enumerator

    func testFreshInstallInteractiveCallsAuthAndEnumerator() async throws {
        try seedConfig(containers: nil)
        try seedEmptyState()
        let authSpy = CallCountingContactsAuth(grantOnRequest: true)
        let enumSpy = CallCountingContactEnumerator(containers: [
            ContainerInfo(identifier: "c1", name: "iCloud", type: .cardDAV, defaultIncluded: true),
        ])
        let flow = makeFlow(
            mode: .freshInstallInteractive,
            authSpy: authSpy, enumSpy: enumSpy)

        let outcome = try await flow.run()

        if case .completedInteractive(let ids) = outcome {
            XCTAssertEqual(ids, ["c1"])
        } else {
            XCTFail("expected .completedInteractive, got \(outcome)")
        }
        XCTAssertEqual(authSpy.requestAccessCalls, 1)
        XCTAssertEqual(enumSpy.listContainersCalls, 1)
    }

    func testReRequestPermissionInteractiveCallsAuthAndEnumeratorWhenDiffers() async throws {
        try seedConfig(containers: ["old"])
        try seedEmptyState()
        let authSpy = CallCountingContactsAuth(grantOnRequest: true)
        let enumSpy = CallCountingContactEnumerator(containers: [
            ContainerInfo(identifier: "new", name: "iCloud", type: .cardDAV, defaultIncluded: true),
        ])
        let flow = makeFlow(
            mode: .reRequestPermissionInteractive,
            authSpy: authSpy, enumSpy: enumSpy)

        let outcome = try await flow.run()

        if case .completedInteractive(let ids) = outcome {
            XCTAssertEqual(ids, ["new"])
        } else {
            XCTFail("expected .completedInteractive, got \(outcome)")
        }
        XCTAssertEqual(authSpy.requestAccessCalls, 1)
        XCTAssertEqual(enumSpy.listContainersCalls, 1)
    }

    func testReRequestPermissionInteractiveEqualSetReturnsNoOp() async throws {
        // Regression guard for the codex-round-3 P1: interactive
        // no-op MUST propagate as Outcome.noOp, not collapse to
        // .completedInteractive. The CLI wrapper's outcome switch
        // distinguishes between "Allowlist updated" and "No
        // allowlist changes detected" on this case.
        try seedConfig(containers: ["X"])
        try seedEmptyState()
        let authSpy = CallCountingContactsAuth(grantOnRequest: true)
        let enumSpy = CallCountingContactEnumerator(containers: [
            ContainerInfo(identifier: "X", name: "iCloud", type: .cardDAV, defaultIncluded: true),
        ])
        let flow = makeFlow(
            mode: .reRequestPermissionInteractive,
            authSpy: authSpy, enumSpy: enumSpy)

        let outcome = try await flow.run()

        XCTAssertEqual(outcome, .noOp)
        XCTAssertEqual(authSpy.requestAccessCalls, 1)
        XCTAssertEqual(enumSpy.listContainersCalls, 1)
        let state = try readState()
        XCTAssertNil(state.sources["icloud_contacts"]?.lastError,
                     "interactive no-op must not bump recovery flag")
    }

    func testConfigureInteractiveCallsAuthAndEnumerator() async throws {
        try seedConfig(containers: ["old"])
        try seedEmptyState()
        let authSpy = CallCountingContactsAuth(grantOnRequest: true)
        let enumSpy = CallCountingContactEnumerator(containers: [
            ContainerInfo(identifier: "new", name: "iCloud", type: .cardDAV, defaultIncluded: true),
        ])
        let flow = makeFlow(
            mode: .configureInteractive,
            authSpy: authSpy, enumSpy: enumSpy)

        let outcome = try await flow.run()

        if case .completedInteractive(let ids) = outcome {
            XCTAssertEqual(ids, ["new"])
        } else {
            XCTFail("expected .completedInteractive, got \(outcome)")
        }
        XCTAssertEqual(authSpy.requestAccessCalls, 1)
        XCTAssertEqual(enumSpy.listContainersCalls, 1)
    }

    func testConfigureInteractiveEqualSetReturnsNoOp() async throws {
        // Regression guard for the codex-round-3 P1: see the
        // re-request-permission counterpart above.
        try seedConfig(containers: ["X"])
        try seedEmptyState()
        let authSpy = CallCountingContactsAuth(grantOnRequest: true)
        let enumSpy = CallCountingContactEnumerator(containers: [
            ContainerInfo(identifier: "X", name: "iCloud", type: .cardDAV, defaultIncluded: true),
        ])
        let flow = makeFlow(
            mode: .configureInteractive,
            authSpy: authSpy, enumSpy: enumSpy)

        let outcome = try await flow.run()

        XCTAssertEqual(outcome, .noOp)
        XCTAssertEqual(authSpy.requestAccessCalls, 1)
        XCTAssertEqual(enumSpy.listContainersCalls, 1)
        let state = try readState()
        XCTAssertNil(state.sources["icloud_contacts"]?.lastError,
                     "interactive no-op must not bump recovery flag")
    }

    // MARK: - configureList: enumerate only

    func testConfigureListCallsEnumeratorButNotAuth() async throws {
        try seedConfig(containers: nil)
        try seedEmptyState()
        let authSpy = CallCountingContactsAuth()
        let enumSpy = CallCountingContactEnumerator(containers: [
            ContainerInfo(identifier: "c1", name: "iCloud", type: .cardDAV, defaultIncluded: true),
        ])
        let flow = makeFlow(
            mode: .configureList,
            authSpy: authSpy, enumSpy: enumSpy)

        let outcome = try await flow.run()

        if case .listed(let visible) = outcome {
            XCTAssertEqual(visible.count, 1)
            XCTAssertEqual(visible.first?.identifier, "c1")
        } else {
            XCTFail("expected .listed, got \(outcome)")
        }
        XCTAssertEqual(authSpy.requestAccessCalls, 0,
                       "--list is a read-only enumerate; no permission prompt")
        XCTAssertEqual(enumSpy.listContainersCalls, 1)
    }

    // MARK: - permission-denied

    func testInteractivePermissionDeniedThrows() async throws {
        try seedConfig(containers: nil)
        try seedEmptyState()
        let authSpy = CallCountingContactsAuth(grantOnRequest: false)
        let enumSpy = CallCountingContactEnumerator()
        let flow = makeFlow(
            mode: .configureInteractive,
            authSpy: authSpy, enumSpy: enumSpy)

        do {
            _ = try await flow.run()
            XCTFail("expected AllowlistConfigureFlowError.permissionDenied")
        } catch AllowlistConfigureFlowError.permissionDenied {
            // expected
        }
        XCTAssertEqual(authSpy.requestAccessCalls, 1)
        XCTAssertEqual(enumSpy.listContainersCalls, 0,
                       "enumeration must not run when permission is denied")
    }
}
