package factory

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// MethodSpec is a synthetic contact_method description. The replay harness maps
// it onto service.ContactMethodInput (the factory does not import service to
// keep the cycle-free leaf invariant).
type MethodSpec struct {
	Type      string // email | phone | telegram
	Value     string
	IsPrimary bool
}

// ContactSpec is a synthetic contact description the harness writes through the
// real contact service/repository. full_name carries the namespace prefix so the
// prefix-keyed cleanup backstop can find it. Email/Phone/TelegramHandle are
// exposed so source-payload factories targeting THIS contact reuse the exact
// same identifiers (so matching links the replay to the seeded contact).
type ContactSpec struct {
	FullName   string
	Methods    []MethodSpec
	Cadence    *string
	CreatedAt  *time.Time
	provenance *generatorProvenance
	// No LastContacted field: creation never seeds last_contacted (CON-001); only a
	// replayed inbound/mutual interaction moves it, so the seed cannot manufacture a
	// connection that never happened (the #641 regression class).
	Birthday *time.Time
	Location *string
	HowMet   *string

	// Convenience accessors to the primary identifiers (also present in
	// Methods) so adapters can address this contact without re-deriving them.
	Email          string
	Phone          string
	TelegramHandle string
}

// ContactOption customizes a generated ContactSpec.
type ContactOption func(*contactConfig)

type contactConfig struct {
	withEmail            bool
	withPhone            bool
	withTelegram         bool
	noMethods            bool
	cadence              *string
	createdAge           *time.Duration
	recentCreationWindow *time.Duration
	birthday             *time.Time
	howMet               *string
	location             *string
	nameEdge             NameEdge
	nameMarker           string
	twinOf               *ContactSpec
}

// WithEmail adds an email contact_method (default ON if no method option is
// given — see Contact).
func WithEmail() ContactOption { return func(c *contactConfig) { c.withEmail = true } }

// WithPhone adds a phone contact_method.
func WithPhone() ContactOption { return func(c *contactConfig) { c.withPhone = true } }

// WithTelegram adds a telegram contact_method (handle).
func WithTelegram() ContactOption { return func(c *contactConfig) { c.withTelegram = true } }

// WithNoMethods builds a contact carrying NO contact_method (the zero-method
// adversarial shape — the Imports/rematch surfaces + the empty-methods UI state).
// It overrides the email default; a contact with WithNoMethods cannot be the
// target of a MatchSeeded replay (there is no identifier to match on).
func WithNoMethods() ContactOption { return func(c *contactConfig) { c.noMethods = true } }

// WithCadence sets the contact's cadence (e.g. "weekly", "monthly").
func WithCadence(cadence string) ContactOption {
	return func(c *contactConfig) { c.cadence = &cadence }
}

// WithCreatedAge backdates the contact to `age` before the anchor, mirroring the
// create handler: created_at is stamped to that one past instant and last_contacted
// is left unset. Combined with a cadence, a far-enough age reads as overdue — with an
// empty timeline that is honest (added long ago, no interactions logged). Draws no
// PRNG (fixed age).
func WithCreatedAge(age time.Duration) ContactOption {
	return func(c *contactConfig) {
		d := age
		c.createdAge = &d
	}
}

// WithRecentCreation backdates the contact to a deterministic instant within the
// last `window`, again stamping created_at and leaving last_contacted unset.
// Draws one recentOffset from the PRNG (the recent-window jitter).
func WithRecentCreation(window time.Duration) ContactOption {
	return func(c *contactConfig) {
		w := window
		c.recentCreationWindow = &w
	}
}

// WithBirthday1900Sentinel sets the prod 1900-MM-DD month/day-only sentinel —
// a representative edge case a declaration or adversarial edge can build on.
//
// It PANICS when the requested (month, day) does not round-trip through the
// sentinel year. 1900 is not a leap year (divisible by 100, not by 400), so
// time.Date normalizes February 29 to March 1 — and a seed that silently stores
// a different date than the one it was asked for is a fixture lying about what
// it represents. The sharper statement is that a year-UNKNOWN February 29
// birthday is not expressible in the product's storage convention at all (1900
// is the year-unknown sentinel the UI keys its age suppression on), so the
// caller has to choose a real leap birth year instead — see LeapSafeBirthYear.
// A panic is right here: this is a programming error in deterministic seed code,
// the same class as exhausting the phone block, not a runtime condition.
func WithBirthday1900Sentinel(month time.Month, day int) ContactOption {
	b := time.Date(sentinelBirthYear, month, day, 0, 0, 0, 0, time.UTC)
	if b.Month() != month || b.Day() != day {
		panic(fmt.Sprintf(
			"synthetic: WithBirthday1900Sentinel(%s, %d) normalizes to %s in the year-unknown sentinel year %d — "+
				"use WithBirthday on LeapSafeBirthYear(anchor) instead",
			month, day, b.Format("2006-01-02"), sentinelBirthYear))
	}
	return func(c *contactConfig) {
		bday := b
		c.birthday = &bday
	}
}

// sentinelBirthYear is the product's year-unknown birthday sentinel (see
// PLACEHOLDER_BIRTHDAY_YEAR in the frontend, which suppresses the rendered age
// for it).
const sentinelBirthYear = 1900

// LeapSafeBirthYear is the birth year a generated birthday should use when the
// month/day must survive verbatim: the largest leap year on or before
// anchor.Year()-30. Leap-safety is what preserves a February 29 target as
// February 29 (a Feb-28 clamp would move a "today" fixture on a Feb-29 anchor
// into the already-celebrated group), and the ~30-33 year offset keeps the
// rendered age plausible.
//
// It lives in factory rather than beside the birthday fixtures so both the
// synthetic root and the declare vocabulary can use it without a cycle.
func LeapSafeBirthYear(anchor time.Time) int {
	y := anchor.UTC().Year() - 30
	for !isLeapYear(y) {
		y--
	}
	return y
}

func isLeapYear(y int) bool {
	return y%4 == 0 && (y%100 != 0 || y%400 == 0)
}

// WithBirthday sets a general (real-year) birthday. The caller supplies an
// absolute date (anchor-relative so it tracks the configured time); this is the
// non-sentinel counterpart to WithBirthday1900Sentinel. It routes through the
// contact-create authority flip into a `birthday` date assertion.
func WithBirthday(t time.Time) ContactOption {
	return func(c *contactConfig) {
		b := t
		c.birthday = &b
	}
}

// WithHowMet sets the contact's how_met text. It routes through the
// contact-create authority flip into an accepted `how_met` text assertion (the
// ContactSpec.HowMet field the harness forwards to CreateContact). A blank
// string is normalized away by the service, so callers pass a meaningful value.
func WithHowMet(s string) ContactOption {
	return func(c *contactConfig) {
		v := s
		c.howMet = &v
	}
}

// WithLocation sets the contact's location (a flat place label). It routes
// through the contact-create authority flip into an accepted `lives_in` edge to a
// place entity node (EnsurePlaceTx find-or-creates the flat place node from the
// label — no synonym/hierarchy resolution, so no `within` edge). The label must
// be namespace-prefixed so the entity teardown's label-prefix sweep catches the
// auto-created place node. A blank string is normalized away by the service, so
// callers pass a meaningful value.
func WithLocation(s string) ContactOption {
	return func(c *contactConfig) {
		v := s
		c.location = &v
	}
}

// NameEdge names a display-name edge case: a token injected between the given
// name and the surname so the rendered name exercises a specific truncation,
// shaping or bidirectional-rendering hazard. The zero value is "no edge".
type NameEdge string

const (
	// NameEdgeUnicode is a diacritic-bearing name (accent composition).
	NameEdgeUnicode NameEdge = "unicode"
	// NameEdgeDescender is a name with descenders (g, y, j, p, q) — the
	// line-height clipping hazard.
	NameEdgeDescender NameEdge = "descender"
	// NameEdgeLong is a deliberately long name: the truncation/ellipsis and
	// layout-overflow hazard. Bounded so the rendered full_name stays inside the
	// 255-character limit the contact API's own validator enforces — a longer
	// name is a state the product itself refuses, so seeding one would break the
	// toolkit's honesty rule rather than test anything.
	NameEdgeLong NameEdge = "long"
	// NameEdgeRTL is a right-to-left (Arabic) segment: the bidirectional
	// rendering hazard, where a mixed-direction name reorders visually.
	NameEdgeRTL NameEdge = "rtl"
	// NameEdgeEmoji is an emoji-bearing name: astral-plane code points, where a
	// naive byte or UTF-16 truncation splits a surrogate pair.
	NameEdgeEmoji NameEdge = "emoji"
)

// nameEdgeLongRepeat is the repeat count behind the long token. It is stated as
// arithmetic rather than as a literal so the 255-character bound is checkable:
// worst case is prefix (6 + 60-char namespace + 1) + given (7) + space (1) +
// token + surname (11) + the disambiguation suffix, which the name-edge unit
// test pins.
const nameEdgeLongRepeat = 9

// nameEdgeTokens is the injected token per edge kind. Each ends in "-" so it
// reads as a name segment rather than a separate word.
var nameEdgeTokens = map[NameEdge]string{
	NameEdgeUnicode:   "Ünïcödé-",
	NameEdgeDescender: "Gregory-", // descenders: g, y, p, q
	NameEdgeLong:      strings.Repeat("Verylongname", nameEdgeLongRepeat) + "-",
	NameEdgeRTL:       "مرحبا-",
	NameEdgeEmoji:     "🎉🚀-",
}

// NameEdgeKinds is every edge kind, sorted, so a test can iterate the set
// instead of restating it.
func NameEdgeKinds() []NameEdge {
	out := make([]NameEdge, 0, len(nameEdgeTokens))
	for kind := range nameEdgeTokens {
		out = append(out, kind)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// NameEdgeToken is the token WithNameEdge injects for a kind, and false for an
// unknown one.
func NameEdgeToken(kind NameEdge) (string, bool) {
	token, ok := nameEdgeTokens[kind]
	return token, ok
}

// WithNameEdge uses a display name carrying the named edge case's token. It
// PANICS on an unknown kind — a typo would otherwise produce an ordinary name
// and the edge would silently stop being tested. The token stays inside the
// display-name dedupe, so two contacts with the same edge still get distinct
// names.
func WithNameEdge(kind NameEdge) ContactOption {
	if _, ok := nameEdgeTokens[kind]; !ok {
		panic(fmt.Sprintf("synthetic: unknown name edge %q", kind))
	}
	return func(c *contactConfig) { c.nameEdge = kind }
}

// WithUnicodeName uses a unicode-bearing display name (edge case).
func WithUnicodeName() ContactOption { return WithNameEdge(NameEdgeUnicode) }

// WithDescenderName uses a name with descenders (g, y, j, p, q) — a UI
// rendering edge case.
func WithDescenderName() ContactOption { return WithNameEdge(NameEdgeDescender) }

// WithNameTwinOf gives this contact the SAME full_name as an already-generated
// spec — the deliberate, narrow opt-out of the display-name dedupe below.
//
// The dedupe exists because names are drawn with replacement, so an accidental
// repeat is a flake that breaks every by-name selector. A DELIBERATE duplicate
// is a different thing: it is the fuzzy-collision fixture (two people who really
// do share a name), and it is the only shape that exercises what the matcher
// does with an ambiguous display-name tie. This option is the single route to
// one, and it is deliberately explicit — it needs the source spec in hand, so it
// cannot happen by accident.
//
// It preserves the PRNG stream exactly: the draw still consumes its sequence
// number, given name and surname, and only the rendered result is overridden
// afterwards, so every LATER contact in the namespace is byte-identical to what
// it would have been. The copied name is still recorded in the dedupe set, so a
// later ordinary draw that lands on the same base is disambiguated as usual.
func WithNameTwinOf(other ContactSpec) ContactOption {
	return func(c *contactConfig) {
		spec := other
		c.twinOf = &spec
	}
}

// WithNameMarker appends a resolution marker token to the contact's display name,
// so a hand-authored fixture can be resolved over the API by search instead of by
// an ad-hoc predicate over whatever the population happens to contain. The marker
// is a caller concern (its convention and its constraints are documented where the
// fixtures are seeded); the factory only places it. Composes with the name edge
// cases (unicode / descender) rather than replacing them, and draws no PRNG — a
// marker must never shift the shared generator stream.
func WithNameMarker(marker string) ContactOption {
	return func(c *contactConfig) { c.nameMarker = marker }
}

// Contact builds a deterministic ContactSpec. With no options it defaults to a
// single email method (the common case). All identifiers are namespace-prefixed.
func (g *Generator) Contact(opts ...ContactOption) ContactSpec {
	cfg := &contactConfig{}
	for _, o := range opts {
		o(cfg)
	}
	// Default to an email method when no method option was supplied — unless the
	// caller explicitly asked for a no-methods contact.
	if !cfg.noMethods && !cfg.withEmail && !cfg.withPhone && !cfg.withTelegram {
		cfg.withEmail = true
	}

	g.contactSeq++
	n := g.contactSeq
	given := g.givenName()
	sur := g.surname()

	display := given + " " + sur
	if token, ok := nameEdgeTokens[cfg.nameEdge]; ok {
		display = given + " " + token + sur
	}
	if cfg.nameMarker != "" {
		display += " " + cfg.nameMarker
	}
	// Names are drawn WITH REPLACEMENT from a 16×10 pool, so one namespace can
	// mint the same display name twice — measured at ~1.8% for a three-contact
	// fixture, which is a flake, not a rarity. A duplicate breaks any selector
	// that resolves a contact BY NAME (Playwright's strict mode fails outright
	// on two matching headings) and makes a manifest ambiguous to read. The
	// repeat therefore carries this contact's sequence number, which is unique
	// within the generator by construction.
	//
	// Disambiguating here rather than redrawing is deliberate: a redraw would
	// consume extra rng values and shift every later draw for that namespace,
	// so worlds that DON'T collide would still have to be re-derived. This way
	// a non-colliding namespace's output is byte-identical to before.
	if cfg.twinOf == nil && g.usedDisplay[display] {
		display = fmt.Sprintf("%s %d", display, n)
	}
	// Namespace-prefixed full_name so the prefix cleanup backstop finds it.
	fullName := g.Prefix() + display
	if cfg.twinOf != nil {
		// The deliberate duplicate (WithNameTwinOf): take the source's rendered
		// name verbatim, AFTER the draws above, so the stream position for every
		// later contact is unchanged.
		if cfg.twinOf.provenance != g.provenance {
			panic(fmt.Sprintf(
				"synthetic: WithNameTwinOf source %q is outside generator provenance for namespace %q",
				cfg.twinOf.FullName, g.Namespace()))
		}
		fullName = cfg.twinOf.FullName
		display = strings.TrimPrefix(fullName, g.Prefix())
	}
	g.usedDisplay[display] = true

	spec := ContactSpec{
		FullName:   fullName,
		Cadence:    cfg.cadence,
		Birthday:   cfg.birthday,
		HowMet:     cfg.howMet,
		Location:   cfg.location,
		provenance: g.provenance,
	}

	if cfg.withEmail {
		email := g.emailFor(given, sur, n)
		spec.Email = email
		spec.Methods = append(spec.Methods, MethodSpec{Type: "email", Value: email, IsPrimary: len(spec.Methods) == 0})
	}
	if cfg.withPhone {
		phone := g.phoneFor()
		spec.Phone = phone
		spec.Methods = append(spec.Methods, MethodSpec{Type: "phone", Value: phone, IsPrimary: len(spec.Methods) == 0})
	}
	if cfg.withTelegram {
		handle := g.telegramHandle(n)
		spec.TelegramHandle = handle
		spec.Methods = append(spec.Methods, MethodSpec{Type: "telegram", Value: handle, IsPrimary: len(spec.Methods) == 0})
	}

	// Backdated cohorts stamp created_at from ONE past instant, mirroring the create
	// handler — which leaves last_contacted unset (CON-001). Only a replayed
	// inbound/mutual interaction moves last_contacted, so a generated contact with an
	// empty timeline correctly reads as never-connected rather than manufacturing a
	// connection that never happened.
	switch {
	case cfg.createdAge != nil:
		t := g.at(-*cfg.createdAge)
		spec.CreatedAt = &t
	case cfg.recentCreationWindow != nil:
		t := g.at(g.recentOffset(*cfg.recentCreationWindow))
		spec.CreatedAt = &t
	}

	return spec
}

// NoteSpec is a synthetic note description.
type NoteSpec struct {
	Body string
}

// Note builds a deterministic note body (namespace-tagged so it is identifiable).
func (g *Generator) Note() NoteSpec {
	g.sourceIDSeq++
	return NoteSpec{Body: g.Prefix() + "note body " + g.givenName()}
}

// NodeSpec is a synthetic graph node description. The caller supplies the
// id when persisting (for persons, id == contact.id); the spec carries only the
// descriptive fields. CanonicalLabel is namespace-prefixed so the prefix-keyed
// cleanup backstop (DeleteNodesByLabelPrefix) finds it.
type NodeSpec struct {
	Type           string // person | venue | entity
	CanonicalLabel string
}

// Node builds a deterministic NodeSpec of the given type with a
// namespace-prefixed canonical_label. The per-run sourceIDSeq is embedded so
// repeated calls within one run produce DISTINCT labels (even if the name PRNG
// happens to repeat a component).
func (g *Generator) Node(nodeType string) NodeSpec {
	g.sourceIDSeq++
	return NodeSpec{
		Type:           nodeType,
		CanonicalLabel: fmt.Sprintf("%s%s-%s-%d", g.Prefix(), nodeType, g.surname(), g.sourceIDSeq),
	}
}

// EntitySpec is a synthetic entity subtype description. Subtype is an
// entity_type key (e.g. "place", "tag"); NormalizedName is namespace-prefixed so
// it is unique per namespace under the (subtype, normalized_name) unique. The
// paired NodeSpec (always type='entity') carries the display label.
type EntitySpec struct {
	Node           NodeSpec
	Subtype        string
	NormalizedName string
}

// Entity builds a deterministic EntitySpec for the given subtype, with a
// namespace-prefixed normalized_name and a paired entity-type node. The per-run
// sourceIDSeq is embedded so the (subtype, normalized_name) value is unique
// within one run.
func (g *Generator) Entity(subtype string) EntitySpec {
	g.sourceIDSeq++
	name := fmt.Sprintf("%s%s-%s-%d", g.Prefix(), subtype, g.givenName(), g.sourceIDSeq)
	return EntitySpec{
		Node:           NodeSpec{Type: "entity", CanonicalLabel: name},
		Subtype:        subtype,
		NormalizedName: name,
	}
}

// VenueSpec is a synthetic venue subtype description. SourceContainerID is
// namespace-prefixed so the (source, kind, source_container_id) unique is unique
// per namespace. The paired NodeSpec (always type='venue') carries the title as
// its canonical_label.
type VenueSpec struct {
	Node              NodeSpec
	Kind              string
	Source            string
	SourceContainerID string
	Title             string
}

// Venue builds a deterministic VenueSpec for the given source/kind, with a
// namespace-prefixed source_container_id and a paired venue node. The per-run
// sourceIDSeq is embedded so the (source, kind, source_container_id) value is
// unique within one run.
func (g *Generator) Venue(source, kind string) VenueSpec {
	g.sourceIDSeq++
	title := fmt.Sprintf("%s%s-%s-%d", g.Prefix(), kind, g.surname(), g.sourceIDSeq)
	return VenueSpec{
		Node:              NodeSpec{Type: "venue", CanonicalLabel: title},
		Kind:              kind,
		Source:            source,
		SourceContainerID: fmt.Sprintf("%scontainer-%d", g.Prefix(), g.sourceIDSeq),
		Title:             title,
	}
}

// AssertionSpec is a synthetic fact/edge-assertion description. The caller
// supplies the SubjectNodeID and PredicateKey when persisting (the factory does
// not know the subject's id or which catalog predicate to use). ValueText and
// PropositionKey are namespace-prefixed so a test scopes its reads to its own
// namespace on the shared DB; Confidence/Salience are sensible defaults the test
// may override.
//
// Exactly one value carrier is set per the predicate's kind (ReplayAssertion
// routes whichever is non-nil onto AssertRequest): ObjectNodeID for an edge
// predicate (person→person, etc.); ValueBool / ValueDate for a bool / date fact;
// otherwise ValueText for a text fact. A non-nil ObjectNodeID takes precedence
// (edges carry no scalar), then ValueBool, then ValueDate, then ValueText.
type AssertionSpec struct {
	PredicateKey   string
	ValueText      string
	ValueBool      *bool
	ValueDate      *time.Time
	ObjectNodeID   *uuid.UUID
	Confidence     int16
	Salience       int16
	PropositionKey string
}

// FactAssertion builds a deterministic fact AssertionSpec for the given
// predicate, with a namespace-prefixed value_text + proposition_key. The per-run
// sourceIDSeq is embedded so repeated calls within one run produce DISTINCT
// proposition keys (so each lands its own live-proposition slot).
func (g *Generator) FactAssertion(predicateKey string) AssertionSpec {
	g.sourceIDSeq++
	value := fmt.Sprintf("%s%s-%s-%d", g.Prefix(), predicateKey, g.givenName(), g.sourceIDSeq)
	return AssertionSpec{
		PredicateKey:   predicateKey,
		ValueText:      value,
		Confidence:     80,
		Salience:       50,
		PropositionKey: fmt.Sprintf("%sprop-%s-%d", g.Prefix(), predicateKey, g.sourceIDSeq),
	}
}

// valueCarrierSpec is the shared base for the value-carrier AssertionSpecs (bool
// / date facts + edges). They have NO text value, so unlike FactAssertion they
// draw NO name PRNG — only the monotonic sourceIDSeq for a distinct proposition_key.
// That makes them strictly ordering-safe to append after the name-drawing
// generators (they cannot shift the name/handle stream), but they still bump
// sourceIDSeq, so callers seed them LAST per the profile ordering rule.
func (g *Generator) valueCarrierSpec(predicateKey string) AssertionSpec {
	g.sourceIDSeq++
	return AssertionSpec{
		PredicateKey:   predicateKey,
		Confidence:     80,
		Salience:       50,
		PropositionKey: fmt.Sprintf("%sprop-%s-%d", g.Prefix(), predicateKey, g.sourceIDSeq),
	}
}

// BoolFact builds a deterministic bool-fact AssertionSpec (e.g. job_seeking /
// on_sabbatical / traveling) carrying ValueBool. Draws no name PRNG.
func (g *Generator) BoolFact(predicateKey string, value bool) AssertionSpec {
	spec := g.valueCarrierSpec(predicateKey)
	v := value
	spec.ValueBool = &v
	return spec
}

// DateFact builds a deterministic date-fact AssertionSpec (e.g. birthday)
// carrying ValueDate, asserted DIRECTLY through the assert write path (distinct
// from the contact-create authority-flip birthday path). Draws no name PRNG.
func (g *Generator) DateFact(predicateKey string, value time.Time) AssertionSpec {
	spec := g.valueCarrierSpec(predicateKey)
	d := value
	spec.ValueDate = &d
	return spec
}

// EdgeAssertion builds a deterministic edge AssertionSpec for the given edge
// predicate (e.g. knows / introduced_by / sibling_of) pointing at an
// already-seeded object node. Carries ObjectNodeID and NO scalar value (the
// assert validator rejects an edge with any scalar). Draws no name PRNG.
func (g *Generator) EdgeAssertion(predicateKey string, objectNodeID uuid.UUID) AssertionSpec {
	spec := g.valueCarrierSpec(predicateKey)
	id := objectNodeID
	spec.ObjectNodeID = &id
	return spec
}
