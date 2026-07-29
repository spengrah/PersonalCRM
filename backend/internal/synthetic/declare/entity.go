package declare

import (
	"fmt"
	"sort"
	"strings"
)

// Entity is one thing a declaration creates. PR1 ships Contact; later domain
// PRs add ExternalCandidate / CalendarEvent / MacHost / MeetingNote / Note /
// Task as their domains migrate.
type Entity interface {
	// handle is the declaration-local name the manifest keys the created row by.
	handle() string
	// kind is the manifest's Seeded.Kind value ("contact", ...).
	kind() string
	// validate is called at Register time; a returned error becomes a panic.
	validate() error
}

// Method kinds a Contact may declare.
const (
	MethodEmail    = "email"
	MethodPhone    = "phone"
	MethodTelegram = "telegram"
)

var methodKinds = map[string]bool{
	MethodEmail:    true,
	MethodPhone:    true,
	MethodTelegram: true,
}

// contactPlan is the lowered form of a Contact declaration.
type contactPlan struct {
	name           string
	cadence        string
	overdueBy      *Amount
	neverContacted bool
	createdAgo     *Amount
	methods        []string
	noMethods      bool
}

func (p *contactPlan) handle() string { return p.name }
func (p *contactPlan) kind() string   { return "contact" }

// ContactProp customizes a declared contact.
type ContactProp func(*contactPlan)

// Contact declares one contact under a declaration-local handle. The handle is
// how a test reads the created row out of the manifest (`entities["card-a"]`)
// and is never rendered — the UI name is generator-derived and comes back in
// the manifest.
func Contact(handle string, props ...ContactProp) Entity {
	p := &contactPlan{name: handle}
	for _, prop := range props {
		prop(p)
	}
	return p
}

// Cadence sets the contact's cadence. The name is validated against the spec
// cadence vocabulary at Register time.
func Cadence(name string) ContactProp {
	return func(p *contactPlan) { p.cadence = name }
}

// OverdueBy declares the contact overdue by AT LEAST the stated amount at seed
// time (a floor — display-day rounding and calendar slack are the app's
// business). It requires Cadence and lowers to a REPLAYED inbound email aged
// period(cadence) + amount, never to a direct last_contacted write: creation
// never seeds last_contacted, and only a replayed inbound/mutual interaction
// moves it, so a fixture cannot manufacture a connection that never happened.
// That lowering needs an addressable email, so OverdueBy is incompatible with
// NoMethods and with a method set that omits email.
func OverdueBy(a Amount) ContactProp {
	return func(p *contactPlan) {
		amount := a
		p.overdueBy = &amount
	}
}

// NeverContacted declares a contact with NO interaction history, so
// last_contacted stays null. Combine with Cadence + CreatedAgo(> one period)
// for the honest "added long ago, never connected" overdue reading. Mutually
// exclusive with OverdueBy.
func NeverContacted() ContactProp {
	return func(p *contactPlan) { p.neverContacted = true }
}

// CreatedAgo backdates the contact's created_at by the stated amount.
func CreatedAgo(a Amount) ContactProp {
	return func(p *contactPlan) {
		amount := a
		p.createdAgo = &amount
	}
}

// Methods sets the contact's method kinds ("email" | "phone" | "telegram").
// Omitting it leaves the factory default (a single email).
func Methods(kinds ...string) ContactProp {
	return func(p *contactPlan) { p.methods = append([]string(nil), kinds...) }
}

// NoMethods declares a contact carrying no contact_method at all.
func NoMethods() ContactProp {
	return func(p *contactPlan) { p.noMethods = true }
}

// hasEmail reports whether the declared method set includes an email — either
// explicitly, or by the factory default when no method option was given.
func (p *contactPlan) hasEmail() bool {
	if p.noMethods {
		return false
	}
	if len(p.methods) == 0 {
		return true // factory default is a single email
	}
	for _, m := range p.methods {
		if m == MethodEmail {
			return true
		}
	}
	return false
}

func (p *contactPlan) validate() error {
	if strings.TrimSpace(p.name) == "" {
		return fmt.Errorf("contact handle must be non-empty")
	}
	if p.cadence != "" && !knownCadence(p.cadence) {
		return fmt.Errorf("contact %q: unknown cadence %q (vocabulary: %s)",
			p.name, p.cadence, strings.Join(Cadences(), ", "))
	}
	if p.noMethods && len(p.methods) > 0 {
		return fmt.Errorf("contact %q: NoMethods and Methods are mutually exclusive", p.name)
	}
	seen := map[string]bool{}
	for _, m := range p.methods {
		if !methodKinds[m] {
			return fmt.Errorf("contact %q: unknown method kind %q (valid: %s)",
				p.name, m, strings.Join(sortedMethodKinds(), ", "))
		}
		if seen[m] {
			return fmt.Errorf("contact %q: duplicate method kind %q", p.name, m)
		}
		seen[m] = true
	}
	for label, amount := range map[string]*Amount{"OverdueBy": p.overdueBy, "CreatedAgo": p.createdAgo} {
		if amount == nil {
			continue
		}
		if amount.negative() {
			return fmt.Errorf("contact %q: %s(%s) is negative — an amount dates a fixture BACKWARD", p.name, label, amount)
		}
		if amount.needsCadence() && p.cadence == "" {
			return fmt.Errorf("contact %q: %s(%s) is stated in periods but the contact declares no cadence", p.name, label, amount)
		}
	}
	if p.overdueBy != nil {
		if p.cadence == "" {
			return fmt.Errorf("contact %q: OverdueBy requires Cadence — overdue-ness is defined against a period", p.name)
		}
		if p.neverContacted {
			return fmt.Errorf("contact %q: OverdueBy and NeverContacted are mutually exclusive", p.name)
		}
		if p.createdAgo != nil {
			return fmt.Errorf("contact %q: OverdueBy and CreatedAgo are mutually exclusive — OverdueBy DERIVES the creation age from the history it replays (a contact must exist before the connection it carries, and the app's due date only ever moves forward)", p.name)
		}
		if !p.hasEmail() {
			return fmt.Errorf("contact %q: OverdueBy lowers to a replayed inbound EMAIL, so the contact must carry an email method", p.name)
		}
	}
	return nil
}

func sortedMethodKinds() []string {
	out := make([]string, 0, len(methodKinds))
	for k := range methodKinds {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
