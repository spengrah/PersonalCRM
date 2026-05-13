// ExecutableAdapter encapsulates the two binary-resolution operations
// the installer needs:
//   - "what is the absolute path of the currently-running crm-mac?"
//   - "ad-hoc codesign this path"
// Production impl uses Bundle.main.executablePath + a Process
// shell-out to `codesign`; tests inject a fake that records calls.
import Foundation

public enum ExecutableAdapterError: Error, Equatable, CustomStringConvertible {
    case notFound
    case codesignFailed(String)

    public var description: String {
        switch self {
        case .notFound: return "current executable path unavailable"
        case .codesignFailed(let m): return "codesign failed: \(m)"
        }
    }
}

public protocol ExecutableAdapter {
    /// Absolute path of the currently-running binary. Used by the
    /// installer to stage `crm-mac` from wherever the operator
    /// invoked it.
    func currentExecutablePath() throws -> String
    /// Run `codesign -s - --force --preserve-metadata=... <path>`.
    func adhocCodesign(path: String) throws
}
