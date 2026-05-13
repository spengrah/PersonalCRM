// ScheduleRunner registers a SourcePlugin for periodic ticks.
// Production impl wraps NSBackgroundActivityScheduler (with a
// DispatchSourceTimer fallback); the fake fires synchronously on
// demand for tests.
//
// Cancellable is returned by register so the caller can cancel a
// specific registration (the daemon cancels all on shutdown).
import Foundation
import CRMMacCore

public protocol Cancellable {
    func cancel()
}

public protocol ScheduleRunner {
    /// Register a plugin. The runner takes ownership of the plugin's
    /// tick cadence; the returned Cancellable lives until the plugin
    /// is unregistered or the daemon shuts down.
    @discardableResult
    func register(_ plugin: SourcePlugin) -> Cancellable

    /// Cancel all currently-registered plugins. Idempotent.
    func cancelAll()
}
