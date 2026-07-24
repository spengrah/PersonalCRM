package factory

import (
	"fmt"
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
	FullName  string
	Methods   []MethodSpec
	Cadence   *string
	CreatedAt *time.Time
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
	unicodeName          bool
	descender            bool
}

// WithEmail adds an email contact_method (default ON if no method option is
// given — see Contact).
func WithEmail() ContactOption { return func(c *contactConfig) { c.withEmail = true } }

// WithPhone adds a phone contact_method.
func WithPhone() ContactOption { return func(c *contactConfig) { c.withPhone = true } }

// WithTelegram adds a telegram contact_method (handle).
func WithTelegram() ContactOption { return func(c *contactConfig) { c.withTelegram = true } }

// WithNoMethods builds a contact carrying NO contact_method (the "no methods"
// catalog bucket — the Imports/rematch surfaces + the empty-methods UI state).
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
// a representative edge case the catalog can build on.
func WithBirthday1900Sentinel(month time.Month, day int) ContactOption {
	return func(c *contactConfig) {
		b := time.Date(1900, month, day, 0, 0, 0, 0, time.UTC)
		c.birthday = &b
	}
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

// WithUnicodeName uses a unicode-bearing display name (edge case).
func WithUnicodeName() ContactOption { return func(c *contactConfig) { c.unicodeName = true } }

// WithDescenderName uses a name with descenders (g, y, j, p, q) — a UI
// rendering edge case.
func WithDescenderName() ContactOption { return func(c *contactConfig) { c.descender = true } }

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
	switch {
	case cfg.unicodeName:
		display = given + " Ünïcödé-" + sur
	case cfg.descender:
		display = given + " Gregory-" + sur // descenders: g, y, p, q
	}
	// Namespace-prefixed full_name so the prefix cleanup backstop finds it.
	fullName := g.Prefix() + display

	spec := ContactSpec{
		FullName: fullName,
		Cadence:  cfg.cadence,
		Birthday: cfg.birthday,
		HowMet:   cfg.howMet,
		Location: cfg.location,
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
