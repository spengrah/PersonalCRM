package declare

// Waivers is the home for ui-surface spec behaviors that are resolved neither
// by a declaration nor by RegisterNone, each with the reason.
//
// A waiver is not a debt note. It records that a behavior has no implementation
// AND no citing test, so there is nothing for a fixture to provision and nothing
// for a resolution to describe. A behavior whose test merely needs no data of
// its own belongs in RegisterNone with the mechanism named, not here.
//
// The completeness test fails on a waiver whose behavior no longer exists or is
// no longer ui-surface. When a proposed behavior gains an implementation and a
// citing test, its entry moves out of this map in the same PR.
var Waivers = map[string]string{
	// spec/dashboard.yaml
	"DSH-006": "proposed — the failed mark-contacted path is not implemented and has no citing test",
	"DSH-009": "proposed — the stale-flow refresh leak is documented, not implemented",

	// spec/contacts.yaml
	"CON-039": "proposed — exact tie-break determinism (CON-026) is not implemented and has no citing test",
	"CON-046": "proposed — a failed mark-contacted is console-only and a failed delete is swallowed, so there is no surface to assert and no citing test",

	// spec/interactions.yaml
}
