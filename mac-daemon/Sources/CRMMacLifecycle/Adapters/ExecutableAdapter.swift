// ExecutableAdapter encapsulates the two binary-resolution operations
// the installer needs:
//   - "what is the absolute path of the currently-running crm-mac?"
//   - "codesign this path"
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
    /// Single-Mach-O ad-hoc sign. Used during the legacy migration
    /// cleanup path only — the bare-binary install pre-PR8c.
    /// `codesign -s - --force --preserve-metadata=... <path>`.
    func adhocCodesign(path: String) throws
    /// Two-pass sign of a complete `.app` bundle:
    ///   1. `codesign --force --sign <identity> --identifier <id> <bundle>/Contents/MacOS/crm-mac`
    ///   2. `codesign --force --sign <identity> --identifier <id> <bundle>`
    /// Production uses `CRM_MAC_CODESIGN_IDENTITY` when set, otherwise
    /// falls back to ad-hoc signing (`--sign -`) for CI/local smoke
    /// builds. The explicit `--identifier` keeps the inner Mach-O from
    /// retaining a build-host-derived identifier.
    func codesignBundle(bundlePath: String, identifier: String) throws
}
