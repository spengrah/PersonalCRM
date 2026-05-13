// DispatchSourceScheduleRunner is the fallback scheduler — uses
// DispatchSourceTimer directly so PR7/PR8 can swap it in if NSBA
// coalescing turns out to be too aggressive. Same protocol shape as
// NSBackgroundActivityScheduleRunner.
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
