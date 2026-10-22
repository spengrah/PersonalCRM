package threadctx

import (
	"cmp"
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
//     identifier: each full name and each of its words, whatever source gave
//     the name. The dictionary holds the user's own names, the CRM's contact
//     names and the display names stored messages give their senders and
//     recipients;
//   - every remaining word of an address header (from, to, cc, bcc,
//     reply-to, sender).
//
// It also masks every name span the name model reports in an entry's text or
// in its other headers, and Mask refuses an entry the masker has not covered
// with spans (Cover), so no request is masked without them. No rule masks a
// word for being capitalized and unknown: the name model finds the names the
// dictionary does not hold. A span masks every word it touches, whole, unless
// each of them is a common English word written in lower case, as a
// dictionary match does.
//
// A common English word written in lower case is no name: a name matches only
// when one of its words is capitalized or is no common word. A contraction
// that is a common word ("I'll", "don't") stays whole and is no name, unless a
// name the dictionary holds is written with that very word ("Teal'c"). A word
// ending in 's is its head word and a suffix ("Emily's", "it's"): the head
// matches a name by the rule every word does, and the suffix stays beside it.
//
// The user's own names become ME, even where another identity's name shares
// one of their words; a participant's names and handles become its role label
// (ME, THEM, PERSON_n); anything else becomes a numbered placeholder
// (NAME_n, EMAIL_n, PHONE_n, URL_n, HANDLE_n), one per distinct value within
// the request. Weekday, month and relative-time words never match a name, and
// no identifier takes a calendar date or any part of one, so they survive.
//
// Question IDs and option keys are identifiers and pass through unchanged.
type Masker struct {
	dictionary Dictionary
	names      *NameSpans
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

// NewMasker builds a Masker over the dictionary that covers requests with the
// name spans of names; with none, it covers no request. It refuses a
// dictionary that holds none of the user's own names, since every request may
// hold them.
func NewMasker(d Dictionary, names *NameSpans) (*Masker, error) {
	if !slices.ContainsFunc(d.Own, func(name string) bool { return len(tokenize(name)) > 0 }) {
		return nil, errNoOwnNames
	}
	return &Masker{dictionary: d, names: names}, nil
}

// LoadMasker builds a Masker over the user's own names and every name the
// CRM holds for its live contacts and its stored messages' senders and
// recipients, covering requests with the name spans of names.
func LoadMasker(ctx context.Context, content *repository.ThreadContentRepository, own []string, names *NameSpans) (*Masker, error) {
	contacts, err := content.ListContactNamesForMasking(ctx)
	if err != nil {
		return nil, fmt.Errorf("list contact names for masking: %w", err)
	}
	display, err := content.ListDisplayNamesForMasking(ctx)
	if err != nil {
		return nil, fmt.Errorf("list display names for masking: %w", err)
	}
	return NewMasker(Dictionary{Own: own, Contacts: contacts, Display: display}, names)
}

// maskingRules versions the rules this file masks by. Change it with any change
// to what Mask hides or keeps: every masked method's version derives from
// MaskingVersion, so a rerun then judges again instead of reusing judgments
// made on the old masking.
const maskingRules = "2026-10-22.1"

// MaskingVersion identifies how Mask masks with the name model's spans of the
// version: its rules, the content of the common-word list they read and the
// model version. A dictionary's names change with contacts and messages and
// are no part of it, so every Masker gives one version, and a caller without
// one derives the same.
func MaskingVersion(model ModelVersion) string {
	sum := sha256.Sum256([]byte(maskingRules + "\x00" + commonWordsFile + "\x00" + model.String()))
	return hex.EncodeToString(sum[:6])
}

// Cover returns the context with every entry covered by the name spans of the
// model version: the spans the span store holds for each of its texts, the
// model asked for those it lacks. It fails with ErrNoNameSpans when any text
// is left without spans, the model down or refusing, so the request is not
// masked and not sent.
func (m *Masker) Cover(ctx context.Context, model ModelVersion, c Context) (Context, error) {
	if m.names == nil {
		return Context{}, fmt.Errorf("%w: no name model", ErrNoNameSpans)
	}
	found, missed, err := m.names.ensure(ctx, model, c.Entries)
	if err != nil {
		return Context{}, err
	}
	out := covered(c, func(message, text string) ([]NameSpan, bool) {
		spans, ok := found[keyOf(message, text)]
		return spans, ok
	})
	for _, e := range out.Entries {
		for _, text := range e.modelTexts() {
			if _, err := e.namesOf(text); err != nil {
				return Context{}, fmt.Errorf("%w: the name model failed: %w", ErrNoNameSpans, missed)
			}
		}
	}
	return out, nil
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
// this call. It fails with ErrNoNameSpans when an entry's text or header was
// not covered (Cover) as it stands.
func (m *Masker) Mask(ctx context.Context, c Context, questions []Question) (MaskedRequest, error) {
	if err := ctx.Err(); err != nil {
		return MaskedRequest{}, err
	}
	s := newMasking(m.dictionary, c.Participants)
	out := MaskedRequest{kind: c.Kind, cutOff: c.CutOff, entries: make([]Entry, len(c.Entries)), questions: make([]Question, len(questions))}
	for i, entry := range c.Entries {
		names, err := entry.namesOf(entry.Text)
		if err != nil {
			return MaskedRequest{}, err
		}
		masked := Entry{Role: entry.Role, SentAt: entry.SentAt, Text: s.mask(entry.Text, false, names)}
		if entry.Headers != nil {
			masked.Headers = make(map[string]string, len(entry.Headers))
			for _, name := range slices.Sorted(maps.Keys(entry.Headers)) {
				value := entry.Headers[name]
				if addressHeaders[strings.ToLower(name)] {
					masked.Headers[name] = s.mask(value, true, nil)
					continue
				}
				names, err := entry.namesOf(value)
				if err != nil {
					return MaskedRequest{}, err
				}
				masked.Headers[name] = s.mask(value, false, names)
			}
		}
		out.entries[i] = masked
	}
	for i, q := range questions {
		masked := Question{ID: q.ID, Kind: q.Kind, Instructions: s.mask(q.Instructions, false, nil), Options: make([]Option, len(q.Options))}
		for j, option := range q.Options {
			masked.Options[j] = Option{Key: option.Key, Label: s.mask(option.Label, false, nil), Description: s.mask(option.Description, false, nil)}
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
	// ownTerms, participantTerms, contactTerms and displayTerms map a
	// lowercased word sequence to the identity it names; the user's own names
	// win over a participant's, a participant's over a contact's, and a
	// contact's over a display name's. Every own term names ME, so no other
	// identity's name can take one from it.
	ownTerms, participantTerms, contactTerms, displayTerms map[string]string
	longestTerm                                            int
	// namedWords are the words written with an apostrophe ("teal'c") that the
	// dictionary's names hold: none of them is a contraction.
	namedWords map[string]bool
	// identifiers maps a normalized identifier a participant owns to its identity.
	identifiers  map[string]string
	placeholders map[string]string
	counts       map[string]int
}

func newMasking(d Dictionary, participants map[Role]Participant) *masking {
	s := &masking{ownTerms: map[string]string{}, participantTerms: map[string]string{}, contactTerms: map[string]string{}, displayTerms: map[string]string{}, namedWords: map[string]bool{}, identifiers: map[string]string{}, placeholders: map[string]string{}, counts: map[string]int{}}
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
			s.addName(s.participantTerms, alias, identity)
		}
	}
	for _, name := range d.Own {
		s.addName(s.ownTerms, name, me)
	}
	contact := func(id uuid.UUID) string {
		if role, ok := roleOfContact[id.String()]; ok {
			return "role:" + string(role)
		}
		return "contact:" + id.String()
	}
	for _, n := range d.Contacts {
		s.addName(s.contactTerms, n.Name, contact(n.ContactID))
	}
	for _, n := range d.Display {
		if _, _, ok := identifier(strings.TrimSpace(n.Name)); ok {
			continue
		}
		identity := "display:" + n.Owner
		if n.ContactID != nil {
			identity = contact(*n.ContactID)
		}
		s.addName(s.displayTerms, n.Name, identity)
	}
	return s
}

// addName adds a name's full word sequence and each of its words as terms, and
// records each of its words written with an apostrophe. A term two identities
// share names neither, so that term alone gets an identity of its own; the
// name's other terms keep naming its identity.
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
		// A protected word is a term too, which lookup never matches alone, so
		// a name span over it finds the identity it names.
		if utf8.RuneCountInString(t.lower) >= 2 && !isDigits(t.lower) {
			add(t.lower)
		}
	}
	add(strings.Join(words, " "))
	s.longestTerm = max(s.longestTerm, len(tokens))
	named, _ := apostrophized(name, tokens)
	for _, word := range named {
		if word != "" {
			s.namedWords[word] = true
		}
	}
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

// mask replaces every identifier and name in text, the names of the
// dictionary and those of the name spans. In an address header every word
// outside an identifier is a name.
func (s *masking) mask(text string, addressHeader bool, names []NameSpan) string {
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
	for _, words := range s.runsBetween(text, spans) {
		for i := 0; i < len(words); {
			if n, identity := s.lookup(text, words[i:]); n > 0 {
				spans = append(spans, span{words[i].start, words[i+n-1].end, s.placeholder(identity, "NAME")})
				i += n
				continue
			}
			t := words[i]
			if addressHeader && !protectedWords[t.lower] && !isDigits(t.lower) {
				spans = append(spans, span{t.start, t.end, s.placeholder("word:"+t.lower, "NAME")})
			}
			i++
		}
	}
	spans = append(spans, s.spanned(text, names)...)
	var out strings.Builder
	last := 0
	for _, sp := range s.merged(text, spans) {
		out.WriteString(text[last:sp.start])
		out.WriteString(sp.label)
		last = sp.end
	}
	out.WriteString(text[last:])
	return out.String()
}

// spanned returns the ranges the name spans mask, unlabeled: each span
// widened to every word it touches, whole, a word written with apostrophes
// included ("Teal'c"). A span every word of which is a common English word
// written in lower case is no name, as a dictionary match is none, and masks
// nothing; an 's suffix is no word of it.
func (s *masking) spanned(text string, names []NameSpan) []span {
	if len(names) == 0 {
		return nil
	}
	tokens := tokenize(text)
	joined, suffix := apostrophized(text, tokens)
	// The range of the word each token is part of.
	wordStart, wordEnd := make([]int, len(tokens)), make([]int, len(tokens))
	for i, t := range tokens {
		wordStart[i], wordEnd[i] = t.start, t.end
		if i > 0 && joined[i] != "" && joined[i] == joined[i-1] && apostropheBetween(text, tokens[i-1], t) {
			wordStart[i] = wordStart[i-1]
		}
	}
	for i := len(tokens) - 2; i >= 0; i-- {
		if wordStart[i+1] == wordStart[i] {
			wordEnd[i] = wordEnd[i+1]
		}
	}
	var out []span
	for _, n := range names {
		start, end, named := n.Start, n.End, false
		for i, t := range tokens {
			if t.end <= n.Start || t.start >= n.End {
				continue
			}
			start, end = min(start, wordStart[i]), max(end, wordEnd[i])
			if suffix[i] {
				continue
			}
			if !ordinaryWord(text[wordStart[i]:wordEnd[i]], cmp.Or(joined[i], t.lower)) {
				named = true
			}
		}
		if named {
			out = append(out, span{start: start, end: end})
		}
	}
	return out
}

// apostropheBetween reports whether one apostrophe alone joins two tokens.
func apostropheBetween(text string, a, b token) bool {
	r, size := utf8.DecodeRuneInString(text[a.end:])
	return strings.ContainsRune(apostrophes, r) && a.end+size == b.start
}

// merged returns the spans in text order with every group of overlapping
// spans joined into one, so no character of any of them is left unmasked.
// A group takes the label of its earliest labeled span, an identifier or a
// dictionary match. A group of name spans alone takes the label of the
// identity its words name in the dictionary, whatever rule kept the
// dictionary from matching them ("May" for a participant named May), and is
// otherwise an unknown name, one placeholder per distinct word sequence within
// the request.
func (s *masking) merged(text string, spans []span) []span {
	slices.SortStableFunc(spans, func(a, b span) int { return a.start - b.start })
	var out []span
	for i := 0; i < len(spans); {
		group := spans[i]
		for i++; i < len(spans) && spans[i].start < group.end; i++ {
			group.end = max(group.end, spans[i].end)
			if group.label == "" {
				group.label = spans[i].label
			}
		}
		if group.label == "" {
			words := text[group.start:group.end]
			tokens := tokenize(words)
			_, suffix := apostrophized(words, tokens)
			var parts []string
			for j, t := range tokens {
				if !suffix[j] {
					parts = append(parts, t.lower)
				}
			}
			term := strings.Join(parts, " ")
			identity, known := s.identityOf(term)
			if !known {
				identity = "name:" + term
			}
			group.label = s.placeholder(identity, "NAME")
		}
		out = append(out, group)
	}
	return out
}

// runsBetween returns the words of text outside the identifier spans, which
// are in text order, split into runs at each span, at each contraction and at
// each 's suffix, none of which is a name: no name match crosses any of them,
// so every span rendered is disjoint from the others. A contraction is a word
// written with an apostrophe that is a common word ("I'll", "don't") and that
// no name of the dictionary holds ("Teal'c"). Every other such word's parts
// are words of their own.
func (s *masking) runsBetween(text string, identifiers []span) [][]token {
	runs := [][]token{nil}
	next := 0
	tokens := tokenize(text)
	joined, suffix := apostrophized(text, tokens)
	for i, t := range tokens {
		for next < len(identifiers) && identifiers[next].end <= t.start {
			next++
			runs = append(runs, nil)
		}
		contraction := commonWords[joined[i]] && !s.namedWords[joined[i]]
		if suffix[i] || contraction || (next < len(identifiers) && identifiers[next].start < t.end) {
			runs = append(runs, nil)
			continue
		}
		runs[len(runs)-1] = append(runs[len(runs)-1], t)
	}
	return runs
}

// apostrophes join the parts of a contraction or of a possessive.
const apostrophes = "'’"

// apostrophized returns, for each token, the word written with an apostrophe
// it is a part of ("I'll", "Teal'c"), its parts lowercased and joined by ',
// and "" for a token that is a word alone. A final 's is a suffix and no part
// of that word: suffix reports its s, and the head before it is a word alone
// ("Emily's", "it's") or the word its own apostrophes join ("O'Neill's").
func apostrophized(text string, tokens []token) (words []string, suffix []bool) {
	words, suffix = make([]string, len(tokens)), make([]bool, len(tokens))
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
		end := j
		if len(parts) > 1 && parts[len(parts)-1] == "s" {
			suffix[j-1] = true
			parts, end = parts[:len(parts)-1], j-1
		}
		if len(parts) > 1 {
			word := strings.Join(parts, "'")
			for k := i; k < end; k++ {
				words[k] = word
			}
		}
		i = j
	}
	return words, suffix
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
		if identity, ok := s.identityOf(strings.Join(parts, " ")); ok {
			return n, identity
		}
	}
	return 0, ""
}

// identityOf returns the identity a dictionary term names, the user's own
// names first, then a participant's, a contact's and a display name's.
func (s *masking) identityOf(term string) (string, bool) {
	for _, terms := range []map[string]string{s.ownTerms, s.participantTerms, s.contactTerms, s.displayTerms} {
		if identity, ok := terms[term]; ok {
			return identity, true
		}
	}
	return "", false
}

// namesSomeone reports whether a word can make a name: it is not an ordinary
// word.
func namesSomeone(t token) bool {
	return !ordinaryWord(t.word, t.lower)
}

// ordinaryWord reports whether a word, written as written and looked up as
// common, names no one (G1): every letter of it is lower case and the
// common-word list holds it. One upper-case letter anywhere ("Will", "wILL")
// makes it a possible name.
func ordinaryWord(written, common string) bool {
	return written == strings.ToLower(written) && commonWords[common]
}
