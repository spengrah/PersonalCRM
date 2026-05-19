// ContainerAllowlistInput parses the operator-supplied
// comma-separated CNContainer identifier list used by both
// `crm-mac install --containers <uuid,uuid,…>` and
// `crm-mac configure containers --containers <uuid,uuid,…>`.
//
// Pure string-massaging: trim whitespace per entry, drop empty
// entries. Validation against the visible CNContainer list
// deliberately does NOT happen here — the non-interactive flow's
// whole point is to skip the (shell-context-broken) enumeration
// step. The daemon's next tick, attributed to the bundle ID under
// launchd, is the authoritative validation point; an unknown UUID
// surfaces there as a recovery-requested failure visible via the
// honest allowlist message in `crm-mac doctor`.
import Foundation

public enum ContainerAllowlistInput {
    /// Split a comma-separated string into trimmed, non-empty
    /// identifier entries. Empty input returns an empty array.
    public static func parse(_ raw: String) -> [String] {
        raw.split(separator: ",", omittingEmptySubsequences: false)
            .map { $0.trimmingCharacters(in: .whitespaces) }
            .filter { !$0.isEmpty }
    }
}
