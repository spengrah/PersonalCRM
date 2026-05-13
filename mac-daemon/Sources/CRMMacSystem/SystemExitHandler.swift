// SystemExitHandler is the production ExitHandler. Calls
// Foundation.exit directly; never returns. The test harness uses
// CapturingExitHandler from CRMMacLifecycleTests/Fakes instead.
import Foundation
import CRMMacLifecycle

public struct SystemExitHandler: ExitHandler {
    public init() {}
    public func requestExit(_ code: Int32) throws -> Never {
        exit(code)
    }
}
