// AnarlogHumanFrontmatterParser is a tolerant pure-Swift parser for
// the subset of YAML that Anarlog's `humans/<uuid>.md` frontmatter
// uses. NOT a general YAML parser — we intentionally stay narrow so
// the daemon doesn't drag in a YAML dependency for what is in
// practice a flat key:value file.
//
// Tolerated shapes:
//   - First line must be `---`.
//   - Lines `^([A-Za-z_][A-Za-z0-9_]*):(.*)$` are key:value pairs.
//   - Values are stripped of surrounding whitespace + optional
//     wrapping single/double quotes.
//   - Arrays: `[]` → empty list; `[a, b, c]` → comma-split + per-entry
//     quote strip. Anarlog v1.0.1 changed the serialization (per spec
//     line 187); we accept either flow-style `[a, b]` or empty `[]`.
//   - Booleans: `true`/`false` (case-insensitive).
//   - Integers: bare decimal digits.
//   - Timestamps: parsed via AnarlogTimestampParser; on parse failure,
//     stored as the raw string in rawExtras (the caller treats the
//     missing-timestamp field as nil but doesn't fail the whole file).
//
// Failure mode: missing closing `---` → return nil. Lenient on
// individual key parses (unknown keys go into rawExtras).
import Foundation

public enum AnarlogHumanFrontmatterParser {

    /// Parse a `humans/<uuid>.md` file body into an
    /// AnarlogHumanRecord. The caller supplies the UUID derived from
    /// the filename (the frontmatter has no `id` field for humans).
    /// Returns nil when the YAML frontmatter is malformed enough that
    /// no structured data can be recovered.
    public static func parse(
        uuid: String,
        fileBytes: Data
    ) -> AnarlogHumanRecord? {
        guard let text = String(data: fileBytes, encoding: .utf8) else {
            return nil
        }
        var lines = text.split(separator: "\n", omittingEmptySubsequences: false)
            .map { String($0) }
        // Allow either Unix or Windows line endings — Foundation's
        // split-on-`\n` leaves trailing `\r` which we strip.
        lines = lines.map { line in
            line.hasSuffix("\r") ? String(line.dropLast()) : line
        }
        // The frontmatter must open with `---` on the first non-empty
        // line. We're strict here: a missing opener means the file
        // isn't an Anarlog human note and we return nil so the caller
        // skips it.
        var idx = 0
        while idx < lines.count && lines[idx].trimmingCharacters(in: .whitespaces).isEmpty {
            idx += 1
        }
        guard idx < lines.count, lines[idx].trimmingCharacters(in: .whitespaces) == "---" else {
            return nil
        }
        idx += 1

        var name = ""
        var emails: [String] = []
        var jobTitle = ""
        var linkedinUsername = ""
        var orgID = ""
        var userID = ""
        var pinOrder = 0
        var pinned = false
        var createdAt: Date?
        var rawExtras: [String: String] = [:]

        var closerIdx: Int?
        while idx < lines.count {
            let line = lines[idx]
            if line.trimmingCharacters(in: .whitespaces) == "---" {
                closerIdx = idx
                break
            }
            // Empty lines inside the frontmatter are ignored.
            let trimmed = line.trimmingCharacters(in: .whitespaces)
            if trimmed.isEmpty {
                idx += 1
                continue
            }
            // Key:value parse. Allow any chars in the value (quotes,
            // colons, brackets); the value-shape decoders take over
            // after the split.
            guard let colonIdx = line.firstIndex(of: ":") else {
                idx += 1
                continue
            }
            let rawKey = String(line[..<colonIdx]).trimmingCharacters(in: .whitespaces)
            let rawValue = String(line[line.index(after: colonIdx)...])
                .trimmingCharacters(in: .whitespaces)
            // Validate key shape — must match
            // `[A-Za-z_][A-Za-z0-9_]*`. Defensive: a stray colon in
            // freeform text (rare in frontmatter but possible) is
            // skipped rather than treated as a key.
            guard isValidKey(rawKey) else {
                idx += 1
                continue
            }

            switch rawKey {
            case "name":
                name = decodeScalar(rawValue)
            case "emails":
                emails = decodeArray(rawValue)
            case "job_title":
                jobTitle = decodeScalar(rawValue)
            case "linkedin_username":
                linkedinUsername = decodeScalar(rawValue)
            case "org_id":
                orgID = decodeScalar(rawValue)
            case "user_id":
                userID = decodeScalar(rawValue)
            case "pin_order":
                pinOrder = Int(decodeScalar(rawValue)) ?? 0
            case "pinned":
                pinned = decodeBool(rawValue)
            case "created_at":
                let scalar = decodeScalar(rawValue)
                createdAt = AnarlogTimestampParser.parse(scalar)
                if createdAt == nil && !scalar.isEmpty {
                    // Preserve the raw string for future debugging —
                    // the caller emits the record without a parsed
                    // timestamp.
                    rawExtras["created_at"] = scalar
                }
            default:
                rawExtras[rawKey] = decodeScalar(rawValue)
            }
            idx += 1
        }
        guard let closer = closerIdx else {
            // Missing closing `---` → malformed. Return nil so the
            // P0 carry-forward path activates.
            return nil
        }

        // Body after closing `---` becomes memo. Trim leading/trailing
        // whitespace + nil out when empty.
        let bodyLines = lines.dropFirst(closer + 1)
        let body = bodyLines.joined(separator: "\n")
            .trimmingCharacters(in: .whitespacesAndNewlines)
        let memo: String? = body.isEmpty ? nil : body

        return AnarlogHumanRecord(
            uuid: uuid,
            name: name,
            emails: emails,
            jobTitle: jobTitle,
            linkedinUsername: linkedinUsername,
            orgID: orgID,
            userID: userID,
            pinOrder: pinOrder,
            pinned: pinned,
            createdAt: createdAt,
            memo: memo,
            rawExtras: rawExtras)
    }

    // MARK: - value decoders

    private static func isValidKey(_ s: String) -> Bool {
        guard let first = s.first else { return false }
        guard first.isLetter || first == "_" else { return false }
        for c in s.dropFirst() {
            guard c.isLetter || c.isNumber || c == "_" else { return false }
        }
        return true
    }

    /// Strip surrounding whitespace + optional wrapping single/double
    /// quotes. `''` becomes empty; `'foo'` becomes `foo`.
    static func decodeScalar(_ raw: String) -> String {
        var s = raw.trimmingCharacters(in: .whitespaces)
        if s.count >= 2 {
            let first = s.first!
            let last = s.last!
            if (first == "\"" && last == "\"") || (first == "'" && last == "'") {
                s = String(s.dropFirst().dropLast())
            }
        }
        return s
    }

    /// Decode a YAML flow-style array. `[]` → []; `[a, b]` → ["a", "b"].
    /// Anything that doesn't match the flow-style shape returns [].
    static func decodeArray(_ raw: String) -> [String] {
        let s = raw.trimmingCharacters(in: .whitespaces)
        guard s.hasPrefix("["), s.hasSuffix("]") else { return [] }
        let inner = String(s.dropFirst().dropLast()).trimmingCharacters(in: .whitespaces)
        if inner.isEmpty { return [] }
        return inner
            .split(separator: ",")
            .map { decodeScalar(String($0)) }
            .filter { !$0.isEmpty }
    }

    static func decodeBool(_ raw: String) -> Bool {
        let s = raw.trimmingCharacters(in: .whitespaces).lowercased()
        return s == "true"
    }
}
