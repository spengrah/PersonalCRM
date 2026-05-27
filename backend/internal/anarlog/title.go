// Package anarlog holds anarlog-specific domain heuristics that don't
// belong in the generic matching/ normalization package. Surfaces:
//
//   - ExtractNameTokens: a pure-Go heuristic that turns a session
//     title (free-form string) into a list of normalized name tokens.
//   - TitleMatcher: a service-layer wrapper around
//     repository.ContactRepository.FindSimilarContacts that applies a
//     trigram collision-gap rule for single-token disambiguation.
//   - DiscoveryWriter: a service-layer wrapper around
//     ExternalContactRepository.UpsertTx that writes weak
//     anarlog_title discovery rows.
package anarlog

import (
	"regexp"
	"strings"
)

// keepTokenRegex enforces the spec's "starts with uppercase, alphabetic,
// length 2..30" invariant on extracted tokens. ASCII-only by design;
// diacritic / non-Latin names are silently dropped (spec philosophy:
// "heuristics, not NLP"). The tagging path (anarlog_human_id) catches
// names that fall through.
var keepTokenRegex = regexp.MustCompile(`^[A-Z][a-zA-Z]{1,29}$`)

// metaSingleWordTokens are word-boundary-stripped from titles. Order
// doesn't matter; each is compiled to a `\b<word>\b` regex with case-
// insensitive flag.
var metaSingleWordTokens = []string{
	"sync", "catchup", "chat", "call", "meeting",
	"intro", "standup", "huddle", "check-in", "checkin",
	"review",
	"1:1", "1-1", "one-on-one",
}

// metaWeekdayTokens covers full + abbreviated weekday names. Word-
// boundary matched so "Monica" doesn't match "mon".
var metaWeekdayTokens = []string{
	"monday", "tuesday", "wednesday", "thursday",
	"friday", "saturday", "sunday",
	"mon", "tue", "tues", "wed", "thu", "thur", "thurs", "fri", "sat", "sun",
}

// metaMonthTokens covers full + abbreviated month names. People literally
// named May, June, April, March, August are dropped by this stripping —
// accepted tradeoff per spec philosophy. The tagging path catches them.
var metaMonthTokens = []string{
	"january", "february", "march", "april", "may", "june",
	"july", "august", "september", "october", "november", "december",
	"jan", "feb", "mar", "apr", "jun", "jul", "aug", "sep", "sept",
	"oct", "nov", "dec",
}

// metaTokensRegex is the combined `\b(...)\b` regex that strips all
// single-word meta-tokens + weekday + month names case-insensitively in
// a single pass. Compiled at package load.
var metaTokensRegex = func() *regexp.Regexp {
	all := make([]string, 0,
		len(metaSingleWordTokens)+len(metaWeekdayTokens)+len(metaMonthTokens))
	all = append(all, metaSingleWordTokens...)
	all = append(all, metaWeekdayTokens...)
	all = append(all, metaMonthTokens...)
	escaped := make([]string, len(all))
	for i, t := range all {
		escaped[i] = regexp.QuoteMeta(t)
	}
	// (?i) case-insensitive, \b word boundary on both sides.
	return regexp.MustCompile(`(?i)\b(` + strings.Join(escaped, "|") + `)\b`)
}()

// dateRegexes strip ISO dates, US numeric dates, and numeric month-day
// patterns. Applied to the lowercased working copy so substring matches
// in the original-case string at the same byte offsets are blanked.
var dateRegexes = []*regexp.Regexp{
	regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}\b`),            // ISO 2026-05-27
	regexp.MustCompile(`\b\d{1,2}/\d{1,2}(?:/\d{2,4})?\b`), // US 5/12 or 5/12/2026
	regexp.MustCompile(`\b\d{1,2}-\d{1,2}\b`),              // numeric month-day 5-12
}

// multiCharSeparators are normalized to a single space BEFORE the final
// whitespace split. Each entry is matched case-insensitively.
var multiCharSeparators = []string{
	" and ", " with ", " w/ ",
}

// singleCharSeparators are replaced with a space at the byte level
// before strings.Fields splits on whitespace. `:` and `-` are intentional
// even though they appear in some date / meta-token patterns — by the
// time we run this step, those have already been stripped.
var singleCharSeparators = []string{
	"/", "&", "+", ":", "-",
}

// stopwords is an intentionally tiny case-insensitive drop list applied
// AFTER the keepTokenRegex filter. Spec philosophy: "expand iteratively
// from observed false positives in dogfooding." Items here would
// otherwise pass the regex but are obviously not names.
var stopwords = map[string]struct{}{
	"re":   {},
	"fwd":  {},
	"test": {},
	"demo": {},
}

// ExtractNameTokens turns a session title into a list of normalized
// name tokens per the spec's heuristic. Returns an empty slice for
// empty or whitespace-only inputs. Deterministic and side-effect-free.
//
// Token casing is preserved from the original title (so "Alice" stays
// "Alice" for display purposes); callers needing the normalized form do
// strings.ToLower themselves. Dedup is case-insensitive (first-seen
// casing wins).
func ExtractNameTokens(title string) []string {
	if strings.TrimSpace(title) == "" {
		return []string{}
	}

	// Lowercase working copy is used for meta-token + date stripping.
	// We replace matches with spaces on the ORIGINAL-case string at the
	// same byte offsets to preserve casing for the keep-regex.
	lower := strings.ToLower(title)
	working := []byte(title)

	blankOut := func(start, end int) {
		for i := start; i < end && i < len(working); i++ {
			working[i] = ' '
		}
	}

	// 1) Strip meta-tokens (single-word, weekday, month).
	for _, m := range metaTokensRegex.FindAllStringIndex(lower, -1) {
		blankOut(m[0], m[1])
	}

	// 2) Strip date patterns.
	for _, re := range dateRegexes {
		for _, m := range re.FindAllStringIndex(lower, -1) {
			blankOut(m[0], m[1])
		}
	}

	stripped := string(working)

	// 3) Replace multi-char separators (case-insensitive) with whitespace.
	for _, sep := range multiCharSeparators {
		stripped = replaceFoldAll(stripped, sep, " ")
	}

	// 4) Replace single-char separators with whitespace.
	for _, sep := range singleCharSeparators {
		stripped = strings.ReplaceAll(stripped, sep, " ")
	}

	// 5) Split on whitespace.
	rawTokens := strings.Fields(stripped)

	// 6) Apply keep regex + stopword filter + dedup (case-insensitive).
	out := make([]string, 0, len(rawTokens))
	seen := make(map[string]struct{}, len(rawTokens))
	for _, tok := range rawTokens {
		if !keepTokenRegex.MatchString(tok) {
			continue
		}
		lowered := strings.ToLower(tok)
		if _, isStop := stopwords[lowered]; isStop {
			continue
		}
		if _, dup := seen[lowered]; dup {
			continue
		}
		seen[lowered] = struct{}{}
		out = append(out, tok)
	}
	return out
}

// replaceFoldAll replaces every case-insensitive occurrence of `old`
// in `s` with `new`. strings.ReplaceAll is case-sensitive; we need
// case-fold for the multi-char separator pass.
func replaceFoldAll(s, old, replacement string) string {
	if old == "" {
		return s
	}
	lower := strings.ToLower(s)
	target := strings.ToLower(old)
	var b strings.Builder
	b.Grow(len(s))
	i := 0
	for i < len(s) {
		idx := strings.Index(lower[i:], target)
		if idx < 0 {
			b.WriteString(s[i:])
			break
		}
		b.WriteString(s[i : i+idx])
		b.WriteString(replacement)
		i += idx + len(old)
	}
	return b.String()
}
