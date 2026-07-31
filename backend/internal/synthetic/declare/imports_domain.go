package declare

import "fmt"

// Imports-domain resolutions (spec/imports-matching.yaml).
//
// Every ui-surface IMP behavior is resolved here; none remain waived. The file is
// named imports_domain.go rather than imports.go because imports_test.go already
// exists and tests the forbidden-import scanner — a same-stem pair would read as
// this file's test and invite a later agent to put domain tests there.
//
// One declaration per behavior, entities picked by handle. Several IMP behaviors
// are cited by tests with mutually incompatible queue shapes (a 21-row pagination
// queue against a two-row confidence-ranked one), so each declaration carries the
// shape of the test that has no alternative and every other citing test picks the
// handles it needs out of a sibling declaration's manifest.
//
// Auto-matching is generator-safe by construction: two rows collide only where
// SameNameAs / SameEmailAs says so, because every other name and email is
// namespace-derived and distinct. A suggested match needs a confidence of 0.5, and
// a name-only similarity contributes at most 0.6 of it, so sharing a namespace
// prefix is not enough — which is why a plain candidate can sit beside three
// contacts and still open the resolver in import mode.
func init() {
	RegisterNone("IMP-031", "every citing test rides the fixture of the behavior whose surface it exercises (IMP-012/013/026/027/030/037); the claim is that resolving an item invalidates dependent surfaces, which needs no fixture of its own")

	// One unmatched candidate: the row awaiting review, and the row the ignore
	// action turns terminal.
	Register(Declaration{
		Behavior: "IMP-007",
		Entities: []Entity{ExternalCandidate("cand", Source(SourceGContacts))},
	})

	// The two candidate shapes the import path has to create a contact from: an
	// address-book row carrying names and an email, and a telegram peer whose only
	// identity is its handle (so the frontend has to send that handle as the name).
	Register(Declaration{
		Behavior: "IMP-012",
		Entities: []Entity{
			ExternalCandidate("addressbook", Source(SourceGContacts)),
			ExternalCandidate("handle-only", Source(SourceTelegram), NoIdentity(), TelegramHandle()),
		},
	})

	// Two curation-signal pairs. The first links a candidate to an UNRELATED
	// contact, so the link is the explicit user action rather than an accepted
	// suggestion; the second is a name collision, so the resolver opens with the
	// twin already selected and the curation signal is the method deselection.
	Register(Declaration{
		Behavior: "IMP-013",
		Entities: []Entity{
			Contact("plain"),
			ExternalCandidate("unmatched", Source(SourceGContacts)),
			Contact("twin"),
			ExternalCandidate("collides", Source(SourceCalendarAttendee), SameNameAs("twin")),
		},
	})

	// The People tab's whole holding: a pageful-and-one of ordinary candidates, an
	// Anarlog-sourced one for the source pill, and a two-member title-token group.
	Register(Declaration{Behavior: "IMP-026", Entities: importsQueueFixture()})

	// The resolver modal's fixture. Every distinct body state it can open in is
	// here: a plain candidate, a multi-email one (so exactly-one-primary is
	// observable), an UNRESOLVED telegram peer (no name to import), a handle-only
	// telegram peer (whose HEADING is the handle, which is what makes the handle
	// observable as a method rather than as a chip beside a name), two
	// cadence-bearing link targets, and a method-bucket pair whose candidate shares
	// the contact's name AND primary email while adding a second email and a phone.
	Register(Declaration{
		Behavior: "IMP-027",
		Entities: []Entity{
			ExternalCandidate("plain", Source(SourceGContacts)),
			ExternalCandidate("multi-method", Source(SourceGContacts), Emails(2)),
			ExternalCandidate("unresolved-tg", Source(SourceTelegram), NoIdentity()),
			ExternalCandidate("tg-handle", Source(SourceTelegram), NoIdentity(), TelegramHandle()),
			Contact("cadenced", Cadence("quarterly")),
			ExternalCandidate("link-a", Source(SourceGContacts)),
			Contact("cadenced2", Cadence("monthly")),
			ExternalCandidate("link-b", Source(SourceGContacts)),
			Contact("buckets", Methods(MethodEmail)),
			ExternalCandidate("buckets-cand", Source(SourceCalendarAttendee),
				SameNameAs("buckets"), SameEmailAs("buckets"), Emails(2), Phones(1)),
		},
	})

	// Exactly two queued candidates. Two is the whole point: the pager needs a
	// neighbour to move to, and resolving the first must leave one of OURS queued —
	// which is what proves the modal advances instead of closing. A larger queue
	// would push the in-session pager walk past its own step bound.
	Register(Declaration{
		Behavior: "IMP-028",
		Entities: []Entity{
			ExternalCandidate("one", Source(SourceGContacts)),
			ExternalCandidate("two", Source(SourceGContacts)),
		},
	})

	// The suggested-match confidence ladder, plus the two no-suggestion cases.
	// name-collide (name only) is declared BEFORE matching (name AND email) on
	// purpose: entity order is seed order, and the render-order assertion is only
	// meaningful if the lower-confidence row was inserted first.
	Register(Declaration{
		Behavior: "IMP-029",
		Entities: []Entity{
			Contact("name-only"),
			ExternalCandidate("name-collide", Source(SourceCalendarAttendee), SameNameAs("name-only")),
			Contact("matched", Methods(MethodEmail)),
			ExternalCandidate("matching", Source(SourceCalendarAttendee),
				SameNameAs("matched"), SameEmailAs("matched")),
			ExternalCandidate("unmatched-gc", Source(SourceGContacts)),
			ExternalCandidate("unmatched-corr", Source(SourceCorrespondence)),
		},
	})

	// A contact with one pending method suggestion on its linked address-book row:
	// the enrich-locked review surface, whose target is fixed and whose confirm
	// needs at least one selected method.
	Register(Declaration{
		Behavior: "IMP-030",
		Entities: []Entity{
			Contact("target"),
			MethodSuggestion("sugg", "target", SourceGContacts),
		},
	})

	// The two telegram display states: a peer with both a name and a handle (the
	// handle renders as a chip beside the name) and a peer with only a handle (the
	// handle becomes the heading, and the chip is suppressed as redundant).
	Register(Declaration{
		Behavior: "IMP-036",
		Entities: []Entity{
			ExternalCandidate("named", Source(SourceTelegram), TelegramHandle()),
			ExternalCandidate("handle-only", Source(SourceTelegram), NoIdentity(), TelegramHandle()),
		},
	})

	// A correspondence candidate carrying co-occurrence evidence for a contact it
	// shares a name with, so the badge names a real contact and the link has a
	// pre-selected target.
	Register(Declaration{
		Behavior: "IMP-037",
		Entities: []Entity{
			Contact("cooccur"),
			ExternalCandidate("corr", Source(SourceCorrespondence),
				SameNameAs("cooccur"), CorrespondenceEvidence(correspondenceEvidenceMessages, "cooccur")),
		},
	})

	// Two orphaned sessions. Two, so resolving one can be shown to leave the other
	// standing, and so a deep link can name a specific one.
	Register(Declaration{
		Behavior: "IMP-038",
		Entities: []Entity{
			MeetingNote("orphan-a"),
			MeetingNote("orphan-b"),
		},
	})

	// One candidate to open the resolver on and dismiss without resolving.
	Register(Declaration{
		Behavior: "IMP-039",
		Entities: []Entity{ExternalCandidate("cand", Source(SourceGContacts))},
	})

	// A contact and a candidate that share NEITHER a name nor an email, so nothing
	// auto-matches and the link is the deliberate user action whose invalidation of
	// the already-cached contact detail is the claim under test.
	Register(Declaration{
		Behavior: "IMP-040",
		Entities: []Entity{
			Contact("target", Methods(MethodEmail)),
			ExternalCandidate("cand", Source(SourceGContacts)),
		},
	})
}

// importsPaginationFixtureSize is one row past the twenty-row page size, so page 2
// of the candidate queue is non-empty and the pager's controls render.
const importsPaginationFixtureSize = 21

// importsTitleTokenGroup is the token the two anarlog_title rows share. The
// lowering prefixes it with the namespace, which is load-bearing rather than
// cosmetic: the discovery surface groups by normalized token DB-WIDE, so two
// namespaces sharing a bare token would land in one grouped row and each would see
// the other's evidence in its count.
const importsTitleTokenGroup = "lena"

// correspondenceEvidenceMessages is the aggregated message count the evidence
// badge renders. The citing test asserts the rendered copy, so the number has to
// be stated once and read from here rather than restated on both sides.
const correspondenceEvidenceMessages = 4

// importsQueueFixture is the People tab's holding: a link target for the token
// group's link-to-existing path, a pageful-and-one of ordinary address-book
// candidates, one Anarlog-sourced candidate for the source pill, and two
// anarlog_title rows sharing a token so they group into ONE row with an evidence
// count of two.
//
// The contact comes first because a declaration's entities run in order and a
// reference can only name an earlier handle. The candidate handles are numbered
// only so a manifest read is legible; their rendered names stay generator-derived.
func importsQueueFixture() []Entity {
	entities := make([]Entity, 0, importsPaginationFixtureSize+4)
	entities = append(entities, Contact("link-target"))
	for i := 1; i <= importsPaginationFixtureSize; i++ {
		entities = append(entities, ExternalCandidate(fmt.Sprintf("p%02d", i), Source(SourceGContacts)))
	}
	return append(entities,
		ExternalCandidate("anarlog", Source(SourceAnarlogHumans)),
		ExternalCandidate("token-a", Source(SourceAnarlogTitle), TitleToken(importsTitleTokenGroup)),
		ExternalCandidate("token-b", Source(SourceAnarlogTitle), TitleToken(importsTitleTokenGroup)),
	)
}
