// Tests for the in-place pair-key rotation flow.
// Uses a real FileManager + tmp dir for ConfigStore + StateStore
// (the production ConfigStore is a value type that wraps FileManager
// directly — see AllowlistConfigureFlowTests for the same pattern).
// All other adapters are fakes.
import XCTest
import CRMMacCore
@testable import CRMMacLifecycle
@testable import CRMMacPiClient

final class RepairerTests: XCTestCase {
    private var tempDir: URL!
    private var configURL: URL!
    private var stateURL: URL!
    private var apiKeyURL: URL!

    override func setUpWithError() throws {
        try super.setUpWithError()
        tempDir = FileManager.default.temporaryDirectory
            .appendingPathComponent("repairer-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: tempDir, withIntermediateDirectories: true)
        configURL = tempDir.appendingPathComponent("config.json")
        stateURL = tempDir.appendingPathComponent("state.json")
        apiKeyURL = tempDir.appendingPathComponent("api-key")
    }

    override func tearDownWithError() throws {
        try? FileManager.default.removeItem(at: tempDir)
        try super.tearDownWithError()
    }

    // MARK: - helpers

    private func seedConfig(
        hostID: UUID = UUID(uuidString: "11111111-2222-3333-4444-555555555555")!,
        piURL: URL = URL(string: "https://pi.example.test")!
    ) throws {
        let cfg = DaemonConfig(
            piURL: piURL,
            hostID: hostID,
            hostname: "test-host",
            installedAt: Date(timeIntervalSince1970: 1_700_000_000))
        try ConfigStore(fileURL: configURL).save(cfg)
    }

    private func seedEmptyState() throws {
        try StateStore(fileURL: stateURL).initializeIfMissing()
    }

    private func makePaths() -> LifecyclePaths {
        // The Repairer only reads configFilePath and apiKeyFilePath;
        // other paths can be arbitrary as long as they're unique.
        return LifecyclePaths(
            configDirPath: tempDir.path,
            binDirPath: tempDir.appendingPathComponent("bin").path,
            configFilePath: configURL.path,
            stateFilePath: stateURL.path,
            launchAgentsDirPath: tempDir.appendingPathComponent("LaunchAgents").path,
            logsDirPath: tempDir.appendingPathComponent("logs").path,
            stdoutLogPath: tempDir.appendingPathComponent("logs/stdout.log").path,
            stderrLogPath: tempDir.appendingPathComponent("logs/stderr.log").path,
            bundleAppPath: tempDir.appendingPathComponent("crm-mac.app").path,
            bundleBinaryPath: tempDir.appendingPathComponent("crm-mac.app/Contents/MacOS/crm-mac").path,
            bundlePlistPath: tempDir.appendingPathComponent("crm-mac.app/Contents/Library/LaunchAgents/\(Daemon.label).plist").path,
            bundleInfoPlistPath: tempDir.appendingPathComponent("crm-mac.app/Contents/Info.plist").path,
            legacyBinaryPath: tempDir.appendingPathComponent("bin/crm-mac").path,
            legacyPlistPath: tempDir.appendingPathComponent("LaunchAgents/\(Daemon.label).plist").path)
    }

    /// Build a Repairer wired to a `LifecycleMockTransport` that
    /// replays the supplied script. Returns the Repairer + the
    /// transport (for invocation assertions) + the keychain (for
    /// post-test reads).
    private func makeRepairer(
        transport: LifecycleMockTransport,
        keychain: KeychainStore,
        launchctl: LaunchctlRunner
    ) -> Repairer {
        Repairer(RepairerDependencies(
            paths: makePaths(),
            filesystem: InMemoryFilesystem(),
            keychain: keychain,
            configStoreFactory: { url in ConfigStore(fileURL: url) },
            piClientFactory: { url in
                PiClient(
                    baseURL: url,
                    transport: transport.asTransport(),
                    sleep: noopSleep)
            },
            launchctl: launchctl,
            logger: NoopLogger()))
    }

    // MARK: - tests

    func testHappyPathReadsConfigCallsRotateWritesKeyAndKickstartsDaemon() async throws {
        try seedConfig()
        try seedEmptyState()
        let keychain = InMemoryKeychainStore(initial: "old-key")
        let launchctl = FakeLaunchctlRunner()
        let json = Data("""
        {"success": true, "data": {"api_key": "new-key", "api_key_rotated_at": "2026-05-28T12:00:00Z"}}
        """.utf8)
        let transport = LifecycleMockTransport([.respond(status: 200, data: json)])

        let repairer = makeRepairer(transport: transport, keychain: keychain, launchctl: launchctl)
        let result = try await repairer.run(newPairingToken: "fresh-token")

        XCTAssertEqual(transport.invocations.count, 1)
        let req = transport.invocations[0]
        XCTAssertEqual(req.httpMethod, "POST")
        XCTAssertTrue(req.url?.path.hasSuffix("/rotate-key") ?? false)
        XCTAssertEqual(req.value(forHTTPHeaderField: "Authorization"), "Bearer old-key")
        // Body carries the new pairing token.
        XCTAssertNotNil(req.httpBody)
        let bodyJSON = try JSONSerialization.jsonObject(with: req.httpBody!) as? [String: String]
        XCTAssertEqual(bodyJSON?["pairing_token"], "fresh-token")

        XCTAssertEqual(keychain.currentValue, "new-key", "api-key file updated")
        XCTAssertEqual(launchctl.kickstartCalls, [Daemon.label], "kickstart invoked exactly once with daemon label")
        XCTAssertTrue(result.daemonRestartIssued)
        XCTAssertNil(result.restartWarning)
        XCTAssertEqual(result.hostID, UUID(uuidString: "11111111-2222-3333-4444-555555555555"))

        // The parsed Date matches the wire string.
        let formatter = ISO8601DateFormatter()
        formatter.formatOptions = [.withInternetDateTime]
        XCTAssertEqual(result.apiKeyRotatedAt, formatter.date(from: "2026-05-28T12:00:00Z"))
    }

    func testWrongCurrentKeySurfacesAuthRevokedAndDoesNotTouchLocalFiles() async throws {
        try seedConfig()
        let keychain = InMemoryKeychainStore(initial: "old-key")
        let launchctl = FakeLaunchctlRunner()
        let json = Data("""
        {"success": false, "error": {"code": "INVALID_KEY", "message": "auth failed"}}
        """.utf8)
        let transport = LifecycleMockTransport([.respond(status: 401, data: json)])

        let repairer = makeRepairer(transport: transport, keychain: keychain, launchctl: launchctl)
        await XCTAssertThrowsErrorAsync(try await repairer.run(newPairingToken: "fresh")) { err in
            guard case RepairerError.rotateRequestFailed(let underlying) = err else {
                XCTFail("expected rotateRequestFailed, got \(err)")
                return
            }
            guard case .authenticationRevoked = underlying else {
                XCTFail("expected authenticationRevoked, got \(underlying)")
                return
            }
        }
        XCTAssertEqual(keychain.currentValue, "old-key", "api-key file unchanged after auth failure")
        XCTAssertTrue(launchctl.kickstartCalls.isEmpty, "kickstart must not be invoked when rotate fails")
    }

    func testTokenAlreadyConsumedSurfacesTypedError() async throws {
        try seedConfig()
        let keychain = InMemoryKeychainStore(initial: "old-key")
        let launchctl = FakeLaunchctlRunner()
        let json = Data("""
        {"success": false, "error": {"code": "TOKEN_ALREADY_USED", "message": "consumed"}}
        """.utf8)
        let transport = LifecycleMockTransport([.respond(status: 400, data: json)])

        let repairer = makeRepairer(transport: transport, keychain: keychain, launchctl: launchctl)
        await XCTAssertThrowsErrorAsync(try await repairer.run(newPairingToken: "stale")) { err in
            guard case RepairerError.rotateRequestFailed(let underlying) = err else {
                XCTFail("expected rotateRequestFailed, got \(err)")
                return
            }
            guard case .clientError(_, let code, _) = underlying else {
                XCTFail("expected clientError, got \(underlying)")
                return
            }
            XCTAssertEqual(code, "TOKEN_ALREADY_USED")
        }
        XCTAssertEqual(keychain.currentValue, "old-key")
        XCTAssertTrue(launchctl.kickstartCalls.isEmpty)
    }

    func testStaleAuthSurfacesTypedError() async throws {
        try seedConfig()
        let keychain = InMemoryKeychainStore(initial: "old-key")
        let launchctl = FakeLaunchctlRunner()
        let json = Data("""
        {"success": false, "error": {"code": "STALE_AUTH", "message": "rotated by another request"}}
        """.utf8)
        let transport = LifecycleMockTransport([.respond(status: 401, data: json)])

        let repairer = makeRepairer(transport: transport, keychain: keychain, launchctl: launchctl)
        await XCTAssertThrowsErrorAsync(try await repairer.run(newPairingToken: "fresh")) { err in
            guard case RepairerError.rotateRequestFailed(let underlying) = err else {
                XCTFail("expected rotateRequestFailed, got \(err)")
                return
            }
            guard case .clientError(_, let code, _) = underlying else {
                XCTFail("expected clientError(STALE_AUTH), got \(underlying)")
                return
            }
            XCTAssertEqual(code, "STALE_AUTH")
        }
        XCTAssertEqual(keychain.currentValue, "old-key")
    }

    func testPersistFailedAfterRotationPropagatesNewKeyForRecovery() async throws {
        try seedConfig()
        let keychain = FailingWriteKeychainStore(initial: "old-key")
        let launchctl = FakeLaunchctlRunner()
        let json = Data("""
        {"success": true, "data": {"api_key": "new-key", "api_key_rotated_at": "2026-05-28T12:00:00Z"}}
        """.utf8)
        let transport = LifecycleMockTransport([.respond(status: 200, data: json)])

        let repairer = makeRepairer(transport: transport, keychain: keychain, launchctl: launchctl)
        await XCTAssertThrowsErrorAsync(try await repairer.run(newPairingToken: "fresh")) { err in
            guard case RepairerError.persistFailedAfterRotation(_, let plaintext) = err else {
                XCTFail("expected persistFailedAfterRotation, got \(err)")
                return
            }
            XCTAssertEqual(plaintext, "new-key", "recovery prompt must carry the new plaintext")
        }
        XCTAssertEqual(keychain.currentValue, "old-key",
            "old key unchanged on persist failure (atomic-rename guarantee modelled by the fake)")
        XCTAssertTrue(launchctl.kickstartCalls.isEmpty,
            "kickstart must not run when we can't persist the new key — the daemon would restart with the old key and fail auth")
    }

    func testStatePreservation() async throws {
        try seedConfig()
        // Seed state.json with non-trivial content the Repairer must
        // not touch. The Repairer holds no StateStore reference so
        // this is a structural assertion.
        let stateData = Data("""
        {"schema_version": 1, "sources": {"messages": {"some_key": "preserved"}}}
        """.utf8)
        try stateData.write(to: stateURL)
        let preSHA = sha256(stateData)

        let keychain = InMemoryKeychainStore(initial: "old-key")
        let launchctl = FakeLaunchctlRunner()
        let json = Data("""
        {"success": true, "data": {"api_key": "new-key", "api_key_rotated_at": "2026-05-28T12:00:00Z"}}
        """.utf8)
        let transport = LifecycleMockTransport([.respond(status: 200, data: json)])

        let repairer = makeRepairer(transport: transport, keychain: keychain, launchctl: launchctl)
        _ = try await repairer.run(newPairingToken: "fresh")

        let postData = try Data(contentsOf: stateURL)
        XCTAssertEqual(sha256(postData), preSHA, "state.json must be byte-identical after repair")
    }

    func testKickstartFailureBecomesRestartWarningButRotationSucceeds() async throws {
        try seedConfig()
        let keychain = InMemoryKeychainStore(initial: "old-key")
        let launchctl = FakeLaunchctlRunner()
        launchctl.script.kickstart = [1] // non-zero exit
        let json = Data("""
        {"success": true, "data": {"api_key": "new-key", "api_key_rotated_at": "2026-05-28T12:00:00Z"}}
        """.utf8)
        let transport = LifecycleMockTransport([.respond(status: 200, data: json)])

        let repairer = makeRepairer(transport: transport, keychain: keychain, launchctl: launchctl)
        let result = try await repairer.run(newPairingToken: "fresh")

        XCTAssertFalse(result.daemonRestartIssued)
        XCTAssertNotNil(result.restartWarning)
        XCTAssertTrue(result.restartWarning?.contains("exit=1") ?? false,
            "warning should include exit code; got \(result.restartWarning ?? "nil")")
        XCTAssertEqual(keychain.currentValue, "new-key",
            "api-key still rotated even when restart fails")
    }

    func testKickstartThrowsBecomesRestartWarning() async throws {
        try seedConfig()
        let keychain = InMemoryKeychainStore(initial: "old-key")
        let launchctl = FakeLaunchctlRunner()
        struct KickstartFailure: Error {}
        launchctl.kickstartThrowsOnce = KickstartFailure()
        let json = Data("""
        {"success": true, "data": {"api_key": "new-key", "api_key_rotated_at": "2026-05-28T12:00:00Z"}}
        """.utf8)
        let transport = LifecycleMockTransport([.respond(status: 200, data: json)])

        let repairer = makeRepairer(transport: transport, keychain: keychain, launchctl: launchctl)
        let result = try await repairer.run(newPairingToken: "fresh")

        XCTAssertFalse(result.daemonRestartIssued)
        XCTAssertNotNil(result.restartWarning)
        XCTAssertTrue(result.restartWarning?.contains("threw") ?? false)
        XCTAssertEqual(keychain.currentValue, "new-key")
    }

    func testNoExistingConfigSurfacesClearError() async throws {
        // No config.json on disk.
        let keychain = InMemoryKeychainStore(initial: "old-key")
        let launchctl = FakeLaunchctlRunner()
        let transport = LifecycleMockTransport([])

        let repairer = makeRepairer(transport: transport, keychain: keychain, launchctl: launchctl)
        await XCTAssertThrowsErrorAsync(try await repairer.run(newPairingToken: "fresh")) { err in
            guard case RepairerError.noExistingInstall(let reason) = err else {
                XCTFail("expected noExistingInstall, got \(err)")
                return
            }
            XCTAssertTrue(reason.contains("config.json"))
        }
        XCTAssertTrue(transport.invocations.isEmpty,
            "PiClient must not be invoked when local config is missing")
        XCTAssertTrue(launchctl.kickstartCalls.isEmpty)
    }

    func testNoExistingAPIKeySurfacesClearError() async throws {
        try seedConfig()
        let keychain = InMemoryKeychainStore(initial: nil) // throws .notFound on read
        let launchctl = FakeLaunchctlRunner()
        let transport = LifecycleMockTransport([])

        let repairer = makeRepairer(transport: transport, keychain: keychain, launchctl: launchctl)
        await XCTAssertThrowsErrorAsync(try await repairer.run(newPairingToken: "fresh")) { err in
            guard case RepairerError.noExistingInstall(let reason) = err else {
                XCTFail("expected noExistingInstall, got \(err)")
                return
            }
            XCTAssertTrue(reason.contains("api-key"))
        }
        XCTAssertTrue(transport.invocations.isEmpty)
    }

    func testResponseDateParseFailureSurfacesTypedError() async throws {
        try seedConfig()
        let keychain = InMemoryKeychainStore(initial: "old-key")
        let launchctl = FakeLaunchctlRunner()
        let json = Data("""
        {"success": true, "data": {"api_key": "new-key", "api_key_rotated_at": "not-a-date"}}
        """.utf8)
        let transport = LifecycleMockTransport([.respond(status: 200, data: json)])

        let repairer = makeRepairer(transport: transport, keychain: keychain, launchctl: launchctl)
        await XCTAssertThrowsErrorAsync(try await repairer.run(newPairingToken: "fresh")) { err in
            guard case RepairerError.responseDateParseFailed(let raw) = err else {
                XCTFail("expected responseDateParseFailed, got \(err)")
                return
            }
            XCTAssertEqual(raw, "not-a-date")
        }
        // The api-key WAS written to disk (step 4 happens before step 5).
        XCTAssertEqual(keychain.currentValue, "new-key",
            "api-key is written before the date parse so the operator's recovery path is `crm-mac start`")
        XCTAssertTrue(launchctl.kickstartCalls.isEmpty,
            "kickstart skipped because the date-parse throw occurs before the kickstart step")
    }

    func testApiKeyRotatedAtParsesWithFractionalSeconds() async throws {
        try seedConfig()
        let keychain = InMemoryKeychainStore(initial: "old-key")
        let launchctl = FakeLaunchctlRunner()
        let json = Data("""
        {"success": true, "data": {"api_key": "new-key", "api_key_rotated_at": "2026-05-28T12:00:00.123456Z"}}
        """.utf8)
        let transport = LifecycleMockTransport([.respond(status: 200, data: json)])

        let repairer = makeRepairer(transport: transport, keychain: keychain, launchctl: launchctl)
        let result = try await repairer.run(newPairingToken: "fresh")
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        XCTAssertEqual(result.apiKeyRotatedAt, f.date(from: "2026-05-28T12:00:00.123456Z"))
    }

    func testApiKeyRotatedAtParsesWithoutFractionalSeconds() async throws {
        try seedConfig()
        let keychain = InMemoryKeychainStore(initial: "old-key")
        let launchctl = FakeLaunchctlRunner()
        let json = Data("""
        {"success": true, "data": {"api_key": "new-key", "api_key_rotated_at": "2026-05-28T12:00:00Z"}}
        """.utf8)
        let transport = LifecycleMockTransport([.respond(status: 200, data: json)])

        let repairer = makeRepairer(transport: transport, keychain: keychain, launchctl: launchctl)
        let result = try await repairer.run(newPairingToken: "fresh")
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime]
        XCTAssertEqual(result.apiKeyRotatedAt, f.date(from: "2026-05-28T12:00:00Z"))
    }
}

// MARK: - test fakes

/// Keychain fake whose write fails on every call. Used by the
/// persist-failed-after-rotation test. Read returns the seeded value;
/// writes throw; current value stays unchanged so the test can assert
/// the old-key-preserved invariant.
private final class FailingWriteKeychainStore: KeychainStore, @unchecked Sendable {
    private let initialValue: String?

    init(initial: String?) { self.initialValue = initial }

    func readAPIKey() throws -> String {
        guard let v = initialValue else { throw KeychainStoreError.notFound }
        return v
    }

    func writeAPIKey(_ value: String) throws {
        _ = value
        throw KeychainStoreError.other("simulated write failure")
    }

    func deleteAPIKey() throws {}

    var currentValue: String? { initialValue }
}

// MARK: - utilities

private func sha256(_ data: Data) -> String {
    // Avoid CryptoKit import overhead; FNV-style hash is sufficient
    // here — the test only needs change detection, not crypto.
    var h: UInt64 = 1469598103934665603
    for b in data {
        h ^= UInt64(b)
        h &*= 1099511628211
    }
    return String(h, radix: 16)
}

// Async XCTAssertThrowsError helper.
func XCTAssertThrowsErrorAsync<T>(
    _ expression: @autoclosure () async throws -> T,
    file: StaticString = #filePath,
    line: UInt = #line,
    _ errorHandler: (_ error: Error) -> Void = { _ in }
) async {
    do {
        _ = try await expression()
        XCTFail("expected throw", file: file, line: line)
    } catch {
        errorHandler(error)
    }
}
