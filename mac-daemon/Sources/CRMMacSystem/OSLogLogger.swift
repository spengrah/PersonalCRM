// OSLogLogger is the production LoggerProtocol implementation. It
// routes messages to os_log under subsystem "xyz.spengrah.crm-mac".
//
// The daemon defaults to .private interpolation for any string-
// valued field; operators can override in Console.app by
// selecting "Include Private Data". We pre-format the metadata into
// the message string using the appropriate os_log privacy modifier
// for each LogValue case.
import Foundation
import os.log
import CRMMacCore

public final class OSLogLogger: LoggerProtocol {
    private let log: OSLog

    public init(subsystem: String = "xyz.spengrah.crm-mac", category: String = "daemon") {
        self.log = OSLog(subsystem: subsystem, category: category)
    }

    public func log(_ level: CRMMacCore.LogLevel, _ message: String, metadata: [String: LogValue]) {
        let osLevel: OSLogType
        switch level {
        case .debug: osLevel = .debug
        case .info: osLevel = .info
        case .warning: osLevel = .default
        case .error: osLevel = .error
        }
        // os_log's string-format interpolation chooses privacy at the
        // call site; we cannot dynamically vary `%{private}@` vs
        // `%{public}@` from a runtime-built format string. To preserve
        // the privacy semantics we render the metadata into the
        // message text up front and emit a single %{public}@ — which
        // does mean operators must trust the daemon's own tagging
        // (LogValue.public vs .private) at compose time. The metadata
        // string format renders private values as "<redacted>" so the
        // bytes that reach the unified log subsystem do not contain
        // identifier material.
        let rendered = Self.compose(message: message, metadata: metadata)
        os_log("%{public}@", log: log, type: osLevel, rendered)
    }

    /// Compose the final log line. Public values are interpolated raw;
    /// private values are rendered as `<redacted>` to keep PII out of
    /// the log stream. The daemon's tagging at the call site (via
    /// LogValue.public / .private) is the authoritative privacy
    /// classifier.
    static func compose(message: String, metadata: [String: LogValue]) -> String {
        guard !metadata.isEmpty else { return message }
        let keys = metadata.keys.sorted()
        let kvs: [String] = keys.map { key in
            switch metadata[key]! {
            case .public(let v): return "\(key)=\(v)"
            case .private: return "\(key)=<redacted>"
            }
        }
        return "\(message) " + kvs.joined(separator: " ")
    }
}
