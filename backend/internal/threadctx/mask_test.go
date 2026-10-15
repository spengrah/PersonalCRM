package threadctx_test

import (
	"strings"
	"testing"
	"time"

	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/threadctx"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Synthetic people only.
var (
	juniper = uuid.MustParse("00000000-0000-4000-8000-000000000001")
	tobias  = uuid.MustParse("00000000-0000-4000-8000-000000000002")
	names   = []repository.ContactName{
		{ContactID: juniper, Name: "Juniper Okafor"},
		{ContactID: juniper, Name: "Juni"},
		{ContactID: tobias, Name: "Tobias Wren"},
	}
	cutOff = time.Date(2026, 1, 14, 0, 0, 0, 0, time.UTC)
)

// dm is a one-to-one thread with Juniper, whose messages are the given texts.
func dm(texts ...string) threadctx.Context {
	entries := make([]threadctx.Entry, len(texts))
	for i, text := range texts {
		entries[i] = threadctx.Entry{Role: threadctx.RoleThem, SentAt: cutOff.Add(time.Duration(i-len(texts)) * time.Hour), Text: text}
	}
	return threadctx.Context{
		Kind:    threadctx.KindConversation,
		CutOff:  cutOff,
		Entries: entries,
		Participants: map[threadctx.Role]threadctx.Participant{
			threadctx.RoleMe:   {IsUser: true},
			threadctx.RoleThem: {ContactID: &juniper},
		},
	}
}

func question(instructions string, labels ...string) threadctx.Question {
	q := threadctx.Question{ID: "ball_in_court:THEM", Kind: threadctx.QuestionChoice, Instructions: instructions}
	for i, label := range labels {
		q.Options = append(q.Options, threadctx.Option{Key: "option_" + string(rune('a'+i)), Label: label})
	}
	return q
}

func mask(t *testing.T, c threadctx.Context, questions ...threadctx.Question) threadctx.MaskedRequest {
	t.Helper()
	masked, err := threadctx.NewMasker(names).Mask(t.Context(), c, questions)
	require.NoError(t, err)
	return masked
}

// hostedText is everything a hosted request carries, as one text.
func hostedText(r threadctx.MaskedRequest) string {
	var parts []string
	for _, entry := range r.Entries() {
		parts = append(parts, string(entry.Role), entry.Text)
		for name, value := range entry.Headers {
			parts = append(parts, name, value)
		}
	}
	for _, q := range r.Questions() {
		parts = append(parts, q.ID, q.Instructions)
		for _, option := range q.Options {
			parts = append(parts, option.Key, option.Label, option.Description)
		}
	}
	return strings.Join(parts, "\n")
}

func entryTexts(entries []threadctx.Entry) []string {
	out := make([]string, len(entries))
	for i, entry := range entries {
		out[i] = entry.Text
	}
	return out
}

func assertGone(t *testing.T, r threadctx.MaskedRequest, identifiers ...string) {
	t.Helper()
	text := strings.ToLower(hostedText(r))
	for _, identifier := range identifiers {
		assert.NotContains(t, text, strings.ToLower(identifier))
	}
}

// No synthetic contact's name, nickname, phone number or email address
// survives anywhere in a masked request.
func TestMask_NoContactIdentifierSurvives(t *testing.T) {
	t.Run("a name in a message body", func(t *testing.T) {
		r := mask(t, dm("Tobias Wren says hi, and so does Okafor"))
		assertGone(t, r, "Tobias", "Wren", "Okafor")
	})

	t.Run("a nickname", func(t *testing.T) {
		r := mask(t, dm("it's juni here, new number"))
		assertGone(t, r, "juni")
	})

	t.Run("a phone number in three formats", func(t *testing.T) {
		r := mask(t, dm("call +1 555 010 4477", "or (555) 010-4477", "or 555.010.4477"))
		assertGone(t, r, "555", "4477")
	})

	t.Run("an email address in a sender header", func(t *testing.T) {
		c := dm("see attached")
		c.Entries[0].Headers = map[string]string{"from": "Juniper Okafor <jo.k.77@example.test>", "to": "tobias.wren@example.test"}
		r := mask(t, c)
		assertGone(t, r, "juniper", "okafor", "jo.k.77", "tobias", "wren", "@example.test")
	})

	t.Run("a name inside an option label", func(t *testing.T) {
		r := mask(t, dm("ok"), question("Does THEM owe ME a reply? Ask Tobias if unsure.", "Yes, Juniper owes a reply", "No"))
		assertGone(t, r, "juniper", "tobias")
	})

	t.Run("a full name split by an email address", func(t *testing.T) {
		r := mask(t, dm("Juniper <jo.k.77@example.test> Okafor"))
		assertGone(t, r, "juniper", "jo.k.77", "okafor")
	})

	t.Run("a phone number shaped like a date", func(t *testing.T) {
		assertGone(t, mask(t, dm("call 555-01-04 tonight")), "555", "01-04")
	})

	t.Run("an email address containing a date", func(t *testing.T) {
		assertGone(t, mask(t, dm("write to peer@2026-10-17.example.test")), "peer@", "2026-10-17.example")
	})

	t.Run("an identifier in metadata", func(t *testing.T) {
		c := dm("dinner?")
		c.Entries[0].Headers = map[string]string{"subject": "Juni + Tobias: call 555 010 4477 or write to supper-club@example.test"}
		r := mask(t, c)
		assertGone(t, r, "juni", "tobias", "4477", "supper-club")
	})
}

// Placeholders are consistent within one thread context, and weekday
// words, relative-time words and calendar dates survive masking.
func TestMask_ConsistentPlaceholdersAndTimeWords(t *testing.T) {
	t.Run("one person named by full name and by nickname", func(t *testing.T) {
		c := dm("Juniper Okafor called", "Juni says hi")
		c.Participants[threadctx.RoleThem] = threadctx.Participant{ContactID: &tobias}
		entries := mask(t, c).Entries()
		first := strings.TrimSuffix(entries[0].Text, " called")
		second := strings.TrimSuffix(entries[1].Text, " says hi")
		assert.Equal(t, first, second)
		assertGone(t, mask(t, c), "jun", "okafor")
	})

	t.Run("a first name two people share", func(t *testing.T) {
		wren := uuid.MustParse("00000000-0000-4000-8000-000000000003")
		c := dm("Juniper Wren called", "Juni called", "Juniper Okafor called")
		c.Participants[threadctx.RoleThem] = threadctx.Participant{ContactID: &wren}
		dictionary := []repository.ContactName{{ContactID: juniper, Name: "Juniper Okafor"}, {ContactID: wren, Name: "Juniper Wren"}, {ContactID: wren, Name: "Juni"}}
		masked, err := threadctx.NewMasker(dictionary).Mask(t.Context(), c, nil)
		require.NoError(t, err)
		assert.Equal(t, []string{"THEM called", "THEM called", "NAME_1 called"}, entryTexts(masked.Entries()))
	})

	for _, text := range []string{"call 5550104477 2026-10-17", "call 5550104477 17 Oct"} {
		t.Run("a calendar date beside a phone number: "+text, func(t *testing.T) {
			masked := mask(t, dm(text)).Entries()[0].Text
			assert.Equal(t, strings.Replace(text, "5550104477", "PHONE_1", 1), masked)
		})
	}

	for _, words := range []string{"Friday", "tomorrow", "next week", "Oct 17", "2026-10-17"} {
		t.Run(words, func(t *testing.T) {
			text := mask(t, dm("Juniper can do "+words)).Entries()[0].Text
			assert.Equal(t, "THEM can do "+words, text)
		})
	}
}

// A name the dictionary does not cover is masked when it does not begin
// a sentence or line, and every word of an address header is masked.
func TestMask_NamesOutsideTheDictionary(t *testing.T) {
	t.Run("a capitalized first name of a non-contact", func(t *testing.T) {
		assertGone(t, mask(t, dm("I ran into Priya at the market")), "priya")
	})

	t.Run("a non-contact's full name in a sender header", func(t *testing.T) {
		c := dm("hello")
		c.Entries[0].Headers = map[string]string{"from": "Priya Raman <p.r@example.test>"}
		assertGone(t, mask(t, c), "priya", "raman")
	})
}
