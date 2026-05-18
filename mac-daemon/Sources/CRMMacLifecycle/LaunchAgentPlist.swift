// Pure-string LaunchAgent plist generator. The XML render is the
// same fixture format tested in CRMMacSystemTests/LaunchAgentPlistTests,
// just relocated to CRMMacLifecycle so the Installer can call it
// without depending on system frameworks.
//
// All path values are XML-escaped before interpolation so unusual
// home-directory characters (`&`, `<`, `>`, `'`, `"`) can't produce
// an invalid plist.
import Foundation

public struct LaunchAgentPlist: Equatable {
    public let label: String
    public let binaryPath: String
    public let configDirPath: String
    public let stdoutPath: String
    public let stderrPath: String

    public init(
        label: String,
        binaryPath: String,
        configDirPath: String,
        stdoutPath: String,
        stderrPath: String
    ) {
        self.label = label
        self.binaryPath = binaryPath
        self.configDirPath = configDirPath
        self.stdoutPath = stdoutPath
        self.stderrPath = stderrPath
    }

    public func render() -> String {
        let l = xmlEscape(label)
        let bin = xmlEscape(binaryPath)
        let cfg = xmlEscape(configDirPath)
        let out = xmlEscape(stdoutPath)
        let err = xmlEscape(stderrPath)
        return """
        <?xml version="1.0" encoding="UTF-8"?>
        <!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
        <plist version="1.0">
        <dict>
            <key>Label</key>
            <string>\(l)</string>
            <key>ProgramArguments</key>
            <array>
                <string>\(bin)</string>
                <string>daemon</string>
            </array>
            <key>RunAtLoad</key>
            <true/>
            <key>KeepAlive</key>
            <dict>
                <key>Crashed</key>
                <true/>
            </dict>
            <key>ProcessType</key>
            <string>Background</string>
            <key>StandardOutPath</key>
            <string>\(out)</string>
            <key>StandardErrorPath</key>
            <string>\(err)</string>
            <key>EnvironmentVariables</key>
            <dict>
                <key>CRM_MAC_CONFIG_DIR</key>
                <string>\(cfg)</string>
            </dict>
        </dict>
        </plist>

        """
    }
}

/// Minimal XML escaping for plist `<string>` values. Replaces the
/// five XML-significant characters so an unusual home directory
/// path (e.g., `/Users/o'malley`) cannot produce a malformed plist.
/// Public so the installer can apply the same escaping when
/// substituting `__INSTALL_PREFIX__` post-render — every path that
/// lands inside `<string>...</string>` must be escaped, regardless
/// of whether it went through `LaunchAgentPlist.render()` or was
/// interpolated afterwards.
public func xmlEscapePlistString(_ s: String) -> String {
    var out = ""
    out.reserveCapacity(s.count)
    for c in s {
        switch c {
        case "&": out.append("&amp;")
        case "<": out.append("&lt;")
        case ">": out.append("&gt;")
        case "\"": out.append("&quot;")
        case "'": out.append("&apos;")
        default: out.append(c)
        }
    }
    return out
}

/// Internal alias preserved for the existing `LaunchAgentPlist.render`
/// call sites.
private func xmlEscape(_ s: String) -> String { xmlEscapePlistString(s) }
