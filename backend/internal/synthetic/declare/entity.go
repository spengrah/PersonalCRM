package declare

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"personal-crm/backend/internal/google"
	"personal-crm/backend/internal/mac"
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
	// MethodGChat is a Google Chat address. Chat addresses ARE emails, so the
	// generated value is email-shaped: the contact API validates a gchat value
	// with the email rule, and identity normalization delegates gchat to the
	// email normalizer.
	MethodGChat = "gchat"
)

var methodKinds = map[string]bool{
	MethodEmail:    true,
	MethodPhone:    true,
	MethodTelegram: true,
	MethodGChat:    true,
}

// Name edges a Contact may declare. They name the RENDERING hazard, not the
// glyphs, and lower onto the factory's own edge tokens.
const (
	NameEdgeDescender = string(factory.NameEdgeDescender)
	NameEdgeLong      = string(factory.NameEdgeLong)
	NameEdgeRTL       = string(factory.NameEdgeRTL)
	NameEdgeEmoji     = string(factory.NameEdgeEmoji)
)

// Candidate sources a declaration may name. Stated LOCALLY, like the cadence
// table. The set is bounded by what the lowering can write through the source's
// OWN production writer, because a candidate written any other way is a row the
// sync path cannot produce:
//
//	gcontacts, gmail_correspondence, gcal_attendee — ExternalContactRepository.Upsert,
//	    the direct write the Google providers use (google/contacts.go,
//	    google/gmail_correspondence.go, google/calendar.go). The ingest registry
//	    does not admit these sources at all.
//	icloud_contacts, anarlog_humans — the mac-daemon ingest pipeline, which is the
//	    ONLY writer for them (service.externalContactAllowedSources admits exactly
//	    these two and rejects every other source).
//	telegram — UpsertTelegramDiscoveryCandidate, the dedicated merge-preserving
//	    upsert PeerMatcher uses.
//	anarlog_title — anarlog.DiscoveryWriter.UpsertTitleCandidateTx.
//
// The last two key their source_id on a decimal peer id and a SHA-256 digest, so
// neither carries the namespace prefix cleanup's name-derived sweep looks for.
// That is why every declared candidate also records namespace ownership by row id.
const (
	SourceGContacts        = "gcontacts"
	SourceCorrespondence   = "gmail_correspondence"
	SourceCalendarAttendee = "gcal_attendee"
	SourceICloudContacts   = "icloud_contacts"
	SourceAnarlogHumans    = "anarlog_humans"
	SourceTelegram         = "telegram"
	SourceAnarlogTitle     = "anarlog_title"
)

var candidateSources = map[string]bool{
	SourceGContacts:        true,
	SourceCorrespondence:   true,
	SourceCalendarAttendee: true,
	SourceICloudContacts:   true,
	SourceAnarlogHumans:    true,
	SourceTelegram:         true,
	SourceAnarlogTitle:     true,
}

// directUpsertSources are the sources whose production writer is the plain
// ExternalContactRepository.Upsert, and therefore the only ones whose emails,
// phones and display name a declaration can choose. The ingest sources mint their
// own display name and email inside the factory payload, and the two dedicated
// writers store no methods at all.
var directUpsertSources = map[string]bool{
	SourceGContacts:        true,
	SourceCorrespondence:   true,
	SourceCalendarAttendee: true,
}

// methodBearingSources are the direct-upsert sources whose row carries a
// declarable EMAIL. gmail_correspondence is excluded: its single email IS its
// source_id, so choosing it independently would not be a row the discoverer can
// produce.
var methodBearingSources = map[string]bool{
	SourceGContacts:        true,
	SourceCalendarAttendee: true,
}

// multiMethodSources are the sources whose production writer emits MORE than one
// email, or any phone at all. Only the address-book provider does: it maps every
// address and number off the Person record (google/contacts.go
// convertPersonToRequest).
//
// gcal_attendee is deliberately absent even though it is method-bearing. The
// calendar provider stores exactly ONE email — the attendee's, which is also the
// row's source_id — and never writes a phone (google/calendar.go
// storeUnmatchedAttendee). No ordering of writes produces a Calendar candidate with
// a second address or a number, so declaring one would be a row the sync path
// cannot reach.
var multiMethodSources = map[string]bool{
	SourceGContacts: true,
}

// rematchClaimedSources are the sources whose stored EMAIL a rematch handler keys
// on, so an UNMATCHED row of that source can never hold an email a contact owns.
// The calendar handler looks its rows up by source_id == the email just added to a
// contact and marks them matched, explicitly so they leave the import queue
// (google/calendar_rematch.go). That closes both write orders: a sync MATCHES such
// an attendee instead of storing it, and a contact created after the row was stored
// makes the handler claim it. Coupling a candidate's email to a contact is
// therefore unproducible here, however the email got there.
//
// The address book is deliberately absent: no registered rematch handler reads
// gcontacts rows, and its own matcher runs only during a sync of that record, so a
// stored row whose email a later-created contact shares stays unmatched — the state
// the resolver's already-present-method bucket exists to serve.
var rematchClaimedSources = map[string]bool{
	SourceCalendarAttendee: true,
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
	primaryMethod   string
	outreach        *Amount
	mutualMeeting   *Amount
	awaitingReply   bool
	history         *int
	nameEdge        string
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

// PrimaryMethod marks one of the declared method kinds as the contact's primary.
//
// Without it the first method built is primary, which is the email whenever the
// contact carries one — so a fixture whose claim is that a NON-default method
// carries the primary mark has no other way to state itself. The kind must appear
// in this contact's own Methods set, and the resulting row set carries exactly one
// primary, which is what the contact API's own validator enforces.
func PrimaryMethod(kind string) ContactProp {
	return func(p *contactPlan) { p.primaryMethod = kind }
}

// Outreach declares an OUTBOUND email sent the stated amount before the run
// anchor. An outbound touches neither last_contacted nor last_interaction_at, so
// this is the fixture for "I reached out and have heard nothing back":
// last_outreach_at is set and last_response_at stays null.
//
// It lowers to a REPLAYED outbound Gmail message — the sync's own interaction
// write — so it requires an addressable email. Unlike OverdueBy it does not
// backdate creation: no forward-only due date has to be dragged backwards here,
// and the outbound-only fixture the standard world already seeds does not either.
// The corollary is the caller's to hold: pairing it with CreatedAgo(small) dates
// the message before the contact existed, which nothing rejects because the
// toolkit's own outbound-only reference fixture has that same shape.
//
// It is mutually exclusive with NeverContacted, which claims the opposite about
// the same timeline.
func Outreach(a Amount) ContactProp {
	return func(p *contactPlan) {
		amount := a
		p.outreach = &amount
	}
}

// MutualMeeting declares a past calendar meeting the stated amount before the run
// anchor. The calendar sync records an attended event as MUTUAL, and mutual bumps
// last_contacted, last_outreach_at AND last_response_at together, so this is the
// fixture for "we spoke, in both directions".
//
// It lowers to a REPLAYED GCal event, which addresses the contact by its email
// address, so it likewise requires an addressable email.
//
// Because a mutual moves last_contacted and recomputes contact_by forward, it is
// mutually exclusive with OverdueBy (which would come out not overdue) and with
// NeverContacted (which claims no history at all), and because a mutual is a
// REPLY it is mutually exclusive with AwaitingReply.
func MutualMeeting(a Amount) ContactProp {
	return func(p *contactPlan) {
		amount := a
		p.mutualMeeting = &amount
	}
}

// AwaitingReply declares a LIVE follow-up loop on the contact — the state
// has_pending_followup reports and the "awaiting reply" indicator renders.
//
// It lowers to the harness's follow-up primitive, whose row is key-for-key the
// shape FollowUpManager writes in production, in the `managed` state a promoted
// remote create settles on. has_pending_followup is computed LIVE from that row's
// (lifecycle, state) pair, so the state is genuinely reachable by seeding even
// though the seed harness runs the follow-up consumer off.
//
// A follow-up loop is opened BY an outbound and by nothing else, so it requires a
// Cadence and Outreach: hung on a contact with no outbound it renders as awaiting a
// reply to nothing, which production cannot reach.
//
// It is mutually exclusive with every prop that replays an inbound or a mutual
// interaction (MutualMeeting, OverdueBy, History), because a reply COMPLETES a live
// follow-up. A fixture pairing them would compose a live loop beside the reply that
// closes it — a state no ordering of production writes produces.
func AwaitingReply() ContactProp {
	return func(p *contactPlan) { p.awaitingReply = true }
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
// no two entities may declare the same pair — nor may one pinned name CONTAIN
// another, since the selectors that resolve these fixtures match on substring.
//
// Registration checks the DUPLICATE case within one entity list. The containment
// case, and the duplicate case ACROSS lists — the composed world runs every
// declaration and edge against ONE namespace — are a stated invariant with no
// automated check. That is a deliberate choice, not an oversight: an
// accumulate-and-compare inside Register would see the cross-list pairs (each is
// examined when its later member registers), but Register PANICS and this package
// is linked into crm-api, so a collision would abort the API at startup rather
// than fail a named test. Adding a pinned name means checking it against the
// registered set by hand.
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
	if p.primaryMethod != "" {
		if p.noMethods {
			return fmt.Errorf("contact %q: PrimaryMethod and NoMethods are mutually exclusive", p.name)
		}
		if !methodKinds[p.primaryMethod] {
			return fmt.Errorf("contact %q: unknown primary method kind %q (valid: %s)",
				p.name, p.primaryMethod, strings.Join(sortedMethodKinds(), ", "))
		}
		if !seen[p.primaryMethod] {
			return fmt.Errorf("contact %q: PrimaryMethod(%q) names a kind the declared Methods set does not carry — the primary has to be one of the contact's own methods",
				p.name, p.primaryMethod)
		}
	}
	for label, amount := range map[string]*Amount{
		"OverdueBy":     p.overdueBy,
		"CreatedAgo":    p.createdAgo,
		"Outreach":      p.outreach,
		"MutualMeeting": p.mutualMeeting,
	} {
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
		if p.mutualMeeting != nil {
			return fmt.Errorf("contact %q: OverdueBy and MutualMeeting are mutually exclusive — a mutual interaction moves last_contacted AND recomputes contact_by forward, and the meeting is dated nearer the anchor than OverdueBy's backdated history, so the contact would come out NOT overdue while still declaring that it is", p.name)
		}
	}
	if p.outreach != nil || p.mutualMeeting != nil {
		if !p.hasEmail() {
			return fmt.Errorf("contact %q: Outreach and MutualMeeting lower to a replayed EMAIL-addressed source payload, so the contact must carry an email method", p.name)
		}
		// NeverContacted is a claim by OMISSION — no interaction history at all,
		// which is what keeps last_contacted null — so it cannot coexist with a prop
		// that replays one. OverdueBy and History are rejected in their own blocks;
		// these two are the rest of the set.
		if p.neverContacted {
			prop, records := "Outreach", "an OUTBOUND email interaction"
			if p.mutualMeeting != nil {
				prop, records = "MutualMeeting", "a MUTUAL calendar interaction, which also sets last_contacted"
			}
			return fmt.Errorf("contact %q: NeverContacted and %s are mutually exclusive — %s replays %s, so the two state opposite things about the same timeline", p.name, prop, prop, records)
		}
	}
	if p.awaitingReply {
		if p.cadence == "" {
			return fmt.Errorf("contact %q: AwaitingReply requires Cadence — a follow-up loop only opens for a contact that has one", p.name)
		}
		if p.outreach == nil {
			return fmt.Errorf("contact %q: AwaitingReply requires Outreach — a follow-up loop is opened by an OUTBOUND and by nothing else (the follow-up manager routes outbound to its create branch and inbound/mutual to its complete branch), so a contact with no outbound has nothing that could have opened the loop it declares", p.name)
		}
		// The COUPLING, not just the presence of an outbound. A live follow-up
		// means no reply has arrived since the outbound that opened it: an inbound
		// or mutual interaction COMPLETES the loop rather than coexisting with it,
		// and the lowering replays every such interaction before it creates the
		// follow-up, so a fixture combining the two composes a state the
		// application would already have closed.
		for _, coupling := range []struct {
			prop     string
			declared bool
			records  string
		}{
			{"MutualMeeting", p.mutualMeeting != nil, "a MUTUAL calendar interaction"},
			{"OverdueBy", p.overdueBy != nil, "an INBOUND email"},
			{"History", p.history != nil, "INBOUND emails"},
		} {
			if coupling.declared {
				return fmt.Errorf("contact %q: AwaitingReply and %s are mutually exclusive — %s replays %s, and a reply COMPLETES a live follow-up rather than leaving it pending, so the composed state is a live loop beside the very reply that would have closed it (declare the outbound alone for the awaiting-reply fixture; a contact that HAS replied is not awaiting one)",
					p.name, coupling.prop, coupling.prop, coupling.records)
			}
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
	name            string
	source          string
	sameNameAs      string
	sameEmailAs     string
	emails          int
	phones          int
	telegramHandle  bool
	noIdentity      bool
	titleToken      string
	messageCount    int
	coOccurringWith string
}

func (p *externalCandidatePlan) handle() string { return p.name }
func (p *externalCandidatePlan) kind() string   { return "external_contact" }

// refs are the earlier contact handles this candidate reads. Every handle-bearing
// prop appears here so the ONE earlier-and-is-a-contact check in
// validateEntityOrder covers all of them.
func (p *externalCandidatePlan) refs() []string {
	var out []string
	seen := map[string]bool{}
	for _, ref := range []string{p.sameNameAs, p.sameEmailAs, p.coOccurringWith} {
		if ref == "" || seen[ref] {
			continue
		}
		seen[ref] = true
		out = append(out, ref)
	}
	return out
}

// ExternalCandidate declares one UNMATCHED import-queue candidate.
//
// match_status is deliberately NOT declarable: every write path hardcodes
// 'unmatched' on insert, so a matched or linked candidate would be a row the
// sync path cannot produce.
func ExternalCandidate(handle string, props ...CandidatePropSource) Entity {
	p := &externalCandidatePlan{name: handle}
	for _, prop := range props {
		prop.applyCandidate(p)
	}
	return p
}

// Source names the import candidate's source. There is no default: which writer
// produces the row, what its source_id looks like, and which fields it can carry
// all follow from the source, so leaving it implicit would hide all three.
func Source(name string) CandidateProp {
	return func(p *externalCandidatePlan) { p.source = name }
}

// SameEmailAs gives this candidate the SAME primary email as an earlier CONTACT
// handle. That email overlap is what raises a name collision to a high-confidence
// suggested match, and it is what puts the shared method in the resolver modal's
// already-present bucket rather than its to-add one.
func SameEmailAs(contactHandle string) CandidateProp {
	return func(p *externalCandidatePlan) { p.sameEmailAs = contactHandle }
}

// Emails declares how many email addresses the candidate carries (default one).
// The addresses are generator-derived and unknown to every seeded contact unless
// SameEmailAs overrides the first one.
func Emails(n int) CandidateProp {
	return func(p *externalCandidatePlan) { p.emails = n }
}

// Phones declares how many phone numbers the candidate carries (default none).
func Phones(n int) CandidateProp {
	return func(p *externalCandidatePlan) { p.phones = n }
}

// TelegramHandle gives a telegram candidate the '@handle' the matcher stores in
// metadata.username — the value the card renders as a t.me chip, and the
// last-resort display name for a peer with no name fields.
func TelegramHandle() CandidateProp {
	return func(p *externalCandidatePlan) { p.telegramHandle = true }
}

// NoIdentity declares a telegram peer with NO name fields at all. Combined with
// TelegramHandle it is the handle-only peer whose heading falls back to the
// handle; on its own it is the UNRESOLVED peer the Imports queue hides behind its
// opt-in toggle. Fixture-by-omission: the state is defined by what the discovery
// pass did NOT learn, so there is nothing to set.
func NoIdentity() CandidateProp {
	return func(p *externalCandidatePlan) { p.noIdentity = true }
}

// TitleToken declares an anarlog_title weak candidate for the named token group.
// Two candidates sharing a group share the normalized token and get DISTINCT
// session uuids, which is what makes them ONE grouped row whose evidence count is
// the number of members.
func TitleToken(group string) CandidateProp {
	return func(p *externalCandidatePlan) { p.titleToken = group }
}

// CorrespondenceEvidence declares the co-occurrence evidence a
// gmail_correspondence candidate's badge renders: the aggregated message count and
// an EARLIER contact handle this address most often co-appeared with.
func CorrespondenceEvidence(messageCount int, coOccurringContactHandle string) CandidateProp {
	return func(p *externalCandidatePlan) {
		p.messageCount = messageCount
		p.coOccurringWith = coOccurringContactHandle
	}
}

func (p *externalCandidatePlan) validate() error {
	if strings.TrimSpace(p.name) == "" {
		return fmt.Errorf("external candidate handle must be non-empty")
	}
	if !candidateSources[p.source] {
		return fmt.Errorf("external candidate %q: unknown source %q (valid: %s)",
			p.name, p.source, strings.Join(sortedCandidateSources(), ", "))
	}
	if p.sameNameAs != "" && !directUpsertSources[p.source] {
		return fmt.Errorf("external candidate %q: SameNameAs needs a direct-upsert source (%s) — the ingest payload and the telegram/title writers mint their own display name",
			p.name, strings.Join(sortedKeys(directUpsertSources), ", "))
	}
	if p.emails < 0 || p.phones < 0 {
		return fmt.Errorf("external candidate %q: Emails/Phones cannot be negative", p.name)
	}
	if (p.emails > 0 || p.phones > 0 || p.sameEmailAs != "") && !methodBearingSources[p.source] {
		return fmt.Errorf("external candidate %q: a declared method set needs source %s — gmail_correspondence keys its source_id on its single address, and the ingest and discovery writers do not take one",
			p.name, strings.Join(sortedKeys(methodBearingSources), ", "))
	}
	// The coupling, not just the COUNT. SameEmailAs sets no count at all, so it
	// slips past the shape check below while putting a contact's own address on the
	// row — which for a rematch-claimed source is the one email value that cannot
	// coexist with an unmatched status.
	if p.sameEmailAs != "" && rematchClaimedSources[p.source] {
		return fmt.Errorf("external candidate %q: source %q cannot hold an email a contact owns — its rematch handler claims such a row the moment the contact gains that address, and a sync matches the attendee rather than storing it, so an unmatched row with SameEmailAs(%q) is unreachable in either write order (a generated single email is fine; %s is the source that keeps an unmatched row on a shared address)",
			p.name, p.source, p.sameEmailAs, strings.Join(sortedKeys(multiMethodSources), ", "))
	}
	// The per-source method SHAPE, not just whether methods are declarable at all.
	// The calendar provider writes exactly one email and no phone on every write, so
	// a wider Calendar candidate is unproducible in any order.
	if !multiMethodSources[p.source] {
		if p.emails > 1 {
			return fmt.Errorf("external candidate %q: source %q stores exactly ONE email (its source_id), so Emails(%d) is a row its writer cannot produce — %s is the source whose provider emits every address off the record",
				p.name, p.source, p.emails, strings.Join(sortedKeys(multiMethodSources), ", "))
		}
		if p.phones > 0 {
			return fmt.Errorf("external candidate %q: source %q never stores a phone, so Phones(%d) is a row its writer cannot produce — %s is the source whose provider emits phone numbers",
				p.name, p.source, p.phones, strings.Join(sortedKeys(multiMethodSources), ", "))
		}
	}
	if p.telegramHandle && p.source != SourceTelegram {
		return fmt.Errorf("external candidate %q: TelegramHandle requires Source(%q)", p.name, SourceTelegram)
	}
	if p.noIdentity {
		if p.source != SourceTelegram {
			return fmt.Errorf("external candidate %q: NoIdentity requires Source(%q) — every other writer stores a name", p.name, SourceTelegram)
		}
		if p.sameNameAs != "" {
			return fmt.Errorf("external candidate %q: NoIdentity and SameNameAs are mutually exclusive — one omits every name field, the other pins one", p.name)
		}
	}
	if (p.titleToken != "") != (p.source == SourceAnarlogTitle) {
		return fmt.Errorf("external candidate %q: TitleToken and Source(%q) require each other — an anarlog_title row IS a session-title token",
			p.name, SourceAnarlogTitle)
	}
	if p.messageCount != 0 || p.coOccurringWith != "" {
		if p.source != SourceCorrespondence {
			return fmt.Errorf("external candidate %q: CorrespondenceEvidence requires Source(%q)", p.name, SourceCorrespondence)
		}
		if p.messageCount < 1 || p.coOccurringWith == "" {
			return fmt.Errorf("external candidate %q: CorrespondenceEvidence needs a positive message count AND a co-occurring contact handle", p.name)
		}
	}
	return nil
}

// --- meeting notes ----------------------------------------------------------

// meetingNotePlan is the lowered form of a MeetingNote declaration.
type meetingNotePlan struct {
	name string
}

func (p *meetingNotePlan) handle() string { return p.name }
func (p *meetingNotePlan) kind() string   { return "meeting_note" }
func (p *meetingNotePlan) refs() []string { return nil }
func (p *meetingNotePlan) validate() error {
	if strings.TrimSpace(p.name) == "" {
		return fmt.Errorf("meeting note handle must be non-empty")
	}
	return nil
}

// MeetingNote declares one ORPHANED meeting note — a recorded session the linker
// could not attach to any calendar event, which is the Imports Interactions tab's
// needs-attention surface. Title and summary are generator-derived.
//
// It hangs off the namespace's own synthetic mac_host, which the harness already
// seeds REVOKED — invisible to every host-listing route, so it neither contends
// for the paired-host singleton index nor can be deleted by a spec that resets
// hosts. The cleanup ladder already deletes meeting notes by that host id.
//
// Only the orphan state is declarable: a conflict row needs a well-formed
// conflict_candidates snapshot referencing real events, which no toolkit producer
// can build.
func MeetingNote(handle string) Entity {
	return &meetingNotePlan{name: handle}
}

// --- calendar events --------------------------------------------------------

// Offsets a declared calendar event may carry, bounded by the provider's OWN
// initial fetch window (now − CalendarPastSyncDays … now + CalendarFutureSyncDays).
// The fake fetcher ignores the window, so nothing enforces it at runtime — which
// is precisely why it is enforced at REGISTRATION: an event outside it is a row
// the real sync could never have fetched.
//
// Each bound loses a day to the meeting's own extent. An upcoming meeting starts
// n days out and runs an hour, a past one started n days plus two hours ago, so
// the last fully-inside day in each direction is one short of the window.
var (
	maxCalendarDaysAhead = google.CalendarFutureSyncDays - 1
	maxCalendarDaysAgo   = google.CalendarPastSyncDays - 1
)

// calendarEventPlan is the lowered form of a CalendarEvent declaration.
type calendarEventPlan struct {
	name    string
	contact string
	// unmatched marks the UnmatchedCalendarEvent form: the only non-self attendee
	// is an address no contact owns, so the row lands with an empty matched set
	// plus a gcal_attendee import candidate.
	unmatched      bool
	startsInDays   *int
	startedDaysAgo *int
	inProgress     bool
	untitled       bool
	location       bool
	sourceLink     bool
	soleAttendee   bool
}

func (p *calendarEventPlan) handle() string { return p.name }
func (p *calendarEventPlan) kind() string   { return "calendar_event" }
func (p *calendarEventPlan) refs() []string {
	if p.contact == "" {
		return nil
	}
	return []string{p.contact}
}

func (p *calendarEventPlan) validate() error {
	if strings.TrimSpace(p.name) == "" {
		return fmt.Errorf("calendar event handle must be non-empty")
	}
	if p.unmatched {
		if p.contact != "" {
			return fmt.Errorf("calendar event %q: an unmatched event names no contact — its attendee is an address no contact owns", p.name)
		}
	} else if strings.TrimSpace(p.contact) == "" {
		return fmt.Errorf("calendar event %q: a contact handle is required", p.name)
	}

	offsets := 0
	if p.startsInDays != nil {
		offsets++
	}
	if p.startedDaysAgo != nil {
		offsets++
	}
	if p.inProgress {
		offsets++
	}
	if offsets != 1 {
		return fmt.Errorf("calendar event %q: declare exactly ONE of StartsInDays, StartedDaysAgo or InProgress (got %d) — the three are mutually exclusive placements of the same meeting",
			p.name, offsets)
	}
	if n := p.startsInDays; n != nil && (*n < 1 || *n > maxCalendarDaysAhead) {
		return fmt.Errorf("calendar event %q: StartsInDays(%d) is outside 1..%d — the calendar provider's initial fetch reaches %d days ahead, so a later event is a row no sync could produce",
			p.name, *n, maxCalendarDaysAhead, google.CalendarFutureSyncDays)
	}
	if n := p.startedDaysAgo; n != nil && (*n < 1 || *n > maxCalendarDaysAgo) {
		return fmt.Errorf("calendar event %q: StartedDaysAgo(%d) is outside 1..%d — the calendar provider's initial fetch reaches %d days back, so an older event is a row no sync could produce",
			p.name, *n, maxCalendarDaysAgo, google.CalendarPastSyncDays)
	}
	if p.untitled && p.sourceLink {
		// The title IS the link (meetings.tsx renders the anchor around it), so an
		// untitled event with a link renders the fallback label as the link text —
		// a state whose two declared intents contradict each other at the surface
		// they both name.
		return fmt.Errorf("calendar event %q: Untitled and SourceLink contradict — the card renders the link AS the title, so a link on an untitled event has no title to become", p.name)
	}
	return nil
}

// CalendarEvent declares one meeting the connected account attended together
// with an EARLIER contact handle, stored by the real calendar sync provider.
//
// Placement is mandatory and singular: exactly one of StartsInDays,
// StartedDaysAgo or InProgress. That is not decoration — the Meetings section
// classifies a meeting by comparing its END time against the app's accelerated
// clock, and the three placements are the three sides of that comparison.
//
// The offsets resolve to REAL days (n × 24h), deliberately NOT through the
// cadence-scaled Amount type. Under CRM_ENV=testing a cadence "day" compresses to
// roughly seventeen seconds, so a cadence-scaled "upcoming in 3 days" would sit
// under a minute ahead and flip to past mid-test. Nothing in the comparison the
// component performs has a cadence dimension, so scaling it is not independence
// but a category error — and the failure it buys is a flake, not a red test.
//
// The title and the location are generator-derived and reported in the manifest;
// a citing test reads them from there or from the events API rather than
// restating a literal.
func CalendarEvent(handle, contactHandle string, props ...CalendarEventProp) Entity {
	p := &calendarEventPlan{name: handle, contact: contactHandle}
	for _, prop := range props {
		prop(p)
	}
	return p
}

// UnmatchedCalendarEvent declares a stored meeting whose only non-self attendee
// is an address NO seeded contact owns — the state the calendar sync leaves when
// it cannot match an attendee: an empty matched set plus one gcal_attendee import
// candidate holding exactly that email.
//
// The manifest reports the ATTENDEE EMAIL as the row's name, because a citing
// test has no other way to learn it (the rematch flow types that address into a
// contact's edit form). The manifest ID stays the calendar_event row uuid, which
// is what the contact events API returns as its `id`.
//
// The email is generator-derived and no declared contact can own it, which is
// also what keeps the row producible: the calendar rematch handler claims any
// stored gcal_attendee row whose email a contact holds, so a candidate coupled to
// a declared contact's address is a row that cannot survive its own sync.
func UnmatchedCalendarEvent(handle string, props ...CalendarEventProp) Entity {
	p := &calendarEventPlan{name: handle, unmatched: true}
	for _, prop := range props {
		prop(p)
	}
	return p
}

// CalendarEventProp customizes a declared calendar event.
type CalendarEventProp func(*calendarEventPlan)

// StartsInDays places the meeting n real days ahead of the run anchor. Upcoming
// by the component's end-time comparison, and — because the past-event projection
// reads only events that have ended — it publishes nothing at all: no attended
// event, no interaction, no venue node.
func StartsInDays(n int) CalendarEventProp {
	return func(p *calendarEventPlan) { p.startsInDays = &n }
}

// StartedDaysAgo places the meeting n real days before the run anchor. Past, so
// the provider projects the attendance: a calendar.attended event, a MUTUAL
// interaction, and a venue node.
func StartedDaysAgo(n int) CalendarEventProp {
	return func(p *calendarEventPlan) { p.startedDaysAgo = &n }
}

// InProgress places a two-hour meeting STRADDLING the run anchor — started an
// hour before it, ending an hour after. It is the one shape that separates
// end-time classification from start-time classification: its start is past and
// its end is not, so a component that classified on start would call it past.
//
// Deliberately a single fixed shape rather than a general duration knob: one
// consumer needs exactly "straddles now", and its end time is after the anchor,
// so like an upcoming meeting it publishes nothing.
func InProgress() CalendarEventProp {
	return func(p *calendarEventPlan) { p.inProgress = true }
}

// Untitled leaves the meeting's summary empty, which the provider stores as the
// empty STRING (never NULL) and the card renders with its fallback label.
func Untitled() CalendarEventProp {
	return func(p *calendarEventPlan) { p.untitled = true }
}

// EventLocation gives the meeting a generator-derived place. Without it the
// provider maps the empty location to NULL and the response omits the key
// entirely, which is the no-location state the card's absence assertion reads —
// so there is no blank-location value to declare.
func EventLocation() CalendarEventProp {
	return func(p *calendarEventPlan) { p.location = true }
}

// SourceLink gives the meeting an external event link, omitted the same way when
// absent. The card renders the title as an anchor when it is present.
func SourceLink() CalendarEventProp {
	return func(p *calendarEventPlan) { p.sourceLink = true }
}

// SoleAttendee makes the stored attendee list hold exactly the peer, the only
// shape that yields an attendee count of one. The provider stores every attendee
// INCLUDING the account, so a default meeting always stores two and the card
// always renders its count row; a one-attendee meeting is one the account
// ORGANIZES without being on the attendee list.
func SoleAttendee() CalendarEventProp {
	return func(p *calendarEventPlan) { p.soleAttendee = true }
}

// --- paired Mac host --------------------------------------------------------

// macHostPlan is the lowered form of a MacHost declaration.
type macHostPlan struct {
	name    string
	sources []macHostSource
}

// macHostSource is one source_health entry the declared host reports.
type macHostSource struct {
	name             string
	backfillComplete bool
}

func (p *macHostPlan) handle() string { return p.name }
func (p *macHostPlan) kind() string   { return "mac_host" }
func (p *macHostPlan) refs() []string { return nil }

func (p *macHostPlan) validate() error {
	if strings.TrimSpace(p.name) == "" {
		return fmt.Errorf("mac host handle must be non-empty")
	}
	if len(p.sources) == 0 {
		return fmt.Errorf("mac host %q: declare at least one source — the source-health table renders nothing for an empty blob, so a host with no sources provisions a surface no assertion can read", p.name)
	}
	seen := map[string]bool{}
	for _, s := range p.sources {
		if !mac.IsAllowedPushSource(s.name) {
			return fmt.Errorf("mac host %q: %q is not a source the daemon may push (allowed: %s)", p.name, s.name, strings.Join(sortedPushSources(), ", "))
		}
		if seen[s.name] {
			return fmt.Errorf("mac host %q: source %q declared twice — source_health is a map, so the second entry would silently replace the first", p.name, s.name)
		}
		seen[s.name] = true
	}
	return nil
}

// MacHost declares the ONE LIVE paired Mac host a world may hold, created through
// the real pairing services (mint a token, pair with it, heartbeat the
// daemon-supplied fields) rather than inserted.
//
// The real path is not a preference. The harness's own marker host is REVOKED —
// which is how it dodges the database-wide singleton index — and a revoked host is
// invisible to every host-listing route, so the settings surface cannot see it.
// Un-revoking it would be a new test-only mutation of a column production only
// ever moves one way.
//
// Consequences a declaration inherits: the host takes the singleton slot, so two
// paired worlds cannot coexist and the second one's seed fails rather than
// queues; and it is LIVE, so it is visible to the sync-staleness watchdog. Both
// are bounded by cleanup deleting the row, which is why the live host is tracked
// in the failure-path teardown as well as the cross-request ladder.
//
// Permissions are fixed rather than declarable: one granted and one denied is the
// single shape that makes both badge states observable on one host, and no
// consuming surface distinguishes any other combination.
func MacHost(handle string, props ...MacHostProp) Entity {
	p := &macHostPlan{name: handle}
	for _, prop := range props {
		prop(p)
	}
	return p
}

// MacHostProp customizes a declared paired host.
type MacHostProp func(*macHostPlan)

// PushedSource adds a source-health entry in the MID-PUSH state: enabled, pushed
// recently, carrying a cursor, backfill not complete. That is the ordinary steady
// state of a paired source and — for the sources that report backfill progress —
// the state whose cell must withhold the opaque change token.
//
// last_pushed_at is the run anchor, deliberately fresh. A stale one would open a
// push_stale breach, and the staleness banner it raises is a GLOBAL surface that
// another spec asserts is quiet, so a stale fixture here would fail a test in a
// different file on a different worker.
func PushedSource(name string) MacHostProp {
	return func(p *macHostPlan) { p.sources = append(p.sources, macHostSource{name: name}) }
}

// BackfilledSource adds a source-health entry whose backfill has COMPLETED. For a
// source that reports backfill progress this is what licenses the cell to
// substitute the live per-host contact count for the change token — so a
// declaration using it also owes that host some candidates to count.
func BackfilledSource(name string) MacHostProp {
	return func(p *macHostPlan) {
		p.sources = append(p.sources, macHostSource{name: name, backfillComplete: true})
	}
}

// sortedPushSources is the daemon's push-source allowlist, sorted, for error text.
func sortedPushSources() []string {
	out := make([]string, 0, len(mac.AllowedPushSources))
	for src := range mac.AllowedPushSources {
		out = append(out, src)
	}
	sort.Strings(out)
	return out
}

// --- method suggestions -----------------------------------------------------

// methodSuggestionPlan is the lowered form of a MethodSuggestion declaration.
type methodSuggestionPlan struct {
	name    string
	contact string
	source  string
}

func (p *methodSuggestionPlan) handle() string { return p.name }
func (p *methodSuggestionPlan) kind() string   { return "method_suggestion" }
func (p *methodSuggestionPlan) refs() []string { return []string{p.contact} }
func (p *methodSuggestionPlan) validate() error {
	if strings.TrimSpace(p.name) == "" {
		return fmt.Errorf("method suggestion handle must be non-empty")
	}
	if strings.TrimSpace(p.contact) == "" {
		return fmt.Errorf("method suggestion %q: a contact handle is required", p.name)
	}
	if p.source != SourceGContacts {
		return fmt.Errorf("method suggestion %q: source must be %q — it is the only address-book source written by the direct upsert this lowering composes (icloud_contacts rows come from the ingest pipeline, which cannot produce a linked row carrying a pending suggestion)",
			p.name, SourceGContacts)
	}
	return nil
}

// MethodSuggestion declares an address-book row already LINKED to an earlier
// contact handle and carrying ONE pending method the contact does not have — the
// state the address-book reconciler leaves behind, and the fixture behind the
// method-suggestion card at the top of the Imports People tab.
//
// The pending method is a single generator-derived email because that is what both
// consuming surfaces exercise; a dismissed set is deliberately absent rather than
// declared-and-unread. The row rides the external_contact cleanup steps: the
// suggestion columns are JSONB on the row itself, and external_contacts is swept
// before contacts, which is the order its crm_contact_id FK needs.
func MethodSuggestion(handle, contactHandle, source string) Entity {
	return &methodSuggestionPlan{name: handle, contact: contactHandle, source: source}
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

func sortedCandidateSources() []string { return sortedKeys(candidateSources) }

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// validateEntityOrder checks that every entity's refs name a CONTACT declared
// EARLIER in the same list, and that no handle repeats. Register and RegisterEdge
// share it so the two cannot disagree about what a well-formed entity list is.
func validateEntityOrder(entities []Entity) error {
	if err := validateIngestCandidatesFollowHost(entities); err != nil {
		return err
	}
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
			// SameEmailAs copies the referenced contact's PRIMARY email, so a
			// contact that carries none would silently leave the candidate with its
			// own generated address and no overlap at all — the opposite of the
			// state the prop names.
			if p, ok := e.(*externalCandidatePlan); ok && p.sameEmailAs == ref {
				if contact, isContact := target.(*contactPlan); !isContact || !contact.hasEmail() {
					return fmt.Errorf("external candidate %q: SameEmailAs(%q) needs a contact that carries an email method", e.handle(), ref)
				}
			}
		}
		if _, dup := seen[e.handle()]; dup {
			return fmt.Errorf("duplicate entity handle %q", e.handle())
		}
		seen[e.handle()] = e
	}
	return nil
}

// validateIngestCandidatesFollowHost enforces the ONE ordering rule the mac-host
// entity introduces. An ingest-sourced candidate is stamped with the host it was
// pushed from, and the per-host source-count route the settings surface reads
// counts strictly by that column — so when a declaration pairs a LIVE host, its
// ingest candidates must land on that host rather than on the harness's revoked
// marker. The lowering routes them there implicitly (a world has at most one
// paired host, so there is nothing to disambiguate), which means a candidate
// declared BEFORE the host would silently land on the marker and be invisible to
// every host-scoped read — a fixture that provisions rows no assertion can see.
//
// The upsert makes it unrecoverable rather than merely wrong: host_id is set on
// insert and thereafter only via COALESCE(existing, new), so an existing owner is
// never reassigned.
func validateIngestCandidatesFollowHost(entities []Entity) error {
	hostIndex := -1
	hostHandle := ""
	for i, e := range entities {
		if p, ok := e.(*macHostPlan); ok {
			hostIndex = i
			hostHandle = p.name
			break
		}
	}
	if hostIndex < 0 {
		return nil
	}
	for i, e := range entities {
		if i > hostIndex {
			break
		}
		p, ok := e.(*externalCandidatePlan)
		if !ok || !ingestCandidateSources[p.source] {
			continue
		}
		return fmt.Errorf("external candidate %q (source %s) is declared BEFORE the paired mac host %q — an ingest candidate is stamped with the host it was pushed from, and the per-host count route reads only that column, so declare the host first",
			p.name, p.source, hostHandle)
	}
	return nil
}

// ingestCandidateSources are the candidate sources written by the mac-daemon
// ingest pipeline, and therefore the ones that carry a host id.
var ingestCandidateSources = map[string]bool{
	SourceICloudContacts: true,
	SourceAnarlogHumans:  true,
}
