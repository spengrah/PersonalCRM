package declare

import "time"

// Knowledge-domain resolutions (spec/knowledge.yaml).
func init() {
	RegisterNone("KNW-035", "both then-items are cited inside the test that calls seedBehavior('CON-045'), so they read that declared birthday fixture and need none of their own")

	// One contact per knowledge row the detail page can render, each beside a
	// contact that lacks it — the only shape in which "shown only when known" is
	// falsifiable. Location stores a namespace-PREFIXED label (the auto-created
	// place node has to carry the prefix for teardown's label sweep to find it)
	// and BirthdayOn resolves a leap-safe birth year, so both rendered values are
	// read back from the API rather than restated by the citing test.
	Register(Declaration{
		Behavior: "KNW-034",
		Entities: []Entity{
			Contact("located", Location("Lisbon")),
			Contact("unlocated"),
			Contact("birthday", BirthdayOn(time.April, 12)),
			Contact("plain"),
		},
	})
}
