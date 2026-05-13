// Two no-op SourcePlugin implementations wired into the scheduler by
// CRMMacLifecycle.PluginRegistry. They exist so the "daemon ticks and
// logs" smoke is exercised end-to-end, even though no source data is
// actually read. Real source readers replace these as they land
// (Apple Messages chat.db reader, CNContactStore reader).
import Foundation

public final class StubMessagesPlugin: SourcePlugin {
    public let id: SourceID = .messages
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
