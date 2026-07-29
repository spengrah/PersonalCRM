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
}

// Postconditions derives one set of checkable facts per declared entity.
func (d Declaration) Postconditions() []Postcondition {
	out := make([]Postcondition, 0, len(d.Entities))
	for _, e := range d.Entities {
		p, ok := e.(*contactPlan)
		if !ok {
			continue
		}
		out = append(out, p.postcondition())
	}
	return out
}

func (p *contactPlan) postcondition() Postcondition {
	pc := Postcondition{Handle: p.name, Listed: true}

	if p.cadence != "" {
		cadence := p.cadence
		pc.Cadence = &cadence
	}

	if p.overdueBy != nil {
		// Replayed inbound history: last_contacted is moved by the interaction,
		// and the message is aged a full period past the declared amount, so the
		// contact is overdue by at least that amount.
		overdue, contacted := true, true
		pc.OverdueMember = &overdue
		pc.LastContacted = &contacted
	} else {
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
	return pc
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
