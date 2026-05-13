// DaemonRunner composes the long-running daemon process:
//   - registers the heartbeat loop + source plugins with the
//     ScheduleRunner
//   - blocks awaiting SIGTERM (or test-injected cancellation)
//   - on shutdown, cancels all registered plugins and returns
//
// Plan A4. Per D14, non-crash exits stay dead — the daemon's
// terminal-error paths (401 -> exit 1; 412 -> exit 2) are routed
// through ExitHandler at HeartbeatLoop, not here.
import Foundation
import CRMMacCore

public final class DaemonRunner {
    private let heartbeat: SourcePlugin
    private let plugins: [SourcePlugin]
    private let runner: ScheduleRunner
    private let logger: LoggerProtocol

    public init(
        heartbeat: SourcePlugin,
        plugins: [SourcePlugin],
        runner: ScheduleRunner,
        logger: LoggerProtocol
    ) {
        self.heartbeat = heartbeat
        self.plugins = plugins
        self.runner = runner
        self.logger = logger
    }

    /// Start all plugins; await cancellation. Returns when the
    /// caller-provided `awaitShutdown` closure completes.
    public func run(awaitShutdown: () async -> Void) async {
        let registry = PluginRegistry(runner: runner, logger: logger)
        registry.registerAll([heartbeat] + plugins)
        logger.info("daemon: started", metadata: [
            "plugin_count": .public(String(registry.registrationCount)),
        ])
        await awaitShutdown()
        registry.cancelAll()
        logger.info("daemon: shutdown")
    }
}
