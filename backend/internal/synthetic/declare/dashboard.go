package declare

// Dashboard-domain resolutions (spec/dashboard.yaml), plus CAD-026 — the
// overdue-cards behavior the dashboard's own seeded describe provisions, pulled
// forward from the cadence domain because the test that CITES it is the test
// that seeds for it.
//
// Every ui-surface DSH behavior is resolved here or waived in waivers.go; the
// completeness test enforces that there is no fourth state.
func init() {
	RegisterNone("DSH-001", "landing redirect — the behavior is the navigation itself, there is no data shape to seed")
	RegisterNone("DSH-002", "static navigation surface — navigation.spec.ts asserts it with no seeded data")
	RegisterNone("DSH-003", "the header add-contact CTA is observable in every dashboard state; its populated-state test rides DSH-005's fixture and its caught-up state is a route-mocked global-emptiness case")
	RegisterNone("DSH-004", "loading and error states are route-injected by the test, not reachable by seeding data")
	RegisterNone("DSH-007", "absence claim — the assertion is that no search surface exists, which needs no seeded data")

	// The overdue list refreshing in place after an interaction is recorded:
	// a mark-contacted target plus a sentinel that STAYS overdue, so the
	// dashboard keeps rendering its numeric header after the target leaves.
	Register(Declaration{
		Behavior: "DSH-005",
		Entities: []Entity{
			Contact("refresh-target", Cadence("weekly"), OverdueBy(Days(5))),
			Contact("refresh-sentinel", Cadence("weekly"), OverdueBy(Days(3))),
		},
	})

	// Three graded-overdue contacts so the cards render with distinct urgency
	// values and the header count can be compared against the rendered cards.
	Register(Declaration{
		Behavior: "CAD-026",
		Entities: []Entity{
			Contact("card-a", Cadence("weekly"), OverdueBy(Days(3))),
			Contact("card-b", Cadence("weekly"), OverdueBy(Days(4))),
			Contact("card-c", Cadence("weekly"), OverdueBy(Days(5))),
		},
	})
}
