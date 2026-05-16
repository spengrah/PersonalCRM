// HandleNormalization — thin re-export of CRMMacCore.NormalizationParity
// scoped to the chat.db handle.id column shapes.
//
// chat.db's handle.id can be:
//   - an email address ("user@example.com")
//   - a phone number ("+15551234567", "5551234567", "1-555-1234")
//   - an iMessage-only opaque (rare; mostly historical)
//
// We don't try to detect type heuristically — the Pi-side identity
// service stores both forms canonicalized, and KnownIdentifiersCache
// canonicalizes both before comparison. The caller passes the raw
// handle.id; this helper returns the canonical form to look up.
import Foundation
import CRMMacCore

public enum HandleNormalization {
    /// Canonicalize a handle.id for sender-filter cache lookup.
    /// Detects email vs. phone by presence of `@`; phone is normalized
    /// to E.164 (with NANP assumption for 10-digit inputs).
    public static func canonicalize(_ raw: String) -> String {
        let trimmed = raw.trimmingCharacters(in: .whitespacesAndNewlines)
        if trimmed.isEmpty { return "" }

        if trimmed.contains("@") {
            return NormalizationParity.normalizeEmail(trimmed)
        }
        return NormalizationParity.normalizePhoneE164(trimmed)
    }
}
