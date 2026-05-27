// DaemonRunner composes the long-running daemon process:
//   - registers the heartbeat loop + source plugins with the
//     ScheduleRunner
//   - blocks awaiting SIGTERM (or test-injected cancellation)
//   - on shutdown, cancels all registered plugins and returns
//
// Non-crash exits stay dead per the launchd plist's
// `KeepAlive={Crashed:true}` — the daemon's terminal-error paths
// (401 -> exit 1; 412 -> exit 2) are routed through ExitHandler at
// HeartbeatLoop, not here.
import Foundation
import CRMMacCore

public final class DaemonRunner {
    private let heartbeat: SourcePlugin
    private let plugins: [SourcePlugin]
    private let runner: ScheduleRunner
    private let logger: LoggerProtocol
    private let preShutdown: (@Sendable () async -> Void)?

    public init(
        heartbeat: SourcePlugin,
        plugins: [SourcePlugin],
        runner: ScheduleRunner,
        logger: LoggerProtocol,
        preShutdown: (@Sendable () async -> Void)? = nil
    ) {
        self.heartbeat = heartbeat
        self.plugins = plugins
        self.runner = runner
        self.logger = logger
        self.preShutdown = preShutdown
    }

    /// Start all plugins; await cancellation. Returns when the
    /// caller-provided `awaitShutdown` closure completes.
    ///
    /// The optional `preShutdown` closure (set at construction time)
    /// runs AFTER `awaitShutdown` returns but BEFORE registered
    /// plugins are cancelled. This is the hook the anarlog sessions
    /// FSEvents watcher uses to stop its CFRunLoop stream cleanly
    /// before the actor that owns it is torn down — without this
    /// ordering the watcher can fire a late callback into a
    /// half-cancelled actor.
    public func run(awaitShutdown: () async -> Void) async {
        let registry = PluginRegistry(runner: runner, logger: logger)
        registry.registerAll([heartbeat] + plugins)
        logger.info("daemon: started", metadata: [
            "plugin_count": .public(String(registry.registrationCount)),
        ])
        await awaitShutdown()
        if let preShutdown {
            await preShutdown()
        }
        registry.cancelAll()
        logger.info("daemon: shutdown")
    }
}
