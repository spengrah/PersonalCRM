package threadctx

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
)

// Masker masks a thread context and its questions before they leave the Pi.
//
// It replaces, everywhere in the request (entry texts, every header, question
// instructions, option labels and descriptions):
//
//   - formatted identifiers (email addresses, URLs, @handles, phone numbers of
//     seven or more digits);
//   - every name of its dictionary and the names and handles the sources give
//     each participant, matched as whole words and never across a formatted
//     identifier: each full name and each of its words. The dictionary holds
//     the user's own names, the CRM's contact names and the display names
//     stored messages give their senders and recipients; a display name's
//     common words that are no person's name name no one alone;
//   - names the dictionary does not cover: a capitalized word that does not
//     begin a sentence or line and is no common word, or is a common word that
//     is also a person's name ("Will"), and every remaining word of an address
//     header (from, to, cc, bcc, reply-to, sender).
//
// A common English word written in lower case is no name: a name matches only
// when one of its words is capitalized or is no common word. A contraction
// that is a common word ("I'll", "don't") stays whole and is no name, and a
// possessive suffix stays apart from the name before it ("Emily's").
//
// The user's own names and a participant's names and handles become its role
// label (ME, THEM, PERSON_n); anything else becomes a numbered placeholder
// (NAME_n, EMAIL_n, PHONE_n, URL_n, HANDLE_n), one per distinct value within
// the request. Weekday, month and relative-time words never match a name, and
// no identifier takes a calendar date or any part of one, so they survive. A
// name outside the dictionary that begins a sentence survives.
//
// Question IDs and option keys are identifiers and pass through unchanged.
type Masker struct {
	dictionary Dictionary
}

// Dictionary is every name a Masker knows before it reads a request.
type Dictionary struct {
	// Own are the user's own names (OWN_NAMES), each a full name or another
	// name the user goes by.
	Own []string
	// Contacts are the names the CRM holds for its live contacts.
	Contacts []repository.ContactName
	// Display are the display names stored messages give their senders and
	// recipients.
	Display []repository.DisplayName
}

// errNoOwnNames refuses a masker that could not hide the user's own names.
var errNoOwnNames = errors.New("the user's own names are unknown: set OWN_NAMES to them, comma-separated")

// NewMasker builds a Masker over the dictionary. It refuses a dictionary that
// holds none of the user's own names, since every request may hold them.
func NewMasker(d Dictionary) (*Masker, error) {
	if !slices.ContainsFunc(d.Own, func(name string) bool { return len(tokenize(name)) > 0 }) {
		return nil, errNoOwnNames
	}
	return &Masker{dictionary: d}, nil
}

// LoadMasker builds a Masker over the user's own names and every name the
// CRM holds for its live contacts and its stored messages' senders and
// recipients.
func LoadMasker(ctx context.Context, content *repository.ThreadContentRepository, own []string) (*Masker, error) {
	contacts, err := content.ListContactNamesForMasking(ctx)
	if err != nil {
		return nil, fmt.Errorf("list contact names for masking: %w", err)
	}
	display, err := content.ListDisplayNamesForMasking(ctx)
	if err != nil {
		return nil, fmt.Errorf("list display names for masking: %w", err)
	}
	return NewMasker(Dictionary{Own: own, Contacts: contacts, Display: display})
}

// maskingRules versions the rules this file masks by. Change it with any change
// to what Mask hides or keeps: every masked method's version derives from
// MaskingVersion, so a rerun then judges again instead of reusing judgments
// made on the old masking.
const maskingRules = "2026-10-21"

// MaskingVersion identifies how Mask masks: its rules and the content of the
// word lists they read. A dictionary's names change with contacts and messages
// and are no part of it, so every Masker gives one version, and a caller
// without one derives the same.
func MaskingVersion() string {
	sum := sha256.Sum256([]byte(maskingRules + "\x00" + commonWordsFile + "\x00" + personNamesFile))
	return hex.EncodeToString(sum[:6])
}

// MaskedRequest is a thread context and its questions with every identifier
// replaced by a placeholder. Only Mask constructs one, and it carries no
// placeholder map and no message key.
type MaskedRequest struct {
	kind      ContextKind
	cutOff    time.Time
	entries   []Entry
	questions []Question
}

// Kind returns the context kind.
func (r MaskedRequest) Kind() ContextKind { return r.kind }

// CutOff returns the cut-off the context was built at.
func (r MaskedRequest) CutOff() time.Time { return r.cutOff }

// Entries returns a copy of the masked entries.
func (r MaskedRequest) Entries() []Entry {
	out := slices.Clone(r.entries)
	for i := range out {
		out[i].Headers = maps.Clone(out[i].Headers)
	}
	return out
}

// Questions returns a copy of the masked questions.
func (r MaskedRequest) Questions() []Question {
	out := slices.Clone(r.questions)
	for i := range out {
		out[i].Options = slices.Clone(out[i].Options)
	}
	return out
}

// Mask returns the whole request masked. The placeholder map lives only for
// this call.
func (m *Masker) Mask(ctx context.Context, c Context, questions []Question) (MaskedRequest, error) {
	if err := ctx.Err(); err != nil {
		return MaskedRequest{}, err
	}
	s := newMasking(m.dictionary, c.Participants)
	out := MaskedRequest{kind: c.Kind, cutOff: c.CutOff, entries: make([]Entry, len(c.Entries)), questions: make([]Question, len(questions))}
	for i, entry := range c.Entries {
		masked := Entry{Role: entry.Role, SentAt: entry.SentAt, Text: s.mask(entry.Text, false)}
		if entry.Headers != nil {
			masked.Headers = make(map[string]string, len(entry.Headers))
			for _, name := range slices.Sorted(maps.Keys(entry.Headers)) {
				masked.Headers[name] = s.mask(entry.Headers[name], addressHeaders[strings.ToLower(name)])
			}
		}
		out.entries[i] = masked
	}
	for i, q := range questions {
		masked := Question{ID: q.ID, Kind: q.Kind, Instructions: s.mask(q.Instructions, false), Options: make([]Option, len(q.Options))}
		for j, option := range q.Options {
			masked.Options[j] = Option{Key: option.Key, Label: s.mask(option.Label, false), Description: s.mask(option.Description, false)}
		}
		out.questions[i] = masked
	}
	return out, nil
}

// addressHeaders hold only names and addresses, so every word in them is masked.
var addressHeaders = map[string]bool{"from": true, "to": true, "cc": true, "bcc": true, "reply-to": true, "sender": true}

// addressSource matches the identifiers a calendar date can sit inside, one
// category per group: URL, email address, @handle. phoneSource matches a phone
// number. identifierPattern joins them, one category per group in
// identifierCategories order.
const (
	addressSource = `(https?://\S+|www\.\S+)|([a-z0-9._%+\-]+@[a-z0-9.\-]+\.[a-z]{2,})|(@[a-z0-9_]{2,})`
	phoneSource   = `(\+?\(?\d[\d\s().\-]{5,}\d)`
)

var (
	identifierPattern = regexp.MustCompile(`(?i)` + addressSource + `|` + phoneSource)
	addressPattern    = regexp.MustCompile(`(?i)` + addressSource)
	phonePattern      = regexp.MustCompile(phoneSource)
)

var identifierCategories = []string{"URL", "EMAIL", "HANDLE", "PHONE"}

// numericRun matches numbers joined by dots, slashes or hyphens. A numeric
// date is a whole run, never part of a longer one such as 01.23.45.67.89.
var numericRun = regexp.MustCompile(`\d+(?:[./-]\d+)+`)

// calendarDate matches a numeric date such as 2026-10-17, 17.10.2026 or 10/17/26.
var calendarDate = regexp.MustCompile(`^(?:\d{4}[./-]\d{1,2}[./-]\d{1,2}|\d{1,2}[./-]\d{1,2}[./-](?:\d{2}|\d{4}))$`)

// namedDate matches a date with a month name, such as "Oct 17", "17 October",
// "Oct. 17th, 2026" or "17th of Oct".
var namedDate = func() *regexp.Regexp {
	month := `(?:jan(?:uary)?|feb(?:ruary)?|mar(?:ch)?|apr(?:il)?|may|june?|july?|aug(?:ust)?|sep(?:t(?:ember)?)?|oct(?:ober)?|nov(?:ember)?|dec(?:ember)?)\b\.?`
	day := `\d{1,2}(?:st|nd|rd|th)?`
	year := `(?:,?\s+\d{4})?`
	return regexp.MustCompile(`(?i)\b(?:` + day + `\s+(?:of\s+)?` + month + year + `|` + month + `\s+` + day + year + `)\b`)
}()

// withoutDates returns text with every calendar date blanked out, byte for
// byte, so no phone match takes a date or any part of one.
func withoutDates(text string) string {
	out := []byte(text)
	blank := func(m []int) {
		for i := m[0]; i < m[1]; i++ {
			out[i] = 0
		}
	}
	for _, m := range numericRun.FindAllStringIndex(text, -1) {
		if calendarDate.MatchString(text[m[0]:m[1]]) {
			blank(m)
		}
	}
	for _, m := range namedDate.FindAllStringIndex(text, -1) {
		blank(m)
	}
	return string(out)
}

// protectedWords never match a name: weekdays, months and relative-time words.
var protectedWords = map[string]bool{}

func init() {
	for _, word := range strings.Fields(`
		monday tuesday wednesday thursday friday saturday sunday
		mon tue tues wed thu thur thurs fri sat sun
		january february march april may june july august september october november december
		jan feb mar apr jun jul aug sep sept oct nov dec
		today tomorrow tonight yesterday morning afternoon evening night noon midnight
		week weekend weekday month year next last this soon later now ago earlier`) {
		protectedWords[word] = true
	}
}

// identifier returns the category and normalized value of a formatted
// identifier, and false when the text is not one.
func identifier(text string) (category, value string, ok bool) {
	groups := identifierPattern.FindStringSubmatchIndex(text)
	if groups == nil || groups[0] != 0 || groups[1] != len(text) {
		return "", "", false
	}
	for i, category := range identifierCategories {
		if groups[2*(i+1)] >= 0 {
			return normalizeIdentifier(category, text)
		}
	}
	return "", "", false
}

func normalizeIdentifier(category, text string) (string, string, bool) {
	if category != "PHONE" {
		return category, strings.ToLower(text), true
	}
	digits := strings.Map(func(r rune) rune {
		if unicode.IsDigit(r) {
			return r
		}
		return -1
	}, text)
	if len(digits) < 7 {
		return "", "", false
	}
	// A country code does not make a different number.
	if len(digits) > 10 {
		digits = digits[len(digits)-10:]
	}
	return category, digits, true
}

// token is one word of a text: a run of letters, digits or marks.
type token struct {
	start, end int
	lower      string
	word       string
}

func tokenize(text string) []token {
	var out []token
	start := -1
	for i, r := range text {
		inWord := unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsMark(r)
		if inWord && start < 0 {
			start = i
		}
		if !inWord && start >= 0 {
			out = append(out, token{start: start, end: i, word: text[start:i], lower: strings.ToLower(text[start:i])})
			start = -1
		}
	}
	if start >= 0 {
		out = append(out, token{start: start, end: len(text), word: text[start:], lower: strings.ToLower(text[start:])})
	}
	return out
}

// masking holds one request's dictionary and placeholder map.
type masking struct {
	// participantTerms, contactTerms and displayTerms map a lowercased word
	// sequence to the identity it names; the user's and each participant's
	// own names win over a contact's, and a contact's over a display name's.
	participantTerms, contactTerms, displayTerms map[string]string
	longestTerm                                  int
	// identifiers maps a normalized identifier a participant owns to its identity.
	identifiers  map[string]string
	placeholders map[string]string
	counts       map[string]int
}

func newMasking(d Dictionary, participants map[Role]Participant) *masking {
	s := &masking{participantTerms: map[string]string{}, contactTerms: map[string]string{}, displayTerms: map[string]string{}, identifiers: map[string]string{}, placeholders: map[string]string{}, counts: map[string]int{}}
	me := "role:" + string(RoleMe)
	s.placeholders[me] = string(RoleMe)
	roleOfContact := map[string]Role{}
	for _, role := range slices.Sorted(maps.Keys(participants)) {
		p := participants[role]
		identity := "role:" + string(role)
		s.placeholders[identity] = string(role)
		if p.ContactID != nil {
			roleOfContact[p.ContactID.String()] = role
		}
		for _, alias := range p.Aliases {
			alias = strings.TrimSpace(alias)
			if category, value, ok := identifier(alias); ok {
				s.identifiers[category+":"+value] = identity
				continue
			}
			s.addName(s.participantTerms, alias, identity, true)
		}
	}
	for _, name := range d.Own {
		s.addName(s.participantTerms, name, me, true)
	}
	contact := func(id uuid.UUID) string {
		if role, ok := roleOfContact[id.String()]; ok {
			return "role:" + string(role)
		}
		return "contact:" + id.String()
	}
	for _, n := range d.Contacts {
		s.addName(s.contactTerms, n.Name, contact(n.ContactID), true)
	}
	for _, n := range d.Display {
		if _, _, ok := identifier(strings.TrimSpace(n.Name)); ok {
			continue
		}
		identity := "display:" + n.Owner
		if n.ContactID != nil {
			identity = contact(*n.ContactID)
		}
		s.addName(s.displayTerms, n.Name, identity, false)
	}
	return s
}

// addName adds a name's full word sequence and each of its words as terms. A
// term two identities share names neither, so that term alone gets an identity
// of its own; the name's other terms keep naming its identity. Unless every
// word names, a term whose every word is a common word that is no person's
// name is left out, so an organization's display name ("The New York Times")
// makes no ordinary word a name.
func (s *masking) addName(terms map[string]string, name, identity string, everyWord bool) {
	tokens := tokenize(name)
	if len(tokens) == 0 {
		return
	}
	add := func(term string, words ...string) {
		if !everyWord && !slices.ContainsFunc(words, func(w string) bool { return !ordinary(w) }) {
			return
		}
		if existing, ok := terms[term]; ok && existing != identity {
			terms[term] = "term:" + term
			return
		}
		terms[term] = identity
	}
	words := make([]string, len(tokens))
	for i, t := range tokens {
		words[i] = t.lower
		if utf8.RuneCountInString(t.lower) >= 2 && !protectedWords[t.lower] && !isDigits(t.lower) {
			add(t.lower, t.lower)
		}
	}
	add(strings.Join(words, " "), words...)
	s.longestTerm = max(s.longestTerm, len(tokens))
}

func isDigits(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return !unicode.IsDigit(r) }) < 0
}

// placeholder returns the label of an identity, numbering a new one within its
// category.
func (s *masking) placeholder(identity, category string) string {
	if label, ok := s.placeholders[identity]; ok {
		return label
	}
	s.counts[category]++
	label := category + "_" + strconv.Itoa(s.counts[category])
	s.placeholders[identity] = label
	return label
}

type span struct {
	start, end int
	label      string
}

// mask replaces every identifier and name in text. In an address header every
// word outside an identifier is a name.
func (s *masking) mask(text string, addressHeader bool) string {
	var spans []span
	add := func(match []int) {
		category, value, ok := identifier(text[match[0]:match[1]])
		if !ok {
			return
		}
		identity, owned := s.identifiers[category+":"+value]
		if !owned {
			identity = category + ":" + value
		}
		spans = append(spans, span{match[0], match[1], s.placeholder(identity, category)})
	}
	// URLs, email addresses and handles are matched over the original text, so
	// a date inside one never hides it. Phone numbers are matched with calendar
	// dates and address spans blanked, so a date is never read as or into a
	// phone number and no phone match reaches into an address.
	for _, match := range addressPattern.FindAllStringIndex(text, -1) {
		add(match)
	}
	blanked := []byte(withoutDates(text))
	for _, sp := range spans {
		for i := sp.start; i < sp.end; i++ {
			blanked[i] = 0
		}
	}
	for _, match := range phonePattern.FindAllStringIndex(string(blanked), -1) {
		add(match)
	}
	slices.SortFunc(spans, func(a, b span) int { return a.start - b.start })
	for _, words := range runsBetween(text, spans) {
		for i := 0; i < len(words); {
			if n, identity := s.lookup(text, words[i:]); n > 0 {
				spans = append(spans, span{words[i].start, words[i+n-1].end, s.placeholder(identity, "NAME")})
				i += n
				continue
			}
			t := words[i]
			if !protectedWords[t.lower] && !isDigits(t.lower) && (addressHeader || (capitalized(t.word) && !beginsSentence(text, t.start) && !ordinary(t.lower))) {
				spans = append(spans, span{t.start, t.end, s.placeholder("word:"+t.lower, "NAME")})
			}
			i++
		}
	}
	slices.SortFunc(spans, func(a, b span) int { return a.start - b.start })
	var out strings.Builder
	last := 0
	for _, sp := range spans {
		out.WriteString(text[last:sp.start])
		out.WriteString(sp.label)
		last = sp.end
	}
	out.WriteString(text[last:])
	return out.String()
}

// runsBetween returns the words of text outside the identifier spans, which
// are in text order, split into runs at each span and at each contraction that
// is a common word, which stays whole and is no name: no name match crosses
// either, so every span rendered is disjoint from the others.
func runsBetween(text string, identifiers []span) [][]token {
	runs := [][]token{nil}
	next := 0
	tokens := tokenize(text)
	contracted := inContraction(text, tokens)
	for i, t := range tokens {
		for next < len(identifiers) && identifiers[next].end <= t.start {
			next++
			runs = append(runs, nil)
		}
		if contracted[i] || (next < len(identifiers) && identifiers[next].start < t.end) {
			runs = append(runs, nil)
			continue
		}
		runs[len(runs)-1] = append(runs[len(runs)-1], t)
	}
	return runs
}

// apostrophes join the parts of a contraction or of a possessive.
const apostrophes = "'’"

// inContraction marks each token that is a part of a contraction that is a
// common word ("I'll", "don't"). Every other word's parts are tokens of their
// own, so a possessive suffix stays apart from the name before it.
func inContraction(text string, tokens []token) []bool {
	out := make([]bool, len(tokens))
	for i := 0; i < len(tokens); {
		parts := []string{tokens[i].lower}
		j := i + 1
		for ; j < len(tokens); j++ {
			r, size := utf8.DecodeRuneInString(text[tokens[j-1].end:])
			if !strings.ContainsRune(apostrophes, r) || tokens[j-1].end+size != tokens[j].start {
				break
			}
			parts = append(parts, tokens[j].lower)
		}
		if len(parts) > 1 && commonWords[strings.Join(parts, "'")] {
			for k := i; k < j; k++ {
				out[k] = true
			}
		}
		i = j
	}
	return out
}

// lookup returns how many words, from the first, form the longest dictionary
// term, and the identity it names; zero when none does. A term's words lie on
// one line, and one of them is capitalized or is no common word.
func (s *masking) lookup(text string, words []token) (int, string) {
	for n := min(s.longestTerm, len(words)); n >= 1; n-- {
		if n > 1 && strings.Contains(text[words[0].start:words[n-1].end], "\n") {
			continue
		}
		if n == 1 && protectedWords[words[0].lower] {
			continue
		}
		if !slices.ContainsFunc(words[:n], namesSomeone) {
			continue
		}
		parts := make([]string, n)
		for i, w := range words[:n] {
			parts[i] = w.lower
		}
		term := strings.Join(parts, " ")
		for _, terms := range []map[string]string{s.participantTerms, s.contactTerms, s.displayTerms} {
			if identity, ok := terms[term]; ok {
				return n, identity
			}
		}
	}
	return 0, ""
}

// namesSomeone reports whether a word can make a name: it is capitalized or
// is no common word, since a common word written in lower case is no name.
func namesSomeone(t token) bool {
	first, _ := utf8.DecodeRuneInString(t.word)
	return unicode.IsUpper(first) || !commonWords[t.lower]
}

// capitalized reports a word of two or more letters whose first letter alone is
// upper case.
func capitalized(word string) bool {
	first, size := utf8.DecodeRuneInString(word)
	rest := word[size:]
	return unicode.IsUpper(first) && rest != "" && strings.IndexFunc(rest, func(r rune) bool { return !unicode.IsLower(r) }) < 0
}

// beginsSentence reports whether the word at start opens the text, a line or a
// sentence.
func beginsSentence(text string, start int) bool {
	for i := start; i > 0; {
		r, size := utf8.DecodeLastRuneInString(text[:i])
		switch {
		case r == '\n' || r == '.' || r == '!' || r == '?':
			return true
		case unicode.IsSpace(r) || strings.ContainsRune(`"'“‘([-`, r):
			i -= size
		default:
			return false
		}
	}
	return true
}
