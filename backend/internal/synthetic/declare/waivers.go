package declare

// Waivers is the migration debt ledger: ui-surface spec behaviors that are
// resolved neither by a declaration nor by RegisterNone, each with the reason
// it is still owed. Every entry is a promise to a later arc PR; each domain PR
// DELETES its lines as part of its contract, and the completeness test fails on
// a waiver whose behavior no longer exists or is no longer ui-surface.
//
// Target at arc end: only deliberate, reasoned entries (proposed-status
// behaviors and genuinely unseedable states).
var Waivers = map[string]string{
	// spec/dashboard.yaml — proposed behaviors with no implementation and no citing test.
	"DSH-006": "proposed — the failed mark-contacted path is not implemented and has no citing test",
	"DSH-009": "proposed — the stale-flow refresh leak is documented, not implemented",

	// spec/cadence-followup.yaml — 1 behavior; CAD-023/026/028/029/030/031/033
	// are resolved in cadence_domain.go and dashboard.go.
	"CAD-027": "its citing tests replace the whole overdue list with a route mock (the three sort orders are pairwise distinct only over a hand-built fixture), so they provision nothing — a reason-string conversion for the arc #759 waiver-truth pass",

	// spec/calendar.yaml — 8 behaviors.
	"CAL-019": "not yet migrated — calendar (arc #759 calendar+mac PR)",
	"CAL-024": "not yet migrated — calendar (arc #759 calendar+mac PR)",
	"CAL-025": "not yet migrated — calendar (arc #759 calendar+mac PR)",
	"CAL-026": "not yet migrated — calendar (arc #759 calendar+mac PR)",
	"CAL-027": "not yet migrated — calendar (arc #759 calendar+mac PR)",
	"CAL-028": "not yet migrated — calendar (arc #759 calendar+mac PR)",
	"CAL-029": "not yet migrated — calendar (arc #759 calendar+mac PR)",
	"CAL-030": "not yet migrated — calendar (arc #759 calendar+mac PR)",

	// spec/contacts.yaml — 2 behaviors; the other 19 are resolved in contacts.go.
	"CON-039": "proposed — exact tie-break determinism (CON-026) is not implemented and has no citing test",
	"CON-046": "proposed — a failed mark-contacted is console-only and a failed delete is swallowed, so there is no surface to assert and no citing test",

	// spec/imports-matching.yaml — every ui-surface IMP behavior is resolved in
	// imports_domain.go; nothing is owed here.

	// spec/knowledge.yaml — 1 behavior; KNW-034 is resolved in knowledge_domain.go.
	"KNW-035": "its citing test rides CON-045's declared birthday fixture, so this is a reason-string conversion — arc #759 waiver-truth pass",

	// spec/mac-host.yaml — 2 behaviors.
	"MAC-018": "not yet migrated — mac-host (arc #759 calendar+mac PR)",
	"MAC-046": "not yet migrated — mac-host (arc #759 calendar+mac PR)",

	// spec/settings.yaml — 11 behaviors.
	"SET-019": "its citing test provisions nothing at all (route-mocked), so this is a reason-string conversion — arc #759 waiver-truth pass",
	"SET-020": "its citing test provisions nothing at all (route-mocked), so this is a reason-string conversion — arc #759 waiver-truth pass",
	"SET-021": "its citing test provisions nothing at all (route-mocked), so this is a reason-string conversion — arc #759 waiver-truth pass",
	"SET-022": "its citing test provisions nothing at all (route-mocked), so this is a reason-string conversion — arc #759 waiver-truth pass",
	"SET-023": "its citing test provisions nothing at all (route-mocked), so this is a reason-string conversion — arc #759 waiver-truth pass",
	"SET-024": "its citing test provisions nothing at all (route-mocked), so this is a reason-string conversion — arc #759 waiver-truth pass",
	"SET-025": "its citing test provisions nothing at all (route-mocked), so this is a reason-string conversion — arc #759 waiver-truth pass",
	"SET-026": "its citing test provisions nothing at all (route-mocked), so this is a reason-string conversion — arc #759 waiver-truth pass",
	"SET-027": "its citing test provisions nothing at all (route-mocked), so this is a reason-string conversion — arc #759 waiver-truth pass",
	"SET-028": "its citing test provisions nothing at all (route-mocked), so this is a reason-string conversion — arc #759 waiver-truth pass",
	"SET-035": "its citing test provisions nothing at all (route-mocked), so this is a reason-string conversion — arc #759 waiver-truth pass",

	// spec/telegram.yaml — 4 behaviors.
	"TGM-038": "its citing test provisions nothing at all (route-mocked), so this is a reason-string conversion — arc #759 waiver-truth pass",
	"TGM-039": "its citing test provisions nothing at all (route-mocked), so this is a reason-string conversion — arc #759 waiver-truth pass",
	"TGM-040": "its citing test provisions nothing at all (route-mocked), so this is a reason-string conversion — arc #759 waiver-truth pass",
	"TGM-041": "its citing test provisions nothing at all (route-mocked), so this is a reason-string conversion — arc #759 waiver-truth pass",

	// spec/todoist.yaml — 1 behavior; TDS-035 is resolved in cadence_domain.go.
	"TDS-034": "its citing test provisions nothing at all (route-mocked), so this is a reason-string conversion — arc #759 waiver-truth pass",
}
