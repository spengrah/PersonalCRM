import Foundation
import CRMMacCore
@testable import CRMMacLifecycle

/// Records registered and cancelled plugins.
public final class FakeScheduleRunner: ScheduleRunner, @unchecked Sendable {
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

    public func cancelledCount() -> Int {
        registrations.filter { $0.cancelled }.count
    }
}
