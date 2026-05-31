// AnarlogSessionMetadataLookup — concrete SessionMetadataLookup
// adapter that reads session metadata from the local filesystem.
//
// Composes AnarlogConfigSource + AnarlogPathResolver +
// AnarlogSessionMetaParser. Returns nil for any failure (config
// missing/disabled, sessions root missing, session dir missing,
// _meta.json missing/unreadable/unparseable) — the caller renders
// "Untitled session" with no time suffix in those cases.
//
// Lives in CRMMacAnarlogSource (not CRMMacOrphanNotifications) so
// the dep graph stays acyclic: anarlog → notifications, never the
// reverse.
import Foundation
import CRMMacCore
import CRMMacOrphanNotifications

public struct AnarlogSessionMetadataLookup: SessionMetadataLookup {
    private let configSource: AnarlogConfigSource
    private let filesystem: AnarlogFilesystem

    public init(
        configSource: AnarlogConfigSource,
        filesystem: AnarlogFilesystem
    ) {
        self.configSource = configSource
        self.filesystem = filesystem
    }

    public func lookup(sessionUUID: String) async -> SessionMetadata? {
        // Canonicalize the UUID first — defend against callers
        // passing uppercase or otherwise-noncanonical input. The
        // session directory on disk uses the lowercased form.
        guard let canonical = AnarlogUUIDValidator.canonicalize(sessionUUID.lowercased()) else {
            return nil
        }
        // Config must be loadable + sessions enabled. (sessionsEnabled
        // is the operator's master switch for the anarlog_sessions
        // plugin; if it's off, we don't read the filesystem at all.)
        let config: AnarlogConfig?
        do {
            config = try configSource.load()
        } catch {
            return nil
        }
        guard let cfg = config, cfg.sessionsEnabled else {
            return nil
        }
        let sessionsDir = AnarlogPathResolver.sessionsDir(rootPath: cfg.rootPath)
        guard filesystem.isDirectory(sessionsDir.path) else { return nil }

        let sessionDir = sessionsDir.appendingPathComponent(canonical, isDirectory: true)
        guard filesystem.isDirectory(sessionDir.path) else { return nil }

        let metaPath = sessionDir.appendingPathComponent("_meta.json", isDirectory: false).path
        guard filesystem.exists(metaPath) else {
            // Session dir exists but no _meta.json yet — still
            // return the sessionDirURL as session metadata (the
            // orphan click launches the Anarlog app, not Finder).
            // Title/time stay nil.
            return SessionMetadata(
                title: nil, createdAt: nil, sessionDirURL: sessionDir)
        }
        let metaBytes: Data
        do {
            metaBytes = try filesystem.readFile(metaPath)
        } catch {
            return SessionMetadata(
                title: nil, createdAt: nil, sessionDirURL: sessionDir)
        }
        guard let meta = AnarlogSessionMetaParser.parse(
            uuid: canonical, metaJSONBytes: metaBytes) else {
            return SessionMetadata(
                title: nil, createdAt: nil, sessionDirURL: sessionDir)
        }
        let title: String? = meta.title.isEmpty ? nil : meta.title
        return SessionMetadata(
            title: title,
            createdAt: meta.createdAt,
            sessionDirURL: sessionDir)
    }
}
