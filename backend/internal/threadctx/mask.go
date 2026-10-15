package threadctx

import (
	"context"
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
)

// Masker masks a thread context and its questions before they leave the Pi.
//
// It replaces, everywhere in the request (entry texts, every header, question
// instructions, option labels and descriptions):
//
//   - formatted identifiers (email addresses, URLs, @handles, phone numbers of
//     seven or more digits);
//   - the CRM's contact names and the names and handles the sources give each
//     participant, matched as whole words regardless of case and never across a
//     formatted identifier: each full name and each of its words;
//   - names the dictionary does not cover: a capitalized word that does not
//     begin a sentence or line, and every remaining word of an address header
//     (from, to, cc, bcc, reply-to, sender).
//
// A participant's names and handles become its role label (ME, THEM,
// PERSON_n); anything else becomes a numbered placeholder (NAME_n, EMAIL_n,
// PHONE_n, URL_n, HANDLE_n), one per distinct value within the request.
// Weekday, month and relative-time words never match a name, and no
// identifier takes a calendar date or any part of one, so they survive. A name
// outside the dictionary that begins a sentence survives.
//
// Question IDs and option keys are identifiers and pass through unchanged.
type Masker struct {
	names []repository.ContactName
}

// NewMasker builds a Masker over the given contact names.
func NewMasker(names []repository.ContactName) *Masker {
	return &Masker{names: names}
}

// LoadMasker builds a Masker over every name the CRM holds for its live
// contacts.
func LoadMasker(ctx context.Context, content *repository.ThreadContentRepository) (*Masker, error) {
	names, err := content.ListContactNamesForMasking(ctx)
	if err != nil {
		return nil, fmt.Errorf("list contact names for masking: %w", err)
	}
	return NewMasker(names), nil
}

// MaskedRequest is a thread context and its questions with every identifier
// replaced by a placeholder. Only Mask constructs one, and it carries no
// placeholder map.
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
	s := newMasking(m.names, c.Participants)
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
	// participantTerms and contactTerms map a lowercased word sequence to the
	// identity it names; a participant's own names win over a contact's.
	participantTerms, contactTerms map[string]string
	longestTerm                    int
	// identifiers maps a normalized identifier a participant owns to its identity.
	identifiers  map[string]string
	placeholders map[string]string
	counts       map[string]int
}

func newMasking(names []repository.ContactName, participants map[Role]Participant) *masking {
	s := &masking{participantTerms: map[string]string{}, contactTerms: map[string]string{}, identifiers: map[string]string{}, placeholders: map[string]string{}, counts: map[string]int{}}
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
			s.addName(s.participantTerms, alias, identity)
		}
	}
	for _, n := range names {
		identity := "contact:" + n.ContactID.String()
		if role, ok := roleOfContact[n.ContactID.String()]; ok {
			identity = "role:" + string(role)
		}
		s.addName(s.contactTerms, n.Name, identity)
	}
	return s
}

// addName adds a name's full word sequence and each of its words as terms. A
// term two identities share names neither, so that term alone gets an identity
// of its own; the name's other terms keep naming its identity.
func (s *masking) addName(terms map[string]string, name, identity string) {
	tokens := tokenize(name)
	if len(tokens) == 0 {
		return
	}
	add := func(term string) {
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
			add(t.lower)
		}
	}
	add(strings.Join(words, " "))
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
			if !protectedWords[t.lower] && !isDigits(t.lower) && (addressHeader || (capitalized(t.word) && !beginsSentence(text, t.start))) {
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
// are in text order, split into runs at each span: no name match crosses an
// identifier, so every span rendered is disjoint from the others.
func runsBetween(text string, identifiers []span) [][]token {
	runs := [][]token{nil}
	next := 0
	for _, t := range tokenize(text) {
		for next < len(identifiers) && identifiers[next].end <= t.start {
			next++
			runs = append(runs, nil)
		}
		if next < len(identifiers) && identifiers[next].start < t.end {
			runs = append(runs, nil)
			continue
		}
		runs[len(runs)-1] = append(runs[len(runs)-1], t)
	}
	return runs
}

// lookup returns how many words, from the first, form the longest dictionary
// term, and the identity it names; zero when none does. A term's words lie on
// one line.
func (s *masking) lookup(text string, words []token) (int, string) {
	for n := min(s.longestTerm, len(words)); n >= 1; n-- {
		if n > 1 && strings.Contains(text[words[0].start:words[n-1].end], "\n") {
			continue
		}
		if n == 1 && protectedWords[words[0].lower] {
			continue
		}
		parts := make([]string, n)
		for i, w := range words[:n] {
			parts[i] = w.lower
		}
		term := strings.Join(parts, " ")
		if identity, ok := s.participantTerms[term]; ok {
			return n, identity
		}
		if identity, ok := s.contactTerms[term]; ok {
			return n, identity
		}
	}
	return 0, ""
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
