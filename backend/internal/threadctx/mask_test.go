package threadctx_test

import (
	"fmt"
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
	// own are the user's own names.
	own    = []string{"Marlowe Quill"}
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
	return maskWith(t, threadctx.Dictionary{Own: own, Contacts: names}, c, questions...)
}

func maskWith(t *testing.T, d threadctx.Dictionary, c threadctx.Context, questions ...threadctx.Question) threadctx.MaskedRequest {
	t.Helper()
	masker, err := threadctx.NewMasker(d, nil)
	require.NoError(t, err)
	masked, err := masker.Mask(t.Context(), threadctx.CoverForTest(c, nil), questions)
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
// spec: KNW-089.identifiers-masked
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

	t.Run("a phone number beside an email address", func(t *testing.T) {
		for _, body := range []string{"call 5550104477 123@example.test", "call 5550104477\n123@example.test", "call 5550104477 (123@example.test)"} {
			assertGone(t, mask(t, dm(body)), "5550104477", "123@example")
		}
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
// spec: KNW-089.role-labels-and-placeholders, KNW-089.calendar-words-survive
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
		masked := maskWith(t, threadctx.Dictionary{Own: own, Contacts: dictionary}, c)
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

// Every word of an address header is masked, though the dictionary does not
// hold it.
// spec: KNW-089.address-header-words-masked
func TestMask_AddressHeaderWords(t *testing.T) {
	t.Run("a non-contact's full name in a sender header", func(t *testing.T) {
		c := dm("hello")
		c.Entries[0].Headers = map[string]string{"from": "Priya Raman <p.r@example.test>"}
		assertGone(t, mask(t, c), "priya", "raman")
	})
}

// contactsNamed is a dictionary of one synthetic contact per name.
func contactsNamed(fullNames ...string) threadctx.Dictionary {
	d := threadctx.Dictionary{Own: own}
	for i, name := range fullNames {
		d.Contacts = append(d.Contacts, repository.ContactName{ContactID: uuid.MustParse(fmt.Sprintf("00000000-0000-4000-8000-0000000001%02d", i)), Name: name})
	}
	return d
}

// Masking keeps ordinary words: a contraction stays whole, and a common word
// written in lower case survives though a contact's name holds it.
// spec: KNW-089.contractions-stay-whole, KNW-089.lowercase-common-words-kept
func TestMask_KeepsOrdinaryWords(t *testing.T) {
	for _, tc := range []struct{ first, last, text string }{
		{"I", "Ll", "I'll call you tonight"},
		{"Don", "T", "please don't wait up"},
		{"We", "Re", "we're on our way"},
		{"They", "Ve", "they've left already"},
	} {
		for _, text := range []string{tc.text, strings.ReplaceAll(tc.text, "'", "’")} {
			t.Run("a contraction with contacts whose names hold the words on either side of its apostrophe: "+text, func(t *testing.T) {
				masked := maskWith(t, contactsNamed(tc.first+" Hartigan", tc.last+" Brannock"), dm(text)).Entries()[0].Text
				assert.Equal(t, text, masked)
			})
		}
	}

	for _, tc := range []struct{ contact, text string }{
		{"It Sorensen", "I think it's good to see you"},
		{"Let Hartigan", "ok let's meet tomorrow"},
		{"Mine Hartigan", "yours is red and mine's blue"},
		{"So Hartigan", "he's tired and so's she"},
		{"Here Hartigan", "ok, here's the plan"},
	} {
		for _, text := range []string{tc.text, strings.ReplaceAll(tc.text, "'", "’")} {
			t.Run("a lower-case word ending in 's with a contact whose name holds its head: "+text, func(t *testing.T) {
				masked := maskWith(t, contactsNamed(tc.contact), dm(text)).Entries()[0].Text
				assert.Equal(t, text, masked)
			})
		}
	}

	t.Run("lower-case will, it, is and me while contacts carry those words as names", func(t *testing.T) {
		text := "it is what it is, and will you tell me"
		masked := maskWith(t, contactsNamed("Will Hartigan", "It Sorensen", "Is Abernathy", "Me Lindqvist"), dm(text)).Entries()[0].Text
		assert.Equal(t, text, masked)
	})

	t.Run("Happy New Year mid-sentence with no contact of those names", func(t *testing.T) {
		text := "we wish you a Happy New Year"
		assert.Equal(t, text, mask(t, dm(text)).Entries()[0].Text)
	})
}

// A known name that is also a common word is masked when capitalized, at the
// start of a sentence too, and a word ending in 's is its head word and a
// suffix kept beside it: the head is masked by the same rule as any word.
// spec: KNW-089.capitalized-known-name-masked, KNW-089.possessive-suffix-kept
func TestMask_KnownNamesThatAreWords(t *testing.T) {
	will := contactsNamed("Will Hartigan")
	for _, tc := range []struct{ name, text, want string }{
		{"Will mid-sentence", "I asked Will about the lease", "I asked NAME_1 about the lease"},
		{"Will at the start of a sentence", "Will says the lease is fine", "NAME_1 says the lease is fine"},
		{"wILL, with an upper-case letter past its first", "I asked wILL about the lease", "I asked NAME_1 about the lease"},
	} {
		t.Run(tc.name+" with a contact named Will", func(t *testing.T) {
			assert.Equal(t, tc.want, maskWith(t, will, dm(tc.text)).Entries()[0].Text)
		})
	}

	for _, contact := range []string{"Emily Castellanos", "Emily S."} {
		for _, text := range []string{"Emily's car is outside", "Emily’s car is outside"} {
			t.Run("a possessive of the name of a contact named "+contact+": "+text, func(t *testing.T) {
				masked := maskWith(t, contactsNamed(contact), dm(text)).Entries()[0].Text
				assert.Equal(t, strings.Replace(text, "Emily", "NAME_1", 1), masked)
			})
		}
	}

	for _, tc := range []struct{ contact, text, want string }{
		{"Mine Hartigan", "Mine's ready. Yours?", "NAME_1's ready. Yours?"},
	} {
		for _, apostrophe := range []string{"'", "’"} {
			text, want := strings.ReplaceAll(tc.text, "'", apostrophe), strings.ReplaceAll(tc.want, "'", apostrophe)
			t.Run("a capitalized word ending in 's whose head a contact's name holds, at the start of a sentence: "+text, func(t *testing.T) {
				assert.Equal(t, want, maskWith(t, contactsNamed(tc.contact), dm(text)).Entries()[0].Text)
			})
		}
	}

	t.Run("a possessive of a word that is no name", func(t *testing.T) {
		text := "the dog's bowl is empty"
		assert.Equal(t, text, maskWith(t, contactsNamed("Emily Castellanos"), dm(text)).Entries()[0].Text)
	})
}

// A known name written with an apostrophe is masked like any other name, even
// where the word list holds it as a contraction.
// spec: KNW-089.known-apostrophized-name-masked
func TestMask_KnownNamesWithAnApostrophe(t *testing.T) {
	t.Run("a contact nicknamed Teal'c", func(t *testing.T) {
		d := contactsNamed("Teal'c")
		assert.Equal(t, "NAME_1 called", maskWith(t, d, dm("Teal'c called")).Entries()[0].Text)
		assert.Equal(t, "we spoke with NAME_1 today", maskWith(t, d, dm("we spoke with Teal’c today")).Entries()[0].Text)
	})

	t.Run("a contact nicknamed Y'all, which the word list holds as a contraction", func(t *testing.T) {
		assert.Equal(t, "tell NAME_1 I said hi", maskWith(t, contactsNamed("Y'all"), dm("tell Y'all I said hi")).Entries()[0].Text)
	})
}

// The masker masks no word for being capitalized and unknown: a capitalized
// word that no known name holds comes through unmasked, common word, given
// name or invented word alike, and is the name model's to find.
// spec: KNW-089.unknown-capitalized-kept
func TestMask_UnknownCapitalizedWordsKept(t *testing.T) {
	for _, text := range []string{
		"we wish you a Happy New Year",
		"we had lunch with Rose and Mark today",
		"I ran into Quenbry at the market",
	} {
		t.Run(text, func(t *testing.T) {
			assert.Equal(t, text, mask(t, dm(text)).Entries()[0].Text)
		})
	}
}

// The user's own names are masked as ME, each full name and each of its
// words, wherever they appear, at the start of a sentence included; an own
// name that is also a common word is masked when capitalized.
// spec: KNW-089.own-names-me
func TestMask_OwnNames(t *testing.T) {
	t.Run("the user's first name at the start of a sentence", func(t *testing.T) {
		assert.Equal(t, "ME will be late. ME again", mask(t, dm("Marlowe will be late. Marlowe again")).Entries()[0].Text)
	})

	t.Run("the user's full name and last name mid-sentence", func(t *testing.T) {
		masked := mask(t, dm("ask Marlowe Quill, or just Quill")).Entries()[0].Text
		assert.Equal(t, "ask ME, or just ME", masked)
	})

	t.Run("an own name that is also a common word", func(t *testing.T) {
		d := threadctx.Dictionary{Own: []string{"Hope Ashworth"}}
		masked := maskWith(t, d, dm("Hope says hi, and I hope so")).Entries()[0].Text
		assert.Equal(t, "ME says hi, and I hope so", masked)
	})

	t.Run("an own name's word that a participant's alias shares", func(t *testing.T) {
		c := dm("Marlowe sent it to Marlowe Raven")
		c.Participants[threadctx.RoleThem] = threadctx.Participant{ContactID: &juniper, Aliases: []string{"Marlowe Raven"}}
		assert.Equal(t, "ME sent it to THEM", mask(t, c).Entries()[0].Text)
	})
}
