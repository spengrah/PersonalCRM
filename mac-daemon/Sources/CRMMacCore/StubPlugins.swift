// No-op SourcePlugin implementation wired into the scheduler by
// CRMMacLifecycle.PluginRegistry. Exists so the "daemon ticks and
// logs" smoke is exercised end-to-end for iCloud Contacts even
// though no source data is actually read yet. The real reader lands
// in PR8.
//
// The Messages stub was removed in PR7 alongside the real
// MessagesSourcePlugin landing.
import Foundation

public final class StubICloudContactsPlugin: SourcePlugin {
    public let id: SourceID = .icloudContacts
    public let tickInterval: TimeInterval

    private let context: SourceContext

    public init(context: SourceContext, tickInterval: TimeInterval = 60) {
        self.context = context
        self.tickInterval = tickInterval
    }

    public func tick() async throws {
        context.logger.info("source tick", metadata: [
            "source": .public(id.rawValue),
            "stub": .public("true"),
        ])
    }
}
