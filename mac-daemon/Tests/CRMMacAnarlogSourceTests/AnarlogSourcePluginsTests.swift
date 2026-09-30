import Foundation
import XCTest
import CRMMacCore
import CRMMacOrphanNotifications
import CRMMacPiClient
@testable import CRMMacAnarlogSource

private struct StubConfigSource: AnarlogConfigSource {
    let config: AnarlogConfig
    func load() throws -> AnarlogConfig? { config }
}

private final class RecordingPresenter: UserNotificationPresenter, @unchecked Sendable {
    private let lock = NSLock()
    private var identifiers: [String] = []

    func requestAuthorization() async -> Bool { true }

    func add(_ spec: NotificationRequestSpec) async throws {
        lock.withLock {
            identifiers.append(spec.identifier)
        }
    }

    func removeDelivered(withIdentifiers ids: [String]) async {}
    func removePending(withIdentifiers ids: [String]) async {}
    func setDelegate(_ ref: UserNotificationDelegateRef?) async {}
    func getDeliveredIdentifiers() async -> [String] { [] }
    func getPendingIdentifiers() async -> [String] { [] }

    func recordedIdentifiers() -> [String] {
        lock.lock()
        defer { lock.unlock() }
        return identifiers
    }
}

final class AnarlogSourcePluginsTests: XCTestCase {

    func testBothPluginsReportToOneHealthNotifier() async throws {
        for humansFirst in [true, false] {
            let directory = FileManager.default.temporaryDirectory
                .appendingPathComponent("anarlog-source-plugins-\(UUID().uuidString)")
            try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
            defer { try? FileManager.default.removeItem(at: directory) }
            let presenter = RecordingPresenter()
            let plugins = try makePlugins(
                cliPath: directory.appendingPathComponent("missing-anarlog").path,
                presenter: presenter)

            if humansFirst {
                try await plugins.humans.performTick()
            } else {
                try await plugins.sessions.performTick()
            }
            XCTAssertEqual(presenter.recordedIdentifiers(), ["anarlog_source:broken:binary_not_found"])

            if humansFirst {
                try await plugins.sessions.performTick()
            } else {
                try await plugins.humans.performTick()
            }
            XCTAssertEqual(presenter.recordedIdentifiers(), ["anarlog_source:broken:binary_not_found"])
        }
    }

    func testBothPluginsRunTheConfiguredCLI() async throws {
        let directory = FileManager.default.temporaryDirectory
            .appendingPathComponent("anarlog-source-cli-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }

        let script = directory.appendingPathComponent("argv-recorder")
        let log = directory.appendingPathComponent("argv.log")
        let scriptText = """
        #!/bin/sh
        printf '%s\\n' "$@" >> "\(log.path)"
        exit 1
        """
        try Data(scriptText.utf8).write(to: script)
        try FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: script.path)

        let plugins = try makePlugins(cliPath: script.path, presenter: RecordingPresenter())
        try await plugins.humans.performTick()
        try await plugins.sessions.performTick()

        let lines = try String(contentsOf: log, encoding: .utf8)
            .split(whereSeparator: \.isNewline).map(String.init)
        let expected = FakeAnarlogCLI.listArgv(offset: 0) + FakeAnarlogCLI.listArgv(offset: 0)
        XCTAssertEqual(lines, expected)
    }

    private func makePlugins(
        cliPath: String,
        presenter: UserNotificationPresenter
    ) throws -> AnarlogSourcePlugins {
        var config = AnarlogConfig(humansEnabled: true, sessionsEnabled: true)
        try config.setOperatorPersonID("aaaaaaaa-0000-4000-8000-000000000001")
        try config.setCLIPath(cliPath)

        let logger = NoopLogger()
        let auth = PiAuth(
            hostID: UUID(uuidString: "11111111-2222-3333-4444-555555555555")!,
            apiKey: "synthetic-key")
        let piClient = PiClient(
            baseURL: URL(string: "https://synthetic.invalid")!,
            transport: { request in
                let response = HTTPURLResponse(
                    url: request.url!, statusCode: 500, httpVersion: "HTTP/1.1", headerFields: nil)!
                return (Data(), response)
            },
            logger: logger)
        let stateURL = URL(fileURLWithPath: cliPath)
            .deletingLastPathComponent()
            .appendingPathComponent("state.json")
        let store = StateStore(fileURL: stateURL)
        try store.initializeIfMissing(hostID: auth.hostID)
        let mutator = StateMutator(store: store)
        let clock: @Sendable () -> Date = { Date(timeIntervalSince1970: 2_000_000_000) }
        let humansPublisher = AnarlogHumansPublisher(
            sender: { _, _ in throw URLError(.unsupportedURL) },
            auth: auth,
            logger: logger,
            clock: clock)
        let sessionsPublisher = AnarlogSessionsPublisher(
            sender: { _, _ in throw URLError(.unsupportedURL) },
            auth: auth,
            logger: logger,
            clock: clock)

        return AnarlogSourcePlugins(
            piClient: piClient,
            auth: auth,
            mutator: mutator,
            humansPublisher: humansPublisher,
            sessionsPublisher: sessionsPublisher,
            configSource: StubConfigSource(config: config),
            healthRegistry: SourceHealthRegistry(),
            orphanNotificationCenter: nil,
            presenter: presenter,
            logger: logger,
            clock: clock)
    }
}
