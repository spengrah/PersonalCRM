package declare

import (
	"fmt"
	"time"
)

// The adversarial edge catalog.
//
// THE CATALOG IS THIS SLICE, IN THIS ORDER. Append only.
//
// Registration order is the composition contract (arc invariant I5), and making
// it a single explicit literal rather than an emergent property of init() order
// across files is what keeps it checkable: a second init() in another file could
// otherwise slip an edge in at a filename-dependent position and silently
// renumber every PRNG draw in the composed world. TestWorldPlan_EdgesKeepCatalogOrder
// holds the line: the world plan's edge segment must equal EdgeNames() rather
// than any sort of it.
var adversarialCatalog = []Edge{
	{
		Name: "long-history",
		Why:  "a contact whose timeline is long enough to paginate — catches interaction reads, ordering and paging metadata that only work on a handful of rows",
		Entities: []Entity{
			// The batch adapter settles once per dependency generation, so a long
			// timeline costs one settle rather than n.
			Contact("subject", Cadence("monthly"), History(48)),
		},
	},
	{
		Name: "zero-method",
		Why:  "a contact with no contact_method at all — catches every read that assumes at least one method exists (primary-method pickers, avatar/initials derivation, method-keyed matching)",
		Entities: []Entity{
			Contact("methodless", NoMethods(), Cadence("weekly")),
		},
	},
	{
		Name: "hostile-names",
		Why:  "truncation-hostile display names (long, right-to-left, emoji) — catches naive truncation that splits a surrogate pair, layout that assumes a short name, and bidirectional reordering",
		Entities: []Entity{
			Contact("long-name", NameEdge(NameEdgeLong)),
			Contact("rtl-name", NameEdge(NameEdgeRTL)),
			Contact("emoji-name", NameEdge(NameEdgeEmoji)),
		},
	},
	{
		Name:     "all-cadences-overdue",
		Why:      "every cadence in the vocabulary overdue at once — catches per-cadence period arithmetic that is only ever exercised for the common values",
		Entities: allCadencesOverdueEntities(),
	},
	{
		Name: "fully-empty",
		Why:  "a contact carrying nothing at all: no method, no cadence, no history, no birthday, no location — catches detail reads and cards that assume any one optional field is present",
		Entities: []Entity{
			Contact("empty", NoMethods()),
		},
	},
	{
		Name:     "deep-import-queue",
		Why:      "an import queue deeper than one page, in BOTH keying shapes the sync path produces (id-keyed address book, email-keyed correspondence) — catches per-source pagination and the source_id keying split",
		Entities: deepImportQueueEntities(),
	},
	{
		Name: "merge-chain",
		Why:  "a two-hop merge chain A→B→C where A owned children BEFORE the first merge — catches reparenting that only works for one hop, and pins what the API does with a merged-away id",
		Entities: []Entity{
			// A is given a real interaction and a note BEFORE any merge, which is
			// what makes the reparenting observable rather than vacuous.
			Contact("a", Cadence("weekly"), OverdueBy(Days(3))),
			Note("a-note", "a"),
			Contact("b"),
			Contact("c"),
			Merge("a", "b"),
			Merge("b", "c"),
		},
	},
	{
		Name: "soft-deleted-parent",
		Why:  "a tombstoned contact whose child rows are still live — catches the asymmetry between child surfaces that pre-check the parent and the one that does not",
		Entities: []Entity{
			// The overdue lowering gives the parent a real interaction, so the
			// tombstone genuinely orphans rows rather than nothing.
			Contact("parent", Cadence("weekly"), OverdueBy(Days(3))),
			Note("parent-note", "parent"),
			SoftDelete("parent"),
		},
	},
	{
		Name:     "page-overflow",
		Why:      "a cohort larger than one contacts page and an overdue population past fifty — replaces 'volume realism' with a state that either exists or does not",
		Entities: pageOverflowEntities(),
	},
	{
		Name: "same-name-pair",
		Why:  "two contacts sharing one display name plus an import candidate that collides with both — catches by-name resolution and pins what the matcher does with an ambiguous name tie",
		Entities: []Entity{
			Contact("twin-a"),
			Contact("twin-b", SameNameAs("twin-a")),
			// The candidate is what makes this a MATCHING collision rather than
			// two rows that happen to share a name.
			ExternalCandidate("collider", Source(SourceGContacts), SameNameAs("twin-a")),
		},
	},
	{
		Name: "birthday-window",
		Why:  "birthdays on today, tomorrow and February 29 — catches window-boundary bucketing and the leap-day next-occurrence rule",
		Entities: []Entity{
			Contact("bday-today", BirthdayInDays(0)),
			Contact("bday-tomorrow", BirthdayInDays(1)),
			Contact("bday-leap", BirthdayOn(time.February, 29)),
		},
	},
}

func init() {
	for _, e := range adversarialCatalog {
		RegisterEdge(e)
	}
}

// allCadencesOverdueEntities is one overdue contact per cadence in the
// vocabulary, handled by the cadence name so a failure names the value.
func allCadencesOverdueEntities() []Entity {
	cadences := Cadences()
	out := make([]Entity, 0, len(cadences))
	for _, c := range cadences {
		out = append(out, Contact(c, Cadence(c), OverdueBy(Days(2))))
	}
	return out
}

// deepImportQueueCountPerSource is how many candidates each source contributes.
// It is past the imports page size (20) in total and, at limit=10, gives each
// source an unambiguous two-page split — which is what makes the per-source
// pagination assertion exact rather than a bound.
const deepImportQueueCountPerSource = 12

// deepImportQueueEntities are the two shapes the direct-upsert seeding primitive
// can actually key: an id-keyed address book and an email-keyed correspondence
// discoverer. Only those two are declarable; a matched or linked candidate is
// not, because the write path hardcodes 'unmatched' on insert.
func deepImportQueueEntities() []Entity {
	out := make([]Entity, 0, 2*deepImportQueueCountPerSource)
	for i := 0; i < deepImportQueueCountPerSource; i++ {
		out = append(out, ExternalCandidate(fmt.Sprintf("gcontact-%02d", i), Source(SourceGContacts)))
	}
	for i := 0; i < deepImportQueueCountPerSource; i++ {
		out = append(out, ExternalCandidate(fmt.Sprintf("correspondence-%02d", i), Source(SourceCorrespondence)))
	}
	return out
}

const (
	// pageOverflowOverdue is the overdue cohort size. It is above fifty on its
	// own, not only once the rest of the world is added in.
	pageOverflowOverdue = 52
	// pageOverflowFresh are the non-overdue members, so the page the cohort
	// overflows is not trivially all-overdue.
	pageOverflowFresh = 4
)

// pageOverflowEntities is the overflow cohort.
//
// The overdue members reach overdue-ness through created_at + period
// (NeverContacted + CreatedAgo) rather than through OverdueBy, deliberately:
// OverdueBy lowers to a replayed message and therefore a full settle EACH, so
// fifty-two of them would be fifty-two settles. "Added long ago, never
// connected" is both the honest shape for a bulk cohort and free of replays. The
// world still holds genuinely history-backed overdue contacts — from the
// all-cadences edge, the dashboard declarations and the pinned overdue pair.
func pageOverflowEntities() []Entity {
	cadences := Cadences()
	out := make([]Entity, 0, pageOverflowOverdue+pageOverflowFresh)
	for i := 0; i < pageOverflowOverdue; i++ {
		cadence := cadences[i%len(cadences)]
		out = append(out, Contact(
			fmt.Sprintf("overdue-%02d", i),
			Cadence(cadence),
			NeverContacted(),
			CreatedAgo(Periods(2)),
		))
	}
	for i := 0; i < pageOverflowFresh; i++ {
		// No cadence at all, so these can never be overdue however the period
		// tables change.
		out = append(out, Contact(fmt.Sprintf("fresh-%02d", i), NeverContacted(), CreatedAgo(Days(1))))
	}
	return out
}
