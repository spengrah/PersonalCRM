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
    /// formats none of the strategies handle.
    public static func parse(_ raw: String) -> Date? {
        // Strategy 1: ISO8601DateFormatter with fractional seconds + Z.
        // Handles `2026-03-16T20:34:49.936Z`.
        let iso1 = ISO8601DateFormatter()
        iso1.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        if let d = iso1.date(from: raw) { return d }

        // Strategy 2: ISO8601DateFormatter without fractional seconds.
        // Handles `2026-03-16T20:34:49Z`.
        let iso2 = ISO8601DateFormatter()
        iso2.formatOptions = [.withInternetDateTime]
        if let d = iso2.date(from: raw) { return d }

        // Strategy 3: ISO8601DateFormatter with fractional seconds +
        // explicit offset (`+00:00`). The .withFractionalSeconds option
        // is sticky enough that the same formatter handles both Z and
        // +HH:MM suffixes, but we use the same combination as Strategy
        // 1 with .withTimeZone instead of the default `Z`-style.
        // ISO8601DateFormatter coerces `+00:00` → UTC under
        // .withInternetDateTime; covered already by Strategy 1.

        // Strategy 4: manual DateFormatter for microsecond precision
        // with `+HH:MM` offset. ISO8601DateFormatter caps fractional
        // seconds at milliseconds in some platform builds; the manual
        // `SSSSSS` format pattern handles up to 6 digits.
        //
        // Pattern: `2026-03-04T07:40:49.531658+00:00`.
        let manualMicroOffset = DateFormatter()
        manualMicroOffset.locale = Locale(identifier: "en_US_POSIX")
        manualMicroOffset.timeZone = TimeZone(secondsFromGMT: 0)
        manualMicroOffset.dateFormat = "yyyy-MM-dd'T'HH:mm:ss.SSSSSSXXX"
        if let d = manualMicroOffset.date(from: raw) { return d }

        // Strategy 5: manual DateFormatter for microsecond precision
        // with a `Z` suffix. Pattern: `2026-03-04T07:40:49.531658Z`.
        let manualMicroZ = DateFormatter()
        manualMicroZ.locale = Locale(identifier: "en_US_POSIX")
        manualMicroZ.timeZone = TimeZone(secondsFromGMT: 0)
        manualMicroZ.dateFormat = "yyyy-MM-dd'T'HH:mm:ss.SSSSSS'Z'"
        if let d = manualMicroZ.date(from: raw) { return d }

        // Strategy 6: manual DateFormatter for no fractional seconds
        // with offset. Pattern: `2026-03-16T20:34:49+00:00`.
        let manualOffset = DateFormatter()
        manualOffset.locale = Locale(identifier: "en_US_POSIX")
        manualOffset.timeZone = TimeZone(secondsFromGMT: 0)
        manualOffset.dateFormat = "yyyy-MM-dd'T'HH:mm:ssXXX"
        if let d = manualOffset.date(from: raw) { return d }

        return nil
    }
}
