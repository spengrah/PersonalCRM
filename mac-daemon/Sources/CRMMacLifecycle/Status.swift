// Status implements `crm-mac status`. Shows the daemon's known
// state without making any network call. The
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
    /// Per-source status block for the messages source. Decoded from
    /// state.sources["messages"].cursor on a best-effort basis;
    /// nil if no cursor has been committed yet OR the cursor JSON
    /// fails to decode.
    public let messages: MessagesSourceStatus?

    public init(
        installed: Bool,
        registered: Bool,
        registeredExitCode: Int32,
        configHostname: String?,
        configPiURL: String?,
        hostID: UUID?,
        lastHeartbeatAt: Date?,
        stateSchemaVersion: Int?,
        messages: MessagesSourceStatus? = nil
    ) {
        self.installed = installed
        self.registered = registered
        self.registeredExitCode = registeredExitCode
        self.configHostname = configHostname
        self.configPiURL = configPiURL
        self.hostID = hostID
        self.lastHeartbeatAt = lastHeartbeatAt
        self.stateSchemaVersion = stateSchemaVersion
        self.messages = messages
    }
}

/// Subset of MessagesCursor surfaced via `crm-mac status`. The full
/// MessagesCursor struct lives in CRMMacMessagesSource (depends on
/// GRDB); we decode just the cursor watermarks here so Status stays
/// free of any GRDB-dependent target.
public struct MessagesSourceStatus: Equatable {
    public let liveCursor: Int64?
    public let backfillCursor: Int64?
    public let installMaxRowID: Int64?
    public let backfillComplete: Bool
    public let pendingScansCount: Int

    public init(
        liveCursor: Int64?,
        backfillCursor: Int64?,
        installMaxRowID: Int64?,
        backfillComplete: Bool,
        pendingScansCount: Int
    ) {
        self.liveCursor = liveCursor
        self.backfillCursor = backfillCursor
        self.installMaxRowID = installMaxRowID
        self.backfillComplete = backfillComplete
        self.pendingScansCount = pendingScansCount
    }

    /// Decoder shape mirrors the Pi-side opaque cursor JSON. Not
    /// shared with CRMMacMessagesSource on purpose — keeps Status
    /// free of any GRDB-dependent target.
    private struct CursorJSON: Decodable {
        let backfillCursor: Int64?
        let liveCursor: Int64?
        let installMaxRowID: Int64?
        let backfillComplete: Bool?
        let pendingScans: [PendingScan]?

        enum CodingKeys: String, CodingKey {
            case backfillCursor      = "backfill_cursor"
            case liveCursor          = "live_cursor"
            case installMaxRowID     = "install_max_rowid"
            case backfillComplete    = "backfill_complete"
            case pendingScans        = "pending_scans"
        }

        struct PendingScan: Decodable {
            let normalizedHandle: String?
            let since: Date?
            enum CodingKeys: String, CodingKey {
                case normalizedHandle = "normalized_handle"
                case since
            }
        }
    }

    /// Best-effort decode from the opaque cursor string.  Nil on
    /// empty input or decode failure.
    public static func decode(opaqueCursor: String) -> MessagesSourceStatus? {
        if opaqueCursor.isEmpty { return nil }
        let decoder = JSONDecoder()
        decoder.dateDecodingStrategy = .iso8601
        guard let parsed = try? decoder.decode(CursorJSON.self,
                                                from: Data(opaqueCursor.utf8)) else {
            return nil
        }
        return MessagesSourceStatus(
            liveCursor: parsed.liveCursor,
            backfillCursor: parsed.backfillCursor,
            installMaxRowID: parsed.installMaxRowID,
            backfillComplete: parsed.backfillComplete ?? false,
            pendingScansCount: parsed.pendingScans?.count ?? 0)
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
        var messagesStatus: MessagesSourceStatus?
        if let data = try? deps.filesystem.read(from: deps.paths.stateFilePath) {
            let decoder = JSONDecoder()
            decoder.dateDecodingStrategy = .iso8601
            if let state = try? decoder.decode(DaemonState.self, from: data) {
                lastHeartbeat = state.lastHeartbeatAt
                schemaVersion = state.schemaVersion
                if let messagesSrc = state.sources["messages"] {
                    messagesStatus = MessagesSourceStatus.decode(
                        opaqueCursor: messagesSrc.cursor)
                }
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
            stateSchemaVersion: schemaVersion,
            messages: messagesStatus)
    }
}
