import Foundation
@testable import CRMMacLifecycle

/// Capture-only ExitHandler. Records the requested code and throws
/// ExitRequested so the calling closure unwinds without doing more
/// work in the test process.
public final class CapturingExitHandler: ExitHandler {
    public private(set) var capturedCodes: [Int32] = []

    public init() {}

    public func requestExit(_ code: Int32) throws -> Never {
        capturedCodes.append(code)
        throw ExitRequested(code: code)
    }
}
