// Swift port of backend/internal/matching/normalize.go.
//
// The Pi-side identity match uses the Go normalizers. The Mac daemon's
// sender filter (CRMMacMessagesSource.KnownIdentifiersCache) needs to
// produce the SAME normalized strings or the filter silently drops real
// inbound messages. Drift between the two sides is caught at PR-review
// time via a shared JSON fixture exercised by both
// backend/internal/matching/parity_test.go and
// CRMMacCoreTests/NormalizationParityTests.swift.
//
// Both normalizers MUST match the Go implementation byte-for-byte for
// every fixture entry; changes here require corresponding changes (or
// fixture updates) on the Go side.
import Foundation

public enum NormalizationParity {
    /// Normalize an email address: lowercase + trim whitespace.
    /// Mirrors `matching.NormalizeEmail` in Go.
    public static func normalizeEmail(_ email: String) -> String {
        let trimmed = email.trimmingCharacters(in: .whitespacesAndNewlines)
        return trimmed.lowercased()
    }

    /// Normalize a phone number to E.164.
    /// Mirrors `matching.NormalizePhoneE164` in Go.
    ///
    /// Behavior:
    ///   - empty / whitespace-only -> ""
    ///   - all non-digit -> ""
    ///   - 10 digits, no leading + -> "+1<digits>" (NANP assumption)
    ///   - 11 digits starting with 1 -> "+1<digits>"
    ///   - any other digit sequence -> "+<digits>"
    public static func normalizePhoneE164(_ phone: String) -> String {
        let trimmed = phone.trimmingCharacters(in: .whitespacesAndNewlines)
        if trimmed.isEmpty {
            return ""
        }

        let hasPlus = trimmed.hasPrefix("+")
        let digitChars = trimmed.unicodeScalars.filter { CharacterSet.decimalDigits.contains($0) }
        let digits = String(String.UnicodeScalarView(digitChars))
        if digits.isEmpty {
            return ""
        }

        if digits.count == 10 && !hasPlus {
            return "+1" + digits
        }

        if digits.count == 11 && digits.first == "1" {
            return "+" + digits
        }

        if hasPlus {
            return "+" + digits
        }

        return "+" + digits
    }
}
