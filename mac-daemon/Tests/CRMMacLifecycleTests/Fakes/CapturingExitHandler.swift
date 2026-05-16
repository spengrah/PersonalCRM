import Foundation
@testable import CRMMacLifecycle

/// Capture-only ExitHandler. Records the requested code and throws
/// ExitRequested so the calling closure unwinds without doing more
/// work in the test process.
///
/// `@unchecked Sendable` because ExitHandler is Sendable-constrained
/// under Swift 6 but the recording fake has mutable state. Tests
/// drive it from a single async context per scenario so the lack of
/// locking is intentional.
public final class CapturingExitHandler: ExitHandler, @unchecked Sendable {
    public private(set) var capturedCodes: [Int32] = []

    public init() {}

    public func requestExit(_ code: Int32) throws -> Never {
        capturedCodes.append(code)
        throw ExitRequested(code: code)
    }
}
