package factory

import (
	"time"
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
	FullName      string
	Methods       []MethodSpec
	Cadence       *string
	LastContacted *time.Time
	Birthday      *time.Time
	Location      *string
	HowMet        *string

	// Convenience accessors to the primary identifiers (also present in
	// Methods) so adapters can address this contact without re-deriving them.
	Email          string
	Phone          string
	TelegramHandle string
}

// ContactOption customizes a generated ContactSpec.
type ContactOption func(*contactConfig)

type contactConfig struct {
	withEmail    bool
	withPhone    bool
	withTelegram bool
	noMethods    bool
	cadence      *string
	overdue      bool
	recent       bool
	birthday     *time.Time
	unicodeName  bool
	descender    bool
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

// WithOverdue gives the contact a last_contacted far enough in the past that,
// combined with a cadence, it reads as overdue. Anchor-relative.
func WithOverdue() ContactOption { return func(c *contactConfig) { c.overdue = true } }

// WithRecent gives the contact a recent last_contacted (anchor-relative).
func WithRecent() ContactOption { return func(c *contactConfig) { c.recent = true } }

// WithBirthday1900Sentinel sets the prod 1900-MM-DD month/day-only sentinel —
// a representative edge case the catalog can build on.
func WithBirthday1900Sentinel(month time.Month, day int) ContactOption {
	return func(c *contactConfig) {
		b := time.Date(1900, month, day, 0, 0, 0, 0, time.UTC)
		c.birthday = &b
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

	switch {
	case cfg.overdue:
		// ~90 days ago — overdue for any sub-quarterly cadence.
		t := g.at(-90 * 24 * time.Hour)
		spec.LastContacted = &t
	case cfg.recent:
		t := g.at(g.recentOffset(48 * time.Hour))
		spec.LastContacted = &t
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
