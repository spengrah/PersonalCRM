package declare

import (
	"sort"
	"time"
)

// Postcondition is a checkable, API-READ fact derived from ONE declared
// property. The declaration is the single statement of intent, so the
// satisfiability test asserts what the declaration itself says rather than a
// second, hand-written expectation list that could drift from it.
//
// A nil pointer field means "this declaration says nothing about that, do not
// assert it".
type Postcondition struct {
	// Handle is the declaration-local handle whose created row this describes.
	Handle string
	// Listed: the contact appears in the namespace-scoped contact list read.
	// Always true — every declared entity must be reachable through the API.
	Listed bool
	// OverdueMember: whether the overdue read must contain this contact.
	OverdueMember *bool
	// Cadence: the cadence the detail read must show.
	Cadence *string
	// LastContacted: whether the detail read's last_contacted must be non-null.
	// True only where the declaration asked for replayed history, so a nil
	// last_contacted with no interaction is a FAILURE, not a shrug.
	LastContacted *bool
	// CreatedAgo: how far before Result.Anchor created_at must sit. Asserted
	// TWO-SIDED — an upper bound alone would pass a contact created arbitrarily
	// early, which is precisely the mis-seed this catches.
	CreatedAgo *time.Duration
	// MethodKinds: the EXACT set of method kinds the detail read must show
	// (empty and non-nil means "exactly zero methods").
	MethodKinds []string
	// Birthday: the date the detail read must show, derived from BirthdayInDays
	// / BirthdayOn / BirthdayPlaceholderToday against the run anchor.
	Birthday *time.Time
	// Location: the RAW (unprefixed) declared location. The assertion computes
	// the expected stored value by prefixing it with the run's own namespace, so
	// this field stays a pure function of the declaration like every other one.
	Location *string
	// InteractionCount: the EXACT number of rows the interactions read must
	// return, derived from History(n).
	InteractionCount *int
	// CreatedBeforeOldestInteraction: created_at must be STRICTLY earlier than
	// the oldest interaction's occurred_at (History's creation-margin rule,
	// checked through the read path rather than only in a unit test).
	CreatedBeforeOldestInteraction bool
	// ExplicitName: the EXACT rendered display name (namespace prefix aside) a
	// pinned literal must produce. It is the only checkable fact ExplicitName
	// implies, and without it the lowering has no oracle at all: the manifest
	// name and the stored full_name are the same value read twice, so comparing
	// them cannot tell a pinned name from a drawn one.
	ExplicitName *string
	// NameEdge: the name-edge kind whose token the manifest name must carry.
	NameEdge *string
	// NameTwinOf: the handle whose rendered name this one must equal exactly.
	NameTwinOf *string
	// Present: false when a LATER entity removed this contact (a merge loser, a
	// soft-delete target). It is the one field derived from an entity OTHER than
	// the contact's own, because absence is a fact about the contact.
	Present *bool
}

// Postconditions derives one set of checkable facts per declared CONTACT entity.
//
// The derivation is over the WHOLE entity list, not per entity in isolation: a
// merge or a soft-delete declared later is what makes an earlier contact absent,
// and absence is a fact about that contact. The rule is otherwise unchanged and
// load-bearing — the declaration is the single statement, and there is no second
// hand-written expectation list to drift from it.
func (d Declaration) Postconditions() []Postcondition {
	return postconditionsFor(d.Entities)
}

// Postconditions derives the same checkable facts for an adversarial edge.
func (e Edge) Postconditions() []Postcondition {
	return postconditionsFor(e.Entities)
}

func postconditionsFor(entities []Entity) []Postcondition {
	removed := removedContactHandles(entities)
	absorbed := mergeWinnerHandles(entities)
	out := make([]Postcondition, 0, len(entities))
	for _, e := range entities {
		p, ok := e.(*contactPlan)
		if !ok {
			continue
		}
		pc := p.postcondition()
		if absorbed[p.name] && !removed[p.name] {
			// A merge WINNER absorbs the loser's methods, interactions and
			// cadence timestamps, so the facts derived from its OWN declaration
			// no longer describe the row. Deriving the post-merge state exactly
			// would mean re-implementing merge inside the expectation, which is
			// the second-source-of-truth this whole derivation exists to avoid —
			// so these say NOTHING, and the merge edge's own read-path check
			// asserts what actually reparented.
			pc.LastContacted = nil
			pc.OverdueMember = nil
			pc.MethodKinds = nil
			pc.InteractionCount = nil
			pc.CreatedBeforeOldestInteraction = false
		}
		if removed[p.name] {
			absent := false
			pc.Present = &absent
			pc.Listed = false
			// A removed contact's detail read is a 404, so nothing derived from
			// its own columns is assertable any more — the rendered NAME included,
			// since it is checked against the detail read's full_name. Overdue
			// membership stays, as false: leaving the overdue read is the
			// observable consequence of the removal, and it is the half most likely
			// to regress.
			pc.Cadence = nil
			pc.LastContacted = nil
			pc.CreatedAgo = nil
			pc.MethodKinds = nil
			pc.Birthday = nil
			pc.Location = nil
			pc.InteractionCount = nil
			pc.ExplicitName = nil
			pc.CreatedBeforeOldestInteraction = false
			notOverdue := false
			pc.OverdueMember = &notOverdue
		}
		out = append(out, pc)
	}
	return out
}

// removedContactHandles are the contact handles a later entity takes out of the
// live reads: every merge LOSER and every soft-delete target.
func removedContactHandles(entities []Entity) map[string]bool {
	removed := map[string]bool{}
	for _, e := range entities {
		switch p := e.(type) {
		case *mergePlan:
			removed[p.loser] = true
		case *softDeletePlan:
			removed[p.target] = true
		}
	}
	return removed
}

// mergeWinnerHandles are the contact handles that ABSORBED another contact.
func mergeWinnerHandles(entities []Entity) map[string]bool {
	winners := map[string]bool{}
	for _, e := range entities {
		if p, ok := e.(*mergePlan); ok {
			winners[p.winner] = true
		}
	}
	return winners
}

func (p *contactPlan) postcondition() Postcondition {
	pc := Postcondition{Handle: p.name, Listed: true}

	if p.cadence != "" {
		cadence := p.cadence
		pc.Cadence = &cadence
	}

	switch {
	case p.overdueBy != nil:
		// Replayed inbound history: last_contacted is moved by the interaction,
		// and the message is aged a full period past the declared amount, so the
		// contact is overdue by at least that amount.
		overdue, contacted := true, true
		pc.OverdueMember = &overdue
		pc.LastContacted = &contacted
	case p.history != nil:
		// Replayed history too, but dated to END recently: whether the contact is
		// overdue follows from the NEWEST message's realized instant against the
		// period, DERIVED here rather than restated, so a change to the spread or
		// the cadence cannot silently invalidate the claim.
		contacted := true
		pc.LastContacted = &contacted
		n := *p.history
		count := n
		pc.InteractionCount = &count
		pc.CreatedBeforeOldestInteraction = true
		newest := historyMessageAge(n-1, n) + sourceHistoryLag
		overdue := p.cadence != "" && newest > period(p.cadence)
		pc.OverdueMember = &overdue
	default:
		// No history was replayed, so there is nothing that could have moved
		// last_contacted. Overdue-ness, if any, comes from created_at + period.
		contacted := false
		pc.LastContacted = &contacted
		var createdAge time.Duration
		if p.createdAgo != nil {
			createdAge = p.createdAgo.resolve(p.cadence)
		}
		overdue := p.cadence != "" && createdAge > period(p.cadence)
		pc.OverdueMember = &overdue
	}

	// Both the explicitly declared and the OverdueBy-derived creation ages are
	// checkable facts, so both are asserted.
	if age, ok := creationAge(p); ok {
		pc.CreatedAgo = &age
	}

	pc.MethodKinds = p.expectedMethodKinds()

	if p.explicitNameSet {
		display := p.explicitGiven + " " + p.explicitSurname
		pc.ExplicitName = &display
	}
	if p.nameEdge != "" {
		kind := p.nameEdge
		pc.NameEdge = &kind
	}
	if p.sameNameAs != "" {
		twin := p.sameNameAs
		pc.NameTwinOf = &twin
	}
	if p.location != nil {
		loc := *p.location
		pc.Location = &loc
	}
	return pc
}

// resolveBirthday fills in the anchor-dependent postcondition fields. It is
// separate from postcondition() because the anchor is a RUN fact, not a
// declaration fact, and inventing one at derivation time would make the
// expectation drift from the row.
func (pc *Postcondition) resolveBirthday(p *contactPlan, anchor time.Time) {
	if p.birthday == nil {
		return
	}
	// The placeholder form goes through the SAME clamp the lowering uses, so the
	// expectation and the stored row cannot disagree about the one day it fires.
	if p.birthday.placeholder {
		bday := p.birthday.resolvePlaceholder(anchor)
		pc.Birthday = &bday
		return
	}
	bday := p.birthday.resolve(anchor)
	pc.Birthday = &bday
}

// PostconditionsAt is Postconditions with the run's anchor supplied, so the
// anchor-dependent facts (today's birthday, a leap-day birthday) are derived
// from the same number the lowering used.
func (d Declaration) PostconditionsAt(anchor time.Time) []Postcondition {
	return postconditionsAt(d.Entities, anchor)
}

// PostconditionsAt is the edge-side twin of Declaration.PostconditionsAt.
func (e Edge) PostconditionsAt(anchor time.Time) []Postcondition {
	return postconditionsAt(e.Entities, anchor)
}

func postconditionsAt(entities []Entity, anchor time.Time) []Postcondition {
	out := postconditionsFor(entities)
	byHandle := map[string]*contactPlan{}
	for _, e := range entities {
		if p, ok := e.(*contactPlan); ok {
			byHandle[p.name] = p
		}
	}
	for i := range out {
		if p, ok := byHandle[out[i].Handle]; ok && out[i].Present == nil {
			out[i].resolveBirthday(p, anchor)
		}
	}
	return out
}

// expectedMethodKinds is the exact method set the contact should carry: none
// when NoMethods, the declared set when Methods was given, and the factory's
// single-email default otherwise.
func (p *contactPlan) expectedMethodKinds() []string {
	if p.noMethods {
		return []string{}
	}
	if len(p.methods) == 0 {
		return []string{MethodEmail}
	}
	kinds := append([]string(nil), p.methods...)
	sort.Strings(kinds)
	return kinds
}
