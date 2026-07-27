package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Slug generation for the #760 migration, per the arc's §3.8 as amended by the
// PR2 plan's D2. The output is a PERMANENT citation key: once minted it is
// never regenerated or renamed by tooling, which is why the migration runs in
// two passes with a human review checkpoint between them.

// slugStopwords are the tokens dropped from a then-item text before the key is
// minted.
//
// Negation-bearing tokens — no, not, never, without, unless, nothing, none —
// are DELIBERATELY absent from this set. Dropping one would let a permanent key
// read as the exact negation of the item it names ("the banner is not shown
// when nothing is stale" → banner-shown). 36 of the corpus's minted keys retain
// such a token; 24 of them retain `no` or `not` specifically.
var slugStopwords = map[string]bool{
	"a": true, "an": true, "and": true, "are": true, "as": true, "at": true,
	"be": true, "by": true, "for": true, "from": true, "in": true, "is": true,
	"it": true, "its": true, "of": true, "on": true, "or": true, "that": true,
	"the": true, "this": true, "to": true, "with": true,
}

const (
	// slugTakeTokens is how many surviving tokens the base slug takes.
	slugTakeTokens = 4
	// slugMaxLen caps the final key, including any disambiguating suffix.
	slugMaxLen = 40
	// slugReserved is the token a waiver uses to address a statement
	// behavior's implicit item. It matches the key charset, so the generator
	// must never mint it as a then-item key.
	slugReserved = "statement"
)

// slugCharset is the key charset the corpus lints against
// (spec/validate.go's thenKeyRegex). Every minted slug is asserted against it.
var slugCharset = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// slugTokens lowercases the text, collapses every run of non-[a-z0-9] into a
// separator, splits, and drops stopwords. If the stopword filter empties the
// list the unfiltered list is kept, so a text made entirely of stopwords still
// yields a key. A text with no alphanumeric content at all yields nil.
func slugTokens(text string) []string {
	var b strings.Builder
	for _, r := range strings.ToLower(text) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	raw := strings.FieldsFunc(b.String(), func(r rune) bool { return r == '-' })
	if len(raw) == 0 {
		return nil
	}
	kept := make([]string, 0, len(raw))
	for _, t := range raw {
		if !slugStopwords[t] {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		return raw
	}
	return kept
}

// truncateSlug hard-truncates to max and trims any trailing separator the cut
// left behind. A truncation landing mid-token would otherwise leave "...-",
// which produces "...--2" once a numeric suffix is appended and fails the
// charset assertion.
func truncateSlug(s string, max int) string {
	if max < 0 {
		max = 0
	}
	if len(s) > max {
		s = s[:max]
	}
	return strings.TrimRight(s, "-")
}

// capTokens joins the tokens and caps the result at max by dropping trailing
// tokens until it fits, hard-truncating only when a single over-long token
// remains.
func capTokens(toks []string, max int) string {
	for len(toks) > 1 && len(strings.Join(toks, "-")) > max {
		toks = toks[:len(toks)-1]
	}
	return truncateSlug(strings.Join(toks, "-"), max)
}

// slugBase is the candidate mintSlug starts from, before any collision or
// reserved-token disambiguation. Comparing a minted key against it is how the
// review artifact flags the keys that needed disambiguating.
func slugBase(text string) string {
	toks := slugTokens(text)
	if len(toks) == 0 {
		return ""
	}
	n := slugTakeTokens
	if n > len(toks) {
		n = len(toks)
	}
	return capTokens(toks[:n], slugMaxLen)
}

// apostropheFragments returns the alphanumeric runs that immediately follow an
// apostrophe in text, lowercased. The separator rule turns each into a
// standalone token, which is the corpus's single largest weak-slug class:
// "a method's value" yields updating-method-s-value.
func apostropheFragments(text string) []string {
	var out []string
	lower := strings.ToLower(text)
	for i := 0; i < len(lower); i++ {
		if lower[i] != '\'' {
			continue
		}
		j := i + 1
		for j < len(lower) && (lower[j] >= 'a' && lower[j] <= 'z' || lower[j] >= '0' && lower[j] <= '9') {
			j++
		}
		if j > i+1 {
			out = append(out, lower[i+1:j])
		}
	}
	return out
}

// mintSlug derives a key for text that does not collide with taken (the keys
// already minted for the SAME behavior, in item order — uniqueness is
// per-behavior) and is never the reserved "statement" token.
//
// Collision resolution appends the next unused token from the post-stopword
// token list one at a time, re-applying the length cap after every append. If
// that list is exhausted the fallback is a -2/-3/... suffix, with the base
// truncated from the token side so the disambiguator always survives.
func mintSlug(text string, taken map[string]bool) (string, error) {
	toks := slugTokens(text)
	if len(toks) == 0 {
		return "", fmt.Errorf("then-item text %q has no alphanumeric content, so it cannot yield a key", text)
	}
	n := slugTakeTokens
	if n > len(toks) {
		n = len(toks)
	}
	cand := capTokens(toks[:n], slugMaxLen)
	for i := slugTakeTokens; (taken[cand] || cand == slugReserved) && i < len(toks); i++ {
		cand = capTokens(toks[:i+1], slugMaxLen)
	}
	if taken[cand] || cand == slugReserved {
		base := capTokens(toks[:n], slugMaxLen)
		found := false
		for k := 2; k < 1000; k++ {
			suffix := strconv.Itoa(k)
			c := truncateSlug(base, slugMaxLen-len(suffix)-1) + "-" + suffix
			if !taken[c] && c != slugReserved {
				cand, found = c, true
				break
			}
		}
		if !found {
			return "", fmt.Errorf("exhausted numeric disambiguators for %q", text)
		}
	}
	if !slugCharset.MatchString(cand) {
		return "", fmt.Errorf("minted slug %q for %q violates the key charset", cand, text)
	}
	return cand, nil
}
