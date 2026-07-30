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

	// spec/cadence-followup.yaml — 7 behaviors.
	"CAD-023": "not yet migrated — cadence-followup (arc #759 PR5)",
	"CAD-027": "not yet migrated — cadence-followup (arc #759 PR5)",
	"CAD-028": "not yet migrated — cadence-followup (arc #759 PR5)",
	"CAD-029": "not yet migrated — cadence-followup (arc #759 PR5)",
	"CAD-030": "not yet migrated — cadence-followup (arc #759 PR5)",
	"CAD-031": "not yet migrated — cadence-followup (arc #759 PR5)",
	"CAD-033": "not yet migrated — cadence-followup (arc #759 PR5)",

	// spec/calendar.yaml — 8 behaviors.
	"CAL-019": "not yet migrated — calendar (arc #759 PR5)",
	"CAL-024": "not yet migrated — calendar (arc #759 PR5)",
	"CAL-025": "not yet migrated — calendar (arc #759 PR5)",
	"CAL-026": "not yet migrated — calendar (arc #759 PR5)",
	"CAL-027": "not yet migrated — calendar (arc #759 PR5)",
	"CAL-028": "not yet migrated — calendar (arc #759 PR5)",
	"CAL-029": "not yet migrated — calendar (arc #759 PR5)",
	"CAL-030": "not yet migrated — calendar (arc #759 PR5)",

	// spec/contacts.yaml — 4 behaviors; the other 17 are resolved in contacts.go.
	"CON-039": "proposed — exact tie-break determinism (CON-026) is not implemented and has no citing test",
	"CON-046": "proposed — a failed mark-contacted is console-only and a failed delete is swallowed, so there is no surface to assert and no citing test",
	"CON-054": "the citing test resolves three of its four fixtures by a marker token shared across their DRAWN names, and the factory's display-name dedupe keys on the marker-INCLUSIVE string: a marked contact that draws an unmarked one's given+surname pair gets no disambiguator, so one rendered name nests inside the other and a substring locator resolves both rows. Declaring it needs the shared name composition changed, which a spec migration is the wrong place to do",
	"CON-056": "the citing test needs a gchat method AND a non-default (telegram) primary; factory.Contact() has no generic method-kind builder and always makes email primary when one is present, so this needs a factory extension",

	// spec/imports-matching.yaml — 14 behaviors.
	"IMP-007": "not yet migrated — imports-matching (arc #759 PR3)",
	"IMP-012": "not yet migrated — imports-matching (arc #759 PR3)",
	"IMP-013": "not yet migrated — imports-matching (arc #759 PR3)",
	"IMP-026": "not yet migrated — imports-matching (arc #759 PR3)",
	"IMP-027": "not yet migrated — imports-matching (arc #759 PR3)",
	"IMP-028": "not yet migrated — imports-matching (arc #759 PR3)",
	"IMP-029": "not yet migrated — imports-matching (arc #759 PR3)",
	"IMP-030": "not yet migrated — imports-matching (arc #759 PR3)",
	"IMP-031": "not yet migrated — imports-matching (arc #759 PR3)",
	"IMP-036": "not yet migrated — imports-matching (arc #759 PR3)",
	"IMP-037": "not yet migrated — imports-matching (arc #759 PR3)",
	"IMP-038": "not yet migrated — imports-matching (arc #759 PR3)",
	"IMP-039": "not yet migrated — imports-matching (arc #759 PR3)",
	"IMP-040": "not yet migrated — imports-matching (arc #759 PR3)",

	// spec/knowledge.yaml — 2 behaviors.
	"KNW-034": "not yet migrated — knowledge (arc #759 PR6)",
	"KNW-035": "not yet migrated — knowledge (arc #759 PR6)",

	// spec/mac-host.yaml — 2 behaviors.
	"MAC-018": "not yet migrated — mac-host (arc #759 PR6)",
	"MAC-046": "not yet migrated — mac-host (arc #759 PR6)",

	// spec/notes-meetings.yaml — 2 behaviors.
	"NTS-007": "not yet migrated — notes-meetings (arc #759 PR6)",
	"NTS-008": "not yet migrated — notes-meetings (arc #759 PR6)",

	// spec/settings.yaml — 11 behaviors.
	"SET-019": "not yet migrated — settings (arc #759 PR4)",
	"SET-020": "not yet migrated — settings (arc #759 PR4)",
	"SET-021": "not yet migrated — settings (arc #759 PR4)",
	"SET-022": "not yet migrated — settings (arc #759 PR4)",
	"SET-023": "not yet migrated — settings (arc #759 PR4)",
	"SET-024": "not yet migrated — settings (arc #759 PR4)",
	"SET-025": "not yet migrated — settings (arc #759 PR4)",
	"SET-026": "not yet migrated — settings (arc #759 PR4)",
	"SET-027": "not yet migrated — settings (arc #759 PR4)",
	"SET-028": "not yet migrated — settings (arc #759 PR4)",
	"SET-035": "not yet migrated — settings (arc #759 PR4)",

	// spec/telegram.yaml — 4 behaviors.
	"TGM-038": "not yet migrated — telegram (arc #759 PR6)",
	"TGM-039": "not yet migrated — telegram (arc #759 PR6)",
	"TGM-040": "not yet migrated — telegram (arc #759 PR6)",
	"TGM-041": "not yet migrated — telegram (arc #759 PR6)",

	// spec/todoist.yaml — 2 behaviors.
	"TDS-034": "not yet migrated — todoist (arc #759 PR6)",
	"TDS-035": "not yet migrated — todoist (arc #759 PR6)",
}
