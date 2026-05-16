// ExitHandler is the indirection that lets the daemon's terminal-
// error paths (401, 412, missing config, etc.) request a non-zero
// exit code WITHOUT actually killing the test process.
//
// The protocol's `exit(_:)` method is `throws Never` semantically —
// it MUST not return normally to the caller. Production impl calls
// Foundation.exit (which truly never returns). The capture-only
// test impl throws ExitRequested so the calling closure unwinds
// without performing further work; tests then assert on the captured
// code.
import Foundation

public struct ExitRequested: Error, Equatable, Sendable {
    public let code: Int32
    public init(code: Int32) {
        self.code = code
    }
}

public protocol ExitHandler: Sendable {
    /// Terminate the process with the given exit code. The production
    /// impl never returns; the capture-only test impl throws
    /// `ExitRequested(code:)` so callers unwind without doing more
    /// work in the test.
    func requestExit(_ code: Int32) throws -> Never
}
