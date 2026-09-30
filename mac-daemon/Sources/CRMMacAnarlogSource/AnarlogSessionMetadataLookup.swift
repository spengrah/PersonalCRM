// AnarlogSessionMetadataLookup — SessionMetadataLookup adapter that
// reads notification labels through AnarlogCLIClient.
import Foundation
import CRMMacCore
import CRMMacOrphanNotifications

public struct AnarlogSessionMetadataLookup: SessionMetadataLookup {
    private let configSource: AnarlogConfigSource
    private let makeCLIClient: @Sendable (_ cliPath: String?) -> any AnarlogCLIClient

    public init(
        configSource: AnarlogConfigSource,
        makeCLIClient: @escaping @Sendable (_ cliPath: String?) -> any AnarlogCLIClient = CRMMacAnarlogSource.makeCLIClient
    ) {
        self.configSource = configSource
        self.makeCLIClient = makeCLIClient
    }

    public func lookup(sessionUUID: String) async -> SessionMetadata? {
        guard let canonical = AnarlogUUIDValidator.canonicalize(sessionUUID.lowercased()) else {
            return nil
        }
        let config: AnarlogConfig?
        do {
            config = try configSource.load()
        } catch {
            return nil
        }
        guard let cfg = config, cfg.sessionsEnabled else { return nil }
        do {
            guard let record = try await makeCLIClient(cfg.cliPath).getSession(id: canonical) else {
                return nil
            }
            let title = record.title.flatMap { $0.isEmpty ? nil : $0 }
            return SessionMetadata(title: title, createdAt: record.createdAt, sessionDirURL: nil)
        } catch {
            return nil
        }
    }
}
