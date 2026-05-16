// Logger abstraction. CRMMacCore stays Foundation-only — the os.Log
// production impl lives in CRMMacSystem and conforms to this protocol.
//
// LogValue carries a privacy hint per the os.log conventions:
// .private for identifier-shaped values, .public for known-safe
// values (component names, status codes). The production wrapper
// uses these to choose the correct interpolation modifier.
import Foundation

/// Privacy-tagged string value. `.private` redacts on the system log
/// stream by default; operators can override in Console.app.
public enum LogValue: Equatable, Sendable {
    case `public`(String)
    case `private`(String)

    /// Returns the underlying string regardless of privacy tier. Used
    /// by the NoopLogger and by tests; production impls should respect
    /// the tier.
    public var stringValue: String {
        switch self {
        case .public(let s): return s
        case .private(let s): return s
        }
    }
}

public enum LogLevel: Sendable {
    case debug
    case info
    case warning
    case error
}

/// Logger protocol consumed by every CRMMac* target. Defaults below
/// route to `log(level:_:metadata:)`; production conformers only need
/// implement that single method.
///
/// `Sendable` constraint: loggers are passed to actors and escaping
/// closures (scheduler runners, source plugins, the heartbeat loop).
/// Production conformers (`OSLogLogger`, `StdoutLogger`) are stateless
/// or wrap thread-safe Apple APIs; the no-op conformer is trivially safe.
public protocol LoggerProtocol: AnyObject, Sendable {
    func log(_ level: LogLevel, _ message: String, metadata: [String: LogValue])
}

extension LoggerProtocol {
    public func debug(_ message: String, metadata: [String: LogValue] = [:]) {
        log(.debug, message, metadata: metadata)
    }
    public func info(_ message: String, metadata: [String: LogValue] = [:]) {
        log(.info, message, metadata: metadata)
    }
    public func warning(_ message: String, metadata: [String: LogValue] = [:]) {
        log(.warning, message, metadata: metadata)
    }
    public func error(_ message: String, metadata: [String: LogValue] = [:]) {
        log(.error, message, metadata: metadata)
    }
}

/// Default no-op logger. Used in tests; never wired in production.
public final class NoopLogger: LoggerProtocol {
    public init() {}
    public func log(_ level: LogLevel, _ message: String, metadata: [String: LogValue]) {
        _ = level; _ = message; _ = metadata
    }
}
