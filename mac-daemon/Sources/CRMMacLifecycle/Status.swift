// Status implements `crm-mac status`. Plan D12 / D14: shows the
// daemon's known state without making any network call. The
// launchctl status is an exit-code-only signal — we do NOT parse the
// `state = running` line from launchctl print (that's informational,
// not API).
import Foundation
import CRMMacCore

public struct StatusReport: Equatable {
    public let installed: Bool
    public let registered: Bool
    public let registeredExitCode: Int32
    public let configHostname: String?
    public let configPiURL: String?
    public let hostID: UUID?
    public let lastHeartbeatAt: Date?
    public let stateSchemaVersion: Int?

    public init(
        installed: Bool,
        registered: Bool,
        registeredExitCode: Int32,
        configHostname: String?,
        configPiURL: String?,
        hostID: UUID?,
        lastHeartbeatAt: Date?,
        stateSchemaVersion: Int?
    ) {
        self.installed = installed
        self.registered = registered
        self.registeredExitCode = registeredExitCode
        self.configHostname = configHostname
        self.configPiURL = configPiURL
        self.hostID = hostID
        self.lastHeartbeatAt = lastHeartbeatAt
        self.stateSchemaVersion = stateSchemaVersion
    }
}

public struct StatusDependencies {
    public let paths: LifecyclePaths
    public let filesystem: FilesystemAdapter
    public let launchctl: LaunchctlRunner

    public init(
        paths: LifecyclePaths,
        filesystem: FilesystemAdapter,
        launchctl: LaunchctlRunner
    ) {
        self.paths = paths
        self.filesystem = filesystem
        self.launchctl = launchctl
    }
}

public struct Status {
    public let deps: StatusDependencies
    public init(_ deps: StatusDependencies) {
        self.deps = deps
    }

    public func run() -> StatusReport {
        let installed = deps.filesystem.fileExists(at: deps.paths.binaryPath)

        var registered = false
        var registeredExit: Int32 = -1
        if let inv = try? deps.launchctl.printService(label: Daemon.label) {
            registeredExit = inv.exitCode
            registered = inv.exitCode == 0
        }

        var configHostname: String?
        var configPiURL: String?
        var hostID: UUID?
        if let data = try? deps.filesystem.read(from: deps.paths.configFilePath) {
            let decoder = JSONDecoder()
            decoder.dateDecodingStrategy = .iso8601
            if let cfg = try? decoder.decode(DaemonConfig.self, from: data) {
                configHostname = cfg.hostname
                configPiURL = cfg.piURL.absoluteString
                hostID = cfg.hostID
            }
        }

        var lastHeartbeat: Date?
        var schemaVersion: Int?
        if let data = try? deps.filesystem.read(from: deps.paths.stateFilePath) {
            let decoder = JSONDecoder()
            decoder.dateDecodingStrategy = .iso8601
            if let state = try? decoder.decode(DaemonState.self, from: data) {
                lastHeartbeat = state.lastHeartbeatAt
                schemaVersion = state.schemaVersion
            }
        }

        return StatusReport(
            installed: installed,
            registered: registered,
            registeredExitCode: registeredExit,
            configHostname: configHostname,
            configPiURL: configPiURL,
            hostID: hostID,
            lastHeartbeatAt: lastHeartbeat,
            stateSchemaVersion: schemaVersion)
    }
}
