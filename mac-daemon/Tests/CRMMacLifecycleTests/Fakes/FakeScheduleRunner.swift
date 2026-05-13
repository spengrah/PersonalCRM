import Foundation
import CRMMacCore
@testable import CRMMacLifecycle

/// Records registered plugins; tests call `fire(id:)` to invoke one
/// tick of a single plugin synchronously.
public final class FakeScheduleRunner: ScheduleRunner {
    public final class Registration: Cancellable {
        public let plugin: SourcePlugin
        public private(set) var cancelled = false
        init(_ plugin: SourcePlugin) {
            self.plugin = plugin
        }
        public func cancel() {
            cancelled = true
        }
    }

    public private(set) var registrations: [Registration] = []

    public init() {}

    @discardableResult
    public func register(_ plugin: SourcePlugin) -> Cancellable {
        let r = Registration(plugin)
        registrations.append(r)
        return r
    }

    public func cancelAll() {
        for r in registrations { r.cancel() }
    }

    /// Fire a single tick on the named plugin. Returns true if found,
    /// false otherwise.
    @discardableResult
    public func fire(id: SourceID) async throws -> Bool {
        for r in registrations where !r.cancelled && r.plugin.id == id {
            try await r.plugin.tick()
            return true
        }
        return false
    }

    public func cancelledCount() -> Int {
        registrations.filter { $0.cancelled }.count
    }
}
