package threadctx_test

import (
	"strings"
	"testing"

	"personal-crm/backend/internal/threadctx"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// over is the name span over the first occurrence of name in text.
func over(t *testing.T, text, name string) threadctx.NameSpan {
	t.Helper()
	start := strings.Index(text, name)
	require.GreaterOrEqual(t, start, 0, "%q holds %q", text, name)
	return threadctx.NameSpan{Start: start, End: start + len(name)}
}

// maskSpans masks c by the dictionary with the name spans the model gives
// each text.
func maskSpans(t *testing.T, d threadctx.Dictionary, c threadctx.Context, spans map[string][]threadctx.NameSpan) threadctx.MaskedRequest {
	t.Helper()
	masker, err := threadctx.NewMasker(d, nil)
	require.NoError(t, err)
	masked, err := masker.Mask(t.Context(), threadctx.CoverForTest(c, spans), nil)
	require.NoError(t, err)
	return masked
}

// The masker masks the union of the dictionary's matches and each entry's
// name spans, and every masked range takes its placeholder from the one
// per-request placeholder map.
// spec: KNW-090.union-masked
func TestMask_NameSpans(t *testing.T) {
	t.Run("a span over a capitalized word no known name holds", func(t *testing.T) {
		const text = "I ran into Quenbry at the market"
		masked := maskSpans(t, threadctx.Dictionary{Own: own}, dm(text), map[string][]threadctx.NameSpan{text: {over(t, text, "Quenbry")}})
		assert.Equal(t, "I ran into NAME_1 at the market", masked.Entries()[0].Text)
	})

	t.Run("the same unknown name in two entries", func(t *testing.T) {
		const first, second = "Quenbry called twice", "tell Quenbry and Ostrava thanks"
		masked := maskSpans(t, threadctx.Dictionary{Own: own}, dm(first, second), map[string][]threadctx.NameSpan{
			first:  {over(t, first, "Quenbry")},
			second: {over(t, second, "Quenbry"), over(t, second, "Ostrava")},
		})
		assert.Equal(t, []string{"NAME_1 called twice", "tell NAME_1 and NAME_2 thanks"}, entryTexts(masked.Entries()))
	})

	// A month word never matches a name in the dictionary, so only the span
	// masks it.
	t.Run("a span over a participant's alias", func(t *testing.T) {
		const text = "May agreed to the plan"
		c := dm(text)
		c.Participants[threadctx.RoleThem] = threadctx.Participant{ContactID: &juniper, Aliases: []string{"May Lindqvist"}}
		masked := maskSpans(t, threadctx.Dictionary{Own: own}, c, map[string][]threadctx.NameSpan{text: {over(t, text, "May")}})
		assert.Equal(t, "THEM agreed to the plan", masked.Entries()[0].Text)
	})

	t.Run("a span over the user's own name", func(t *testing.T) {
		const text = "June will bring the cake"
		d := threadctx.Dictionary{Own: []string{"June Ashworth"}}
		masked := maskSpans(t, d, dm(text), map[string][]threadctx.NameSpan{text: {over(t, text, "June")}})
		assert.Equal(t, "ME will bring the cake", masked.Entries()[0].Text)
	})

	t.Run("a span that overlaps a dictionary match only in part", func(t *testing.T) {
		const text = "met Tobias Wren Halvorsen today"
		masked := maskSpans(t, threadctx.Dictionary{Own: own, Contacts: names}, dm(text), map[string][]threadctx.NameSpan{text: {over(t, text, "Wren Halvorsen")}})
		assert.Equal(t, "met NAME_1 today", masked.Entries()[0].Text)
	})

	t.Run("a span that ends inside a word", func(t *testing.T) {
		const text = "I ran into Quenbry at the market"
		masked := maskSpans(t, threadctx.Dictionary{Own: own}, dm(text), map[string][]threadctx.NameSpan{text: {over(t, text, "Quen")}})
		assert.Equal(t, "I ran into NAME_1 at the market", masked.Entries()[0].Text)
	})

	t.Run("a known name the model reports no span for", func(t *testing.T) {
		const text = "Tobias Wren and Quenbry"
		masked := maskSpans(t, threadctx.Dictionary{Own: own, Contacts: names}, dm(text), map[string][]threadctx.NameSpan{text: {over(t, text, "Quenbry")}})
		assert.Equal(t, "NAME_1 and NAME_2", masked.Entries()[0].Text)
	})
}

// A name span follows the rule a dictionary match does: a span whose every
// word is a common English word written in lower case is no name and masks
// nothing; any other span masks every word it touches, whole. A word is
// judged by the common-word list and the 's split the dictionary's matches
// are.
// spec: KNW-090.lowercase-common-spans-kept
func TestMask_NameSpansKeepOrdinaryWords(t *testing.T) {
	for _, tc := range []struct {
		name       string
		dictionary threadctx.Dictionary
		text, span string
		want       string
	}{
		{"one lower-case common word", threadctx.Dictionary{Own: own}, "ask dad about it", "dad", "ask dad about it"},
		{"several words, each lower-case and common", threadctx.Dictionary{Own: own}, "ask my sister about it", "my sister", "ask my sister about it"},
		{"a lower-case word that is no common word", threadctx.Dictionary{Own: own}, "ask quenbry about it", "quenbry", "ask NAME_1 about it"},
		{"a common word capitalized mid-sentence", threadctx.Dictionary{Own: own}, "ask Will about it", "Will", "ask NAME_1 about it"},
		{"a common word with an upper-case letter past its first", threadctx.Dictionary{Own: own}, "ask wILL about it", "wILL", "ask NAME_1 about it"},
		{"a common word capitalized at the start of a sentence", threadctx.Dictionary{Own: own}, "Will asked about it", "Will", "NAME_1 asked about it"},
		{"a lower-case common word with a capitalized word", threadctx.Dictionary{Own: own}, "ask aunt Rose about it", "aunt Rose", "ask NAME_1 about it"},
		{"a lower-case common word a contact holds as a name", contactsNamed("Will Brannock"), "talked to will about it", "will", "talked to will about it"},
		{"a lower-case 's word whose head is a common word", threadctx.Dictionary{Own: own}, "took dad's car", "dad's", "took dad's car"},
		{"a lower-case 's word whose head is no common word", threadctx.Dictionary{Own: own}, "took quenbry's car", "quenbry's", "took NAME_1 car"},
		{"a span inside a lower-case common word", threadctx.Dictionary{Own: own}, "she is willing to help", "will", "she is willing to help"},
		{"a span inside a lower-case word that is no common word", threadctx.Dictionary{Own: own}, "ask quenbryson about it", "quenbry", "ask NAME_1 about it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			masked := maskSpans(t, tc.dictionary, dm(tc.text), map[string][]threadctx.NameSpan{tc.text: {over(t, tc.text, tc.span)}})
			assert.Equal(t, tc.want, masked.Entries()[0].Text)
		})
	}
}
