// PR6 ships two no-op SourcePlugin implementations. They are wired
// into the scheduler by CRMMacLifecycle.PluginRegistry so the
// "daemon ticks and logs" Definition-of-Done item is exercised
// end-to-end, even though no source data is actually read.
//
// PR7 replaces StubMessagesPlugin with a real chat.db reader.
// PR8 replaces StubICloudContactsPlugin with a CNContactStore reader.
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
