// AnarlogTimestampParser handles the timestamp-format drift the
// parent spec calls out (`tolerate timestamp format drift`, spec line
// 187). Anarlog's `_meta.json.created_at` and human-frontmatter
// `created_at` have been observed in multiple shapes across versions:
//
//   - `"2026-03-16T20:34:49.936Z"`           (milliseconds + Z)
//   - `"2026-03-16T20:34:49Z"`                (seconds + Z)
//   - `"2026-03-16T20:34:49.936789+00:00"`   (microseconds + offset)
//   - `"2026-03-04T07:40:49.531658+00:00"`   (microseconds + offset; humans)
//
// The parser tries formats in order; first match wins. Returns nil if
// nothing matches — callers treat nil as a parse failure and trigger
// the P0 carry-forward path.
import Foundation

public enum AnarlogTimestampParser {

    /// Parse an anarlog-emitted ISO-8601 timestamp string. Tolerant of
    /// the variants observed across Anarlog versions; returns nil on
    /// formats none of the strategies handle. Fractional seconds beyond
    /// milliseconds are truncated — Anarlog timestamps are session
    /// `created_at` values that we never compare at sub-millisecond
    /// resolution, and `ISO8601DateFormatter` only handles 3 fractional
    /// digits.
    public static func parse(_ raw: String) -> Date? {
        let normalized = truncateFractionalToMillis(raw)

        // Strategy 1: ISO8601DateFormatter with fractional seconds.
        // Handles `2026-03-16T20:34:49.936Z` and (post-truncation)
        // `2026-03-04T07:40:49.531+00:00`.
        let iso1 = ISO8601DateFormatter()
        iso1.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        if let d = iso1.date(from: normalized) { return d }

        // Strategy 2: ISO8601DateFormatter without fractional seconds.
        // Handles `2026-03-16T20:34:49Z`.
        let iso2 = ISO8601DateFormatter()
        iso2.formatOptions = [.withInternetDateTime]
        if let d = iso2.date(from: normalized) { return d }

        // Strategy 3: manual DateFormatter for no fractional seconds
        // with offset. Pattern: `2026-03-16T20:34:49+00:00`.
        let manualOffset = DateFormatter()
        manualOffset.locale = Locale(identifier: "en_US_POSIX")
        manualOffset.timeZone = TimeZone(secondsFromGMT: 0)
        manualOffset.dateFormat = "yyyy-MM-dd'T'HH:mm:ssXXX"
        if let d = manualOffset.date(from: normalized) { return d }

        return nil
    }

    /// Trim fractional seconds longer than 3 digits (`.531658` → `.531`).
    /// Anarlog emits microseconds for humans / `_meta.json`; we collapse
    /// to milliseconds so a single ISO8601DateFormatter pass handles
    /// every input. No-op when fractional seconds are absent or already
    /// ≤3 digits.
    private static func truncateFractionalToMillis(_ raw: String) -> String {
        guard let dot = raw.firstIndex(of: ".") else { return raw }
        let after = raw.index(after: dot)
        // Scan digits after the dot.
        var end = after
        while end < raw.endIndex, raw[end].isNumber {
            end = raw.index(after: end)
        }
        let fractionDigits = raw.distance(from: after, to: end)
        guard fractionDigits > 3 else { return raw }
        let keep = raw.index(after, offsetBy: 3)
        return String(raw[..<keep]) + String(raw[end...])
    }
}
