package threadctx_test

import (
	"strings"
	"testing"

	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/threadctx"

	"github.com/stretchr/testify/assert"
)

// A personal pronoun, and a contraction headed by one, is never a name: the
// dictionary leaves it whole even where a stored name holds the word or the
// contraction, and a name span over it masks nothing.
// spec: KNW-089.pronouns-never-names, KNW-090.pronoun-spans-kept
func TestMask_PronounsAreNeverNames(t *testing.T) {
	apostrophes := func(text string) []string { return []string{text, strings.ReplaceAll(text, "'", "’")} }

	// Display names that are phrases, as some chat users set them.
	phrases := threadctx.Dictionary{Own: own, Display: []repository.DisplayName{
		{Owner: "telegram:1001", Name: "I'll Be There"},
		{Owner: "telegram:1002", Name: "It Is What It Is"},
	}}
	for _, text := range []string{"i'll grab time", "I'll grab a new one for you in a bit", "It's not the most organized thing"} {
		for _, text := range apostrophes(text) {
			t.Run("a stored name holding the pronoun or its contraction: "+text, func(t *testing.T) {
				assert.Equal(t, text, maskWith(t, phrases, dm(text)).Entries()[0].Text)
			})
		}
	}

	// No name match crosses a pronoun: the stored phrase's other words are
	// capitalized known-name words, each masked alone.
	t.Run("a message that is a whole stored phrase holding a pronoun", func(t *testing.T) {
		assert.Equal(t, "It NAME_1 NAME_1 It NAME_1", maskWith(t, phrases, dm("It Is What It Is")).Entries()[0].Text)
	})

	t.Run("a capitalized It's with a contact named It", func(t *testing.T) {
		for _, text := range apostrophes("It's good to see you") {
			assert.Equal(t, text, maskWith(t, contactsNamed("It Sorensen"), dm(text)).Entries()[0].Text)
		}
	})

	for _, tc := range []struct{ text, span string }{
		{"I'll grab a new one for you in a bit", "I"},
		{"I'd love a rundown", "I"},
		{"I'm happy to schedule manually", "I"},
		{"You're welcome! See you soon", "You"},
		{"We'll call later", "We"},
	} {
		for _, text := range apostrophes(tc.text) {
			t.Run("a name span over "+tc.span+": "+text, func(t *testing.T) {
				masked := maskSpans(t, threadctx.Dictionary{Own: own}, dm(text), map[string][]threadctx.NameSpan{text: {over(t, text, tc.span)}})
				assert.Equal(t, text, masked.Entries()[0].Text)
			})
		}
	}

	t.Run("a name span over I beside a span over an unknown name", func(t *testing.T) {
		const text = "I'll call Quenbry tomorrow"
		masked := maskSpans(t, threadctx.Dictionary{Own: own}, dm(text), map[string][]threadctx.NameSpan{text: {over(t, text, "I"), over(t, text, "Quenbry")}})
		assert.Equal(t, "I'll call NAME_1 tomorrow", masked.Entries()[0].Text)
	})
}
