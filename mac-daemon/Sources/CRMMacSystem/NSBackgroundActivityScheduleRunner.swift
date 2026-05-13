// NSBackgroundActivityScheduleRunner wraps NSBackgroundActivityScheduler
// to drive SourcePlugin ticks. This is the default scheduler; the
// DispatchSourceTimer fallback in DispatchSourceScheduleRunner.swift
// is preserved for swap-in if coalescing proves too aggressive once
// source readers are wired up.
//
// Each registered plugin gets its own NSBackgroundActivityScheduler
// instance. The tickInterval is mapped to `interval` and `tolerance`
// is set to ~25% of the interval per Apple's recommendation.
import Foundation
import CRMMacCore
import CRMMacLifecycle

public final class NSBackgroundActivityScheduleRunner: ScheduleRunner {
    private let logger: LoggerProtocol
    private let schedulerFactory: (String) -> NSBackgroundActivityScheduler
    private var registrations: [Registration] = []

    public init(
        logger: LoggerProtocol = NoopLogger(),
        schedulerFactory: @escaping (String) -> NSBackgroundActivityScheduler = { id in
            NSBackgroundActivityScheduler(identifier: id)
        }
    ) {
        self.logger = logger
        self.schedulerFactory = schedulerFactory
    }

    @discardableResult
    public func register(_ plugin: SourcePlugin) -> Cancellable {
        let identifier = "xyz.spengrah.crm-mac.\(plugin.id.rawValue)"
        let scheduler = schedulerFactory(identifier)
        scheduler.repeats = true
        scheduler.interval = plugin.tickInterval
        scheduler.tolerance = plugin.tickInterval * 0.25
        // Box the plugin so the scheduler's escaping closure does not
        // strongly retain `self`.
        let weakLogger = logger
        let registration = Registration(plugin: plugin, scheduler: scheduler)
        scheduler.schedule { completion in
            Task {
                do {
                    try await plugin.tick()
                } catch {
                    weakLogger.error("source tick failed", metadata: [
                        "source": .public(plugin.id.rawValue),
                        "error": .private(String(describing: error)),
                    ])
                }
                completion(.finished)
            }
        }
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
        let scheduler: NSBackgroundActivityScheduler
        private(set) var cancelled = false

        init(plugin: SourcePlugin, scheduler: NSBackgroundActivityScheduler) {
            self.plugin = plugin
            self.scheduler = scheduler
        }

        func cancel() {
            guard !cancelled else { return }
            scheduler.invalidate()
            cancelled = true
        }
    }
}
