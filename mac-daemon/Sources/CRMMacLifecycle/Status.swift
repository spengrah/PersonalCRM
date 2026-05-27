// Status implements `crm-mac status`. Shows the daemon's known
// state without making any network call. Reads the agent service
// registration status via AgentService (replaces the previous
// launchctl printService probe).
import Foundation
import CRMMacCore

public struct StatusReport: Equatable {
    public let installed: Bool
    public let registered: Bool
    /// Detailed agent-service registration state. Lets the
    /// rendered output distinguish "not registered" from "requires
    /// approval" — both surface as `registered: false`.
    public let registrationStatus: AgentServiceStatus
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
    /// Per-source status block for the icloud_contacts source.
    /// Surfaces last-tick timestamps + recovery-flag visibility.
    /// Nil when state.sources["icloud_contacts"] is absent.
    public let icloudContacts: ICloudContactsSourceStatus?

    /// Per-source status block for the anarlog_humans source.
    /// Nil when the source is not configured AND state has no entry.
    public let anarlogHumans: AnarlogSourceStatus?

    /// Per-source status block for the anarlog_sessions source.
    public let anarlogSessions: AnarlogSourceStatus?

    public init(
        installed: Bool,
        registered: Bool,
        registrationStatus: AgentServiceStatus,
        configHostname: String?,
        configPiURL: String?,
        hostID: UUID?,
        lastHeartbeatAt: Date?,
        stateSchemaVersion: Int?,
        messages: MessagesSourceStatus? = nil,
        icloudContacts: ICloudContactsSourceStatus? = nil,
        anarlogHumans: AnarlogSourceStatus? = nil,
        anarlogSessions: AnarlogSourceStatus? = nil
    ) {
        self.installed = installed
        self.registered = registered
        self.registrationStatus = registrationStatus
        self.configHostname = configHostname
        self.configPiURL = configPiURL
        self.hostID = hostID
        self.lastHeartbeatAt = lastHeartbeatAt
        self.stateSchemaVersion = stateSchemaVersion
        self.messages = messages
        self.icloudContacts = icloudContacts
        self.anarlogHumans = anarlogHumans
        self.anarlogSessions = anarlogSessions
    }
}

/// Per-source status block for the anarlog reader plugins. Shared
/// shape between humans + sessions — the only difference is the
/// schema_version label.
public struct AnarlogSourceStatus: Equatable {
    public let enabled: Bool
    public let lastScheduledAt: Date?
    public let lastPushedAt: Date?
    /// Decoded count of cursor entries (live + floor-skipped). 0 for
    /// empty cursor; nil when the cursor JSON failed to decode.
    public let cursorUUIDCount: Int?
    /// Recovery flag — lastError starts with `recovery_requested:`.
    public let recoveryRequested: Bool
    public let lastError: String?
    public let schemaVersion: String

    public init(
        enabled: Bool,
        lastScheduledAt: Date?,
        lastPushedAt: Date?,
        cursorUUIDCount: Int?,
        recoveryRequested: Bool,
        lastError: String?,
        schemaVersion: String
    ) {
        self.enabled = enabled
        self.lastScheduledAt = lastScheduledAt
        self.lastPushedAt = lastPushedAt
        self.cursorUUIDCount = cursorUUIDCount
        self.recoveryRequested = recoveryRequested
        self.lastError = lastError
        self.schemaVersion = schemaVersion
    }

    /// Decode a humans cursor string to a UUID count. Returns nil on
    /// malformed input so Status can render `(decode_error)` instead
    /// of silently zero.
    public static func humansCursorUUIDCount(_ cursor: String) -> Int? {
        if cursor.isEmpty { return 0 }
        // Inline decode — Status stays free of any anarlog-target
        // dependency. The shape is the literal `{uuid: {...}}` map
        // per spec line 498; we only need the key count, so we use
        // JSONSerialization without typed decoding.
        guard let data = cursor.data(using: .utf8),
              let parsed = try? JSONSerialization.jsonObject(with: data) as? [String: Any] else {
            return nil
        }
        return parsed.count
    }

    /// Same shape as humansCursorUUIDCount but for the sessions cursor.
    /// Cursor entries with metaHash="floor_skip" are included in the
    /// count — they're real entries, just sentinel ones.
    public static func sessionsCursorUUIDCount(_ cursor: String) -> Int? {
        if cursor.isEmpty { return 0 }
        guard let data = cursor.data(using: .utf8),
              let parsed = try? JSONSerialization.jsonObject(with: data) as? [String: Any] else {
            return nil
        }
        return parsed.count
    }
}

/// Subset of SourceState surfaced via `crm-mac status` for the
/// icloud_contacts source. Mirrors the same shape `MessagesSourceStatus`
/// uses (and keeps Status free of any source-target dependency).
public struct ICloudContactsSourceStatus: Equatable {
    public let lastScheduledAt: Date?
    public let lastPushedAt: Date?
    public let containerCount: Int
    /// Recovery-flag indicator. True when `lastError` starts with
    /// `recovery_requested:` — surfaced prominently in the rendered
    /// status output so the operator sees pending reconciliation.
    public let recoveryRequested: Bool
    /// The literal `lastError` string from SourceState. Nil when the
    /// source is healthy.
    public let lastError: String?

    public init(
        lastScheduledAt: Date?,
        lastPushedAt: Date?,
        containerCount: Int,
        recoveryRequested: Bool,
        lastError: String?
    ) {
        self.lastScheduledAt = lastScheduledAt
        self.lastPushedAt = lastPushedAt
        self.containerCount = containerCount
        self.recoveryRequested = recoveryRequested
        self.lastError = lastError
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
    public let agentService: AgentService

    public init(
        paths: LifecyclePaths,
        filesystem: FilesystemAdapter,
        agentService: AgentService
    ) {
        self.paths = paths
        self.filesystem = filesystem
        self.agentService = agentService
    }
}

public struct Status {
    public let deps: StatusDependencies
    public init(_ deps: StatusDependencies) {
        self.deps = deps
    }

    public func run() -> StatusReport {
        // Installed = the bundle is present on disk. Pre-rewrite this
        // probed the bare-binary path; post-rewrite the bundle path
        // is the source of truth. The legacy-binary case is a stale
        // install in transition; treated as not-yet-installed-with-
        // bundle.
        let installed = deps.filesystem.fileExists(at: deps.paths.bundleAppPath)

        let regStatus = deps.agentService.currentStatus()
        let registered = regStatus == .enabled

        var configHostname: String?
        var configPiURL: String?
        var hostID: UUID?
        var icloudContainerCount = 0
        var anarlogConfig: AnarlogConfig?
        if let data = try? deps.filesystem.read(from: deps.paths.configFilePath) {
            let decoder = JSONDecoder()
            decoder.dateDecodingStrategy = .iso8601
            if let cfg = try? decoder.decode(DaemonConfig.self, from: data) {
                configHostname = cfg.hostname
                configPiURL = cfg.piURL.absoluteString
                hostID = cfg.hostID
                icloudContainerCount = cfg.sources?.icloudContacts?.containers.count ?? 0
                anarlogConfig = cfg.sources?.anarlog
            }
        }

        var lastHeartbeat: Date?
        var schemaVersion: Int?
        var messagesStatus: MessagesSourceStatus?
        var icloudStatus: ICloudContactsSourceStatus?
        var anarlogHumansStatus: AnarlogSourceStatus?
        var anarlogSessionsStatus: AnarlogSourceStatus?
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
                if let icloudSrc = state.sources["icloud_contacts"] {
                    let lastError = icloudSrc.lastError
                    icloudStatus = ICloudContactsSourceStatus(
                        lastScheduledAt: icloudSrc.lastScheduledAt,
                        lastPushedAt: icloudSrc.lastPushedAt,
                        containerCount: icloudContainerCount,
                        recoveryRequested: (lastError ?? "").hasPrefix("recovery_requested:"),
                        lastError: lastError)
                } else if icloudContainerCount > 0 {
                    icloudStatus = ICloudContactsSourceStatus(
                        lastScheduledAt: nil,
                        lastPushedAt: nil,
                        containerCount: icloudContainerCount,
                        recoveryRequested: false,
                        lastError: nil)
                }
                anarlogHumansStatus = makeAnarlogStatus(
                    state: state.sources["anarlog_humans"],
                    enabled: anarlogConfig?.humansEnabled ?? false,
                    cursorDecoder: AnarlogSourceStatus.humansCursorUUIDCount,
                    schemaLabel: "anarlog_humans_v1")
                anarlogSessionsStatus = makeAnarlogStatus(
                    state: state.sources["anarlog_sessions"],
                    enabled: anarlogConfig?.sessionsEnabled ?? false,
                    cursorDecoder: AnarlogSourceStatus.sessionsCursorUUIDCount,
                    schemaLabel: "anarlog_sessions_v1")
            }
        }

        return StatusReport(
            installed: installed,
            registered: registered,
            registrationStatus: regStatus,
            configHostname: configHostname,
            configPiURL: configPiURL,
            hostID: hostID,
            lastHeartbeatAt: lastHeartbeat,
            stateSchemaVersion: schemaVersion,
            messages: messagesStatus,
            icloudContacts: icloudStatus,
            anarlogHumans: anarlogHumansStatus,
            anarlogSessions: anarlogSessionsStatus)
    }

    /// Build an AnarlogSourceStatus from a SourceState slot. Returns
    /// nil only when BOTH the source slot AND the enabled flag are
    /// absent — operators see a status block for anarlog as soon as
    /// they enable it, even before the first tick lands.
    private func makeAnarlogStatus(
        state: SourceState?,
        enabled: Bool,
        cursorDecoder: (String) -> Int?,
        schemaLabel: String
    ) -> AnarlogSourceStatus? {
        if state == nil && !enabled {
            return nil
        }
        let lastError = state?.lastError
        return AnarlogSourceStatus(
            enabled: enabled,
            lastScheduledAt: state?.lastScheduledAt,
            lastPushedAt: state?.lastPushedAt,
            cursorUUIDCount: state.flatMap { cursorDecoder($0.cursor) },
            recoveryRequested: (lastError ?? "").hasPrefix("recovery_requested:"),
            lastError: lastError,
            schemaVersion: schemaLabel)
    }
}
