package matching

import (
	"strings"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// MinHandleLenForNameMatch is the minimum length of a normalized username
// handle before it is usable as a name-search term. Shorter handles produce
// too many trigram matches (e.g., "bob", "jim", "alex").
const MinHandleLenForNameMatch = 4

// NormalizeHandleForNameMatch converts a Telegram (or similar) username into
// a search term suitable for pg_trgm comparison against contact.full_name.
// Returns the normalized term and true if it is >= MinHandleLenForNameMatch.
// Returns "" and false if the result is too short, empty, or all punctuation.
//
// Pipeline (order matters):
//  1. Trim whitespace
//  2. Strip leading '@'
//  3. Unicode NFD + drop combining marks (diacritic fold)
//  4. Lowercase
//  5. Strip trailing digits
//  6. Replace '.', '_', '-' with ' '
//  7. Collapse whitespace + trim
func NormalizeHandleForNameMatch(raw string) (string, bool) {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "@")
	if s == "" {
		return "", false
	}

	t := transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
	if folded, _, err := transform.String(t, s); err == nil {
		s = folded
	}

	s = strings.ToLower(s)
	s = strings.TrimRightFunc(s, func(r rune) bool { return r >= '0' && r <= '9' })

	replacer := strings.NewReplacer(".", " ", "_", " ", "-", " ")
	s = replacer.Replace(s)
	s = strings.Join(strings.Fields(s), " ")

	if len([]rune(s)) < MinHandleLenForNameMatch {
		return "", false
	}
	return s, true
}

// NormalizeForExactHandleMatch applies a strict-equality normalization used to
// detect when a Telegram handle represents the same identity as a CRM
// contact's full_name. Applies the same pipeline to both sides:
//   - trim whitespace
//   - NFD + strip combining marks (diacritic fold)
//   - lowercase
//   - remove every character that is not [a-z0-9]
//
// Returns the collapsed alphanumeric form. Callers compare with ==.
// Example: NormalizeForExactHandleMatch("José Smith") → "josesmith";
// NormalizeForExactHandleMatch("@jose_smith") → "josesmith".
func NormalizeForExactHandleMatch(s string) string {
	s = strings.TrimSpace(s)
	t := transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
	if folded, _, err := transform.String(t, s); err == nil {
		s = folded
	}
	s = strings.ToLower(s)
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}
