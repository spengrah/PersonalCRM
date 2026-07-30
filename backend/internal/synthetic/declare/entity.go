package declare

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"personal-crm/backend/internal/synthetic/factory"
)

// Entity is one thing a declaration creates. PR1 shipped Contact; the
// adversarial catalog adds ExternalCandidate / Note / Merge / SoftDelete, and
// later domain PRs add CalendarEvent / MacHost / MeetingNote / Task as their
// domains migrate.
type Entity interface {
	// handle is the declaration-local name the manifest keys the created row by.
	handle() string
	// kind is the manifest's Seeded.Kind value ("contact", ...).
	kind() string
	// validate is called at Register time; a returned error becomes a panic.
	validate() error
	// refs are the handles this entity requires to have ALREADY been created,
	// EARLIER in the same entity list. They are resolved against the run's own
	// manifest, so a forward or self reference has nothing to resolve against —
	// which is why it is a registration-time error rather than a runtime nil.
	refs() []string
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

// Name edges a Contact may declare. They name the RENDERING hazard, not the
// glyphs, and lower onto the factory's own edge tokens.
const (
	NameEdgeLong  = string(factory.NameEdgeLong)
	NameEdgeRTL   = string(factory.NameEdgeRTL)
	NameEdgeEmoji = string(factory.NameEdgeEmoji)
)

// Candidate sources a declaration may name. Stated LOCALLY, like the cadence
// table, and deliberately restricted to the two shapes
// Harness.SeedExternalContactCandidate can actually key: an id-keyed address
// book and an email-keyed correspondence discoverer. A source outside this set
// would either produce a row the sync path cannot produce or silently fall into
// the address-book branch.
const (
	SourceGContacts      = "gcontacts"
	SourceCorrespondence = "gmail_correspondence"
)

var candidateSources = map[string]bool{
	SourceGContacts:      true,
	SourceCorrespondence: true,
}

// --- prop plumbing ----------------------------------------------------------
//
// SameNameAs is valid on BOTH a Contact and an ExternalCandidate (a collision
// needs the two twins AND the candidate that collides with them), so the two
// entity constructors take an INTERFACE rather than their own func type. The
// existing func-typed props satisfy it through a method on the named func type,
// so every prop constructor below keeps its original shape.

// ContactProp customizes a declared contact.
type ContactProp func(*contactPlan)

func (f ContactProp) applyContact(p *contactPlan) { f(p) }

// CandidateProp customizes a declared import candidate.
type CandidateProp func(*externalCandidatePlan)

func (f CandidateProp) applyCandidate(p *externalCandidatePlan) { f(p) }

// ContactPropSource is anything Contact accepts.
type ContactPropSource interface{ applyContact(*contactPlan) }

// CandidatePropSource is anything ExternalCandidate accepts.
type CandidatePropSource interface{ applyCandidate(*externalCandidatePlan) }

// TwinProp is a prop valid on either entity kind.
type TwinProp interface {
	ContactPropSource
	CandidatePropSource
}

// --- contacts ---------------------------------------------------------------

// birthdayPlan is a declared birthday: either an offset in days from the run
// anchor, or an explicit month/day. Both resolve at run time on a leap-safe
// birth year, so February 29 is representable rather than silently normalized.
//
// The one exception is the PLACEHOLDER form, which deliberately stores the
// product's own year-unknown sentinel year instead — see
// BirthdayPlaceholderToday.
type birthdayPlan struct {
	inDays      *int
	month       time.Month
	day         int
	placeholder bool
}

// contactPlan is the lowered form of a Contact declaration.
type contactPlan struct {
	name            string
	cadence         string
	overdueBy       *Amount
	neverContacted  bool
	createdAgo      *Amount
	methods         []string
	noMethods       bool
	history         *int
	nameEdge        string
	nameMarker      *string
	explicitNameSet bool
	explicitGiven   string
	explicitSurname string
	sameNameAs      string
	birthday        *birthdayPlan
	location        *string
}

func (p *contactPlan) handle() string { return p.name }
func (p *contactPlan) kind() string   { return "contact" }
func (p *contactPlan) refs() []string {
	if p.sameNameAs == "" {
		return nil
	}
	return []string{p.sameNameAs}
}

// Contact declares one contact under a declaration-local handle. The handle is
// how a test reads the created row out of the manifest (`entities["card-a"]`)
// and is never rendered — the UI name is generator-derived and comes back in
// the manifest.
func Contact(handle string, props ...ContactPropSource) Entity {
	p := &contactPlan{name: handle}
	for _, prop := range props {
		prop.applyContact(p)
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

// History declares n inbound emails spread deterministically across the recent
// past, driven through the BATCH replay adapter (one settle per dependency
// generation rather than n full settles).
//
// It lowers to replayed history for the same reason OverdueBy does — only a real
// interaction may move last_contacted — so it likewise requires an addressable
// email, and it DERIVES its own creation backdate from the oldest message it
// replays: a contact must exist before the connection it carries. That makes it
// mutually exclusive with CreatedAgo and OverdueBy (two derivations of one
// field) and with NeverContacted (a contradiction in terms).
//
// Each generated Gmail message has a distinct thread id, and email interaction
// identity includes that thread id. History therefore produces n interaction
// rows even when the compressed CRM_ENV=testing table places adjacent messages
// inside the manual interaction window; that window applies only when no
// source_ref exists.
func History(n int) ContactProp {
	return func(p *contactPlan) {
		count := n
		p.history = &count
	}
}

// NameEdge declares a display name carrying a rendering-hazard token
// (NameEdgeLong / NameEdgeRTL / NameEdgeEmoji). The rendered name stays inside
// the 255-character bound the contact API's own validator enforces.
func NameEdge(kind string) ContactProp {
	return func(p *contactPlan) { p.nameEdge = kind }
}

// BirthdayInDays declares a birthday whose next occurrence is n days from the
// run anchor (0 is "today"), on a leap-safe birth year.
func BirthdayInDays(n int) ContactProp {
	return func(p *contactPlan) {
		days := n
		p.birthday = &birthdayPlan{inDays: &days}
	}
}

// BirthdayOn declares an explicit month/day birthday on a leap-safe birth year,
// so BirthdayOn(time.February, 29) is well defined for every anchor.
func BirthdayOn(month time.Month, day int) ContactProp {
	return func(p *contactPlan) { p.birthday = &birthdayPlan{month: month, day: day} }
}

// BirthdayPlaceholderToday declares a birthday on TODAY's month/day stored
// against the product's year-unknown PLACEHOLDER year (1900), which is what the
// UI keys its age suppression on.
//
// It is the only prop that reaches that state: BirthdayInDays / BirthdayOn both
// resolve a REAL leap-safe birth year, which the frontend correctly reads as a
// known birth year and renders an age for. A fixture for "the birthday is known
// but the year is not" therefore cannot be expressed through them.
//
// One calendar day is not representable: 1900 is not a leap year, so a
// placeholder-year February 29 does not exist at all. Rather than hand the
// sentinel builder a date it must panic on — this runs on every composed-world
// seed, so a panic would break SEEDING once every four years — the lowering
// clamps that one day to February 28. The clamp keeps seeding safe; it does not
// make the app classify the clamped contact as having a birthday TODAY on that
// day, which is a gap in the product's own storage convention rather than
// something a fixture can paper over.
func BirthdayPlaceholderToday() ContactProp {
	return func(p *contactPlan) {
		days := 0
		p.birthday = &birthdayPlan{inDays: &days, placeholder: true}
	}
}

// Location declares the contact's location (a flat place label).
//
// The stored value is namespace-PREFIXED, like every other generated identifier,
// because the auto-created place node's label has to carry the prefix for the
// entity teardown's label-prefix sweep to find it. A test that needs the rendered
// string therefore reads it back from the API rather than restating the literal
// it asked for.
func Location(s string) ContactProp {
	return func(p *contactPlan) {
		v := s
		p.location = &v
	}
}

// ExplicitName pins the contact's rendered name to a caller-supplied literal.
//
// It exists for fixtures whose citing test depends on knowing the rendered
// name's relative ORDER before the data is seeded: a list that must come back in
// a known name-ascending order, or a name set deliberately anti-correlated with
// the property under test so an implementation that fell back to name ordering
// cannot accidentally pass. Deriving that order in the browser from
// generator-drawn names would compare JavaScript collation against PostgreSQL's,
// which is a different ordering.
//
// It skips the factory's display-name dedupe (an exact literal is the point), so
// no two entities may declare the same pair. Registration checks that within one
// entity list; the composed world — where every declaration and edge runs against
// ONE namespace — is checked across lists by its own guard, because registration
// order makes a cross-list check at init time impossible.
//
// It is mutually exclusive with SameNameAs and NameEdge: a twin copies another
// contact's rendered name, and an edge splices a token into the pair, so either
// one would render something other than the literal that was pinned.
func ExplicitName(given, surname string) ContactProp {
	return func(p *contactPlan) {
		p.explicitNameSet = true
		p.explicitGiven, p.explicitSurname = given, surname
	}
}

// NameMarker appends a caller-chosen token to the contact's rendered name, so a
// SET of declared contacts can be resolved over the API by search instead of by
// an ad-hoc predicate. It composes with everything else and draws no PRNG.
func NameMarker(marker string) ContactProp {
	return func(p *contactPlan) {
		m := marker
		p.nameMarker = &m
	}
}

// sameNameProp is the twin-name prop. It implements BOTH entity kinds' prop
// interfaces because a name collision needs a contact twin AND an import
// candidate that collides with the pair.
type sameNameProp struct{ handle string }

func (s sameNameProp) applyContact(p *contactPlan)             { p.sameNameAs = s.handle }
func (s sameNameProp) applyCandidate(p *externalCandidatePlan) { p.sameNameAs = s.handle }

// SameNameAs gives this entity the SAME rendered name as an earlier CONTACT
// handle in the same entity list.
//
// On a Contact it is the single, deliberate opt-out of the factory's
// display-name dedupe — the fixture for "two people really do share a name",
// which is the only shape that exercises what the import matcher does with an
// ambiguous name tie. On an ExternalCandidate it overrides the generated display
// name, which is what turns two same-named rows into an actual matching
// collision rather than a cosmetic one.
func SameNameAs(handle string) TwinProp { return sameNameProp{handle: handle} }

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
	if p.history != nil {
		if *p.history < 1 {
			return fmt.Errorf("contact %q: History(%d) must be at least 1", p.name, *p.history)
		}
		if p.overdueBy != nil {
			return fmt.Errorf("contact %q: History and OverdueBy are mutually exclusive — both DERIVE the creation age from the history they replay", p.name)
		}
		if p.createdAgo != nil {
			return fmt.Errorf("contact %q: History and CreatedAgo are mutually exclusive — History derives the creation age from its oldest replayed message", p.name)
		}
		if p.neverContacted {
			return fmt.Errorf("contact %q: History and NeverContacted are mutually exclusive", p.name)
		}
		if !p.hasEmail() {
			return fmt.Errorf("contact %q: History lowers to replayed inbound EMAIL, so the contact must carry an email method", p.name)
		}
		if err := historySpanWithinBatchReach(*p.history); err != nil {
			return fmt.Errorf("contact %q: %w", p.name, err)
		}
	}
	if p.nameEdge != "" {
		if _, ok := factory.NameEdgeToken(factory.NameEdge(p.nameEdge)); !ok {
			return fmt.Errorf("contact %q: unknown name edge %q (valid: %s)",
				p.name, p.nameEdge, strings.Join(nameEdgeVocabulary(), ", "))
		}
		if p.sameNameAs != "" {
			return fmt.Errorf("contact %q: NameEdge and SameNameAs are mutually exclusive — a twin copies the source's rendered name, edge token included", p.name)
		}
	}
	if p.sameNameAs == p.name && p.sameNameAs != "" {
		return fmt.Errorf("contact %q: SameNameAs cannot reference itself", p.name)
	}
	if p.location != nil && strings.TrimSpace(*p.location) == "" {
		return fmt.Errorf("contact %q: Location must be non-empty — the service normalizes a blank location away, silently contradicting a non-nil Location postcondition", p.name)
	}
	if p.explicitNameSet {
		if strings.TrimSpace(p.explicitGiven) == "" || strings.TrimSpace(p.explicitSurname) == "" {
			return fmt.Errorf("contact %q: ExplicitName needs BOTH a given name and a surname — a half-pinned name is not the literal the caller asked for", p.name)
		}
		if p.sameNameAs != "" {
			return fmt.Errorf("contact %q: ExplicitName and SameNameAs are mutually exclusive — both state what the rendered name is", p.name)
		}
		if p.nameEdge != "" {
			return fmt.Errorf("contact %q: ExplicitName and NameEdge are mutually exclusive — a name edge splices its token between the given name and the surname, so the rendered name would not be the literal that was pinned", p.name)
		}
	}
	if p.nameMarker != nil && strings.TrimSpace(*p.nameMarker) == "" {
		return fmt.Errorf("contact %q: NameMarker must be a non-blank token — a blank marker resolves nothing", p.name)
	}
	if p.birthday != nil {
		if err := p.birthday.validate(p.name); err != nil {
			return err
		}
	}
	return nil
}

func (b *birthdayPlan) validate(handle string) error {
	if b.placeholder {
		// A placeholder birthday derives its month/day from the run anchor through
		// the clamp, which is a valid calendar date by construction; the zero-valued
		// month/day struct fields below are never read for it.
		return nil
	}
	if b.inDays != nil {
		return nil
	}
	if b.month < time.January || b.month > time.December {
		return fmt.Errorf("contact %q: BirthdayOn month %d is out of range", handle, b.month)
	}
	// Day-of-month is checked against the LEAP maximum, so February 29 is
	// accepted (it is representable on a leap-safe birth year) while February 30
	// is not.
	if b.day < 1 || b.day > leapDaysInMonth(b.month) {
		return fmt.Errorf("contact %q: BirthdayOn(%s, %d) is not a real date", handle, b.month, b.day)
	}
	return nil
}

func leapDaysInMonth(m time.Month) int {
	// A leap year, so February is 29.
	return time.Date(2024, m+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

func nameEdgeVocabulary() []string {
	kinds := factory.NameEdgeKinds()
	out := make([]string, 0, len(kinds))
	for _, k := range kinds {
		out = append(out, string(k))
	}
	return out
}

// --- import candidates ------------------------------------------------------

// externalCandidatePlan is the lowered form of an ExternalCandidate declaration.
type externalCandidatePlan struct {
	name       string
	source     string
	sameNameAs string
}

func (p *externalCandidatePlan) handle() string { return p.name }
func (p *externalCandidatePlan) kind() string   { return "external_contact" }
func (p *externalCandidatePlan) refs() []string {
	if p.sameNameAs == "" {
		return nil
	}
	return []string{p.sameNameAs}
}

// ExternalCandidate declares one UNMATCHED import-queue candidate.
//
// match_status is deliberately NOT declarable: the write path hardcodes
// 'unmatched' on insert, so a matched or linked candidate would be a row the
// sync path cannot produce.
func ExternalCandidate(handle string, props ...CandidatePropSource) Entity {
	p := &externalCandidatePlan{name: handle}
	for _, prop := range props {
		prop.applyCandidate(p)
	}
	return p
}

// Source names the import candidate's source.
func Source(name string) CandidateProp {
	return func(p *externalCandidatePlan) { p.source = name }
}

func (p *externalCandidatePlan) validate() error {
	if strings.TrimSpace(p.name) == "" {
		return fmt.Errorf("external candidate handle must be non-empty")
	}
	if !candidateSources[p.source] {
		return fmt.Errorf("external candidate %q: unknown source %q (valid: %s)",
			p.name, p.source, strings.Join(sortedCandidateSources(), ", "))
	}
	return nil
}

// --- notes ------------------------------------------------------------------

// notePlan is the lowered form of a Note declaration.
type notePlan struct {
	name    string
	contact string
}

func (p *notePlan) handle() string { return p.name }
func (p *notePlan) kind() string   { return "note" }
func (p *notePlan) refs() []string { return []string{p.contact} }
func (p *notePlan) validate() error {
	if strings.TrimSpace(p.name) == "" {
		return fmt.Errorf("note handle must be non-empty")
	}
	if strings.TrimSpace(p.contact) == "" {
		return fmt.Errorf("note %q: a contact handle is required", p.name)
	}
	return nil
}

// Note declares a notepad note on an EARLIER contact handle. The body is
// generator-derived, never a literal, so the world stays deterministic and
// PII-free by construction.
func Note(handle, contactHandle string) Entity {
	return &notePlan{name: handle, contact: contactHandle}
}

// --- merges -----------------------------------------------------------------

// mergePlan is the lowered form of a Merge declaration.
type mergePlan struct {
	loser  string
	winner string
}

func (p *mergePlan) handle() string { return "merge-" + p.loser + "-into-" + p.winner }
func (p *mergePlan) kind() string   { return "merge" }
func (p *mergePlan) refs() []string { return []string{p.loser, p.winner} }
func (p *mergePlan) validate() error {
	if strings.TrimSpace(p.loser) == "" || strings.TrimSpace(p.winner) == "" {
		return fmt.Errorf("merge: both a loser and a winner contact handle are required")
	}
	if p.loser == p.winner {
		return fmt.Errorf("merge %q: a contact cannot be merged into itself", p.handle())
	}
	return nil
}

// Merge merges an EARLIER contact handle (the loser, archived) into another
// earlier contact handle (the winner, kept), through the production merge path.
// Chaining two of them — a into b, then b into c — is what makes two-hop
// reparenting observable.
func Merge(loser, winner string) Entity {
	return &mergePlan{loser: loser, winner: winner}
}

// --- soft deletes -----------------------------------------------------------

// softDeletePlan is the lowered form of a SoftDelete declaration.
type softDeletePlan struct {
	target string
}

func (p *softDeletePlan) handle() string { return "soft-delete-" + p.target }
func (p *softDeletePlan) kind() string   { return "soft_delete" }
func (p *softDeletePlan) refs() []string { return []string{p.target} }
func (p *softDeletePlan) validate() error {
	if strings.TrimSpace(p.target) == "" {
		return fmt.Errorf("soft delete: a contact handle is required")
	}
	return nil
}

// SoftDelete tombstones an EARLIER contact handle through the production delete
// path. Declaring it AFTER the contact's children is what produces a
// soft-deleted parent with live children — a state a seed-and-delete primitive
// cannot reach.
func SoftDelete(contactHandle string) Entity {
	return &softDeletePlan{target: contactHandle}
}

// --- shared -----------------------------------------------------------------

func sortedMethodKinds() []string {
	out := make([]string, 0, len(methodKinds))
	for k := range methodKinds {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedCandidateSources() []string {
	out := make([]string, 0, len(candidateSources))
	for k := range candidateSources {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// validateEntityOrder checks that every entity's refs name a CONTACT declared
// EARLIER in the same list, and that no handle repeats. Callers (Register,
// RegisterEdge, RunDeclarationForTest) share it so the three cannot disagree
// about what a well-formed entity list is.
func validateEntityOrder(entities []Entity) error {
	seen := map[string]Entity{}
	// ExplicitName deliberately skips the factory's runtime dedupe, so two
	// entities pinning the SAME literal would render one ambiguous name. Caught
	// here, at registration, rather than as a confusing strict-mode locator
	// failure in whichever test happens to run first.
	explicitNames := map[string]string{}
	for i, e := range entities {
		if e == nil {
			return fmt.Errorf("entity %d is nil", i)
		}
		if err := e.validate(); err != nil {
			return err
		}
		if p, ok := e.(*contactPlan); ok && p.explicitNameSet {
			display := p.explicitGiven + " " + p.explicitSurname
			if prior, dup := explicitNames[display]; dup {
				return fmt.Errorf("entities %q and %q both declare the explicit name %q — ExplicitName skips the display-name dedupe, so the two would render as one name", prior, p.name, display)
			}
			explicitNames[display] = p.name
		}
		for _, ref := range e.refs() {
			target, ok := seen[ref]
			if !ok {
				return fmt.Errorf("entity %q references handle %q, which is not declared EARLIER in the same list", e.handle(), ref)
			}
			if target.kind() != "contact" {
				return fmt.Errorf("entity %q references handle %q, which is a %s — only a contact can be referenced", e.handle(), ref, target.kind())
			}
		}
		if _, dup := seen[e.handle()]; dup {
			return fmt.Errorf("duplicate entity handle %q", e.handle())
		}
		seen[e.handle()] = e
	}
	return nil
}
