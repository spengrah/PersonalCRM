// DispatchSourceScheduleRunner is the fallback scheduler.
//
// Status: not currently selected by ProductionContext (the daemon
// always uses NSBackgroundActivityScheduleRunner). It ships
// pre-built so we can swap it in via a one-line ProductionContext
// edit if observability after source-reader wiring shows NSBA's
// coalescing is too aggressive for our 60s ticks. Removing it now
// would force a future PR to rediscover the right protocol shape
// and re-derive the DispatchSourceTimer interaction; keeping it
// here is the cheap option.
//
// Same protocol shape as NSBackgroundActivityScheduleRunner.
import Foundation
import CRMMacCore
import CRMMacLifecycle

public final class DispatchSourceScheduleRunner: ScheduleRunner {
    private let logger: LoggerProtocol
    private let queue: DispatchQueue
    private var registrations: [Registration] = []

    public init(
        logger: LoggerProtocol = NoopLogger(),
        queue: DispatchQueue = DispatchQueue(label: "xyz.spengrah.crm-mac.scheduler")
    ) {
        self.logger = logger
        self.queue = queue
    }

    @discardableResult
    public func register(_ plugin: SourcePlugin) -> Cancellable {
        let timer = DispatchSource.makeTimerSource(queue: queue)
        timer.schedule(
            deadline: .now() + plugin.tickInterval,
            repeating: plugin.tickInterval)
        let registration = Registration(plugin: plugin, timer: timer)
        let weakLogger = logger
        timer.setEventHandler {
            Task {
                do {
                    try await plugin.tick()
                } catch {
                    weakLogger.error("source tick failed", metadata: [
                        "source": .public(plugin.id.rawValue),
                        "error": .private(String(describing: error)),
                    ])
                }
            }
        }
        timer.resume()
        registrations.append(registration)
        return registration
    }

    public func cancelAll() {
        for r in registrations {
            r.cancel()
        }
        registrations.removeAll()
    }

    fileprivate final class Registration: Cancellable {
        let plugin: SourcePlugin
        let timer: DispatchSourceTimer
        private(set) var cancelled = false

        init(plugin: SourcePlugin, timer: DispatchSourceTimer) {
            self.plugin = plugin
            self.timer = timer
        }

        func cancel() {
            guard !cancelled else { return }
            timer.cancel()
            cancelled = true
        }
    }
}
