// PluginRegistry is the composition point where the daemon's stub
// plugins (PR6) and real plugins (PR7+) are registered against the
// ScheduleRunner. PR6's registry registers the StubMessagesPlugin
// and StubICloudContactsPlugin from CRMMacCore — they log a no-op
// tick on the configured cadence so the spec § PR6 Definition-of-Done
// item "stub scheduler jobs fire on schedule" is exercised.
import Foundation
import CRMMacCore

public final class PluginRegistry {
    private let runner: ScheduleRunner
    private let logger: LoggerProtocol
    private var registrations: [Cancellable] = []

    public init(runner: ScheduleRunner, logger: LoggerProtocol) {
        self.runner = runner
        self.logger = logger
    }

    /// Register every plugin in `plugins` against the scheduler. Each
    /// returned Cancellable is retained so cancelAll() can tear them
    /// down on shutdown.
    public func registerAll(_ plugins: [SourcePlugin]) {
        for plugin in plugins {
            let reg = runner.register(plugin)
            registrations.append(reg)
            logger.info("plugin registered", metadata: [
                "source": .public(plugin.id.rawValue),
                "tick_interval": .public(String(plugin.tickInterval)),
            ])
        }
    }

    /// Cancel every registered plugin (idempotent).
    public func cancelAll() {
        for r in registrations { r.cancel() }
        registrations.removeAll()
        runner.cancelAll()
    }

    public var registrationCount: Int { registrations.count }
}
