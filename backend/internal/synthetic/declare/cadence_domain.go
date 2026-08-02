package declare

// Cadence-domain resolutions (spec/cadence-followup.yaml), plus TDS-035 — the
// projected-task display behavior whose citing tests live in the same
// contact-tasks spec as the CAD task behaviors, so its resolution is stated
// beside theirs rather than away from the surface it describes.
//
// The file is named cadence_domain.go for the same reason imports_domain.go is:
// a same-stem *_test.go pair would read as this file's test.
//
// The no-fixture registrations are the honest disposition of MOCKED SURFACES,
// but they do not all mock the same one. CAD-030/031/033 and TDS-035 are cited
// by contact-tasks.spec.ts: every task row it asserts over is injected by
// page.route, so a declaration would provision the container contact and could
// be wrong about the task shape without any assertion failing — a resolution
// claiming coverage it does not have. Those tests do seed CON-041's
// one-contact fixture for the page they render on. CAD-027 is different: it is
// cited by dashboard.spec.ts, seeds nothing at all, and mocks the overdue list
// instead. Each reason below states its own mechanism.
func init() {
	RegisterNone("CAD-030", "the empty-state item is the real state of any bare contact, and its test rides CON-041's fixture; the ordering, badge and completed-collapse items are asserted over route-injected task lists, because a live follow-up/manual/completed spread needs a Todoist provider the E2E environment does not have")
	RegisterNone("CAD-031", "the kind-picker and text-validation items are asserted on a bare contact detail page riding CON-041's fixture, with no task mock at all; the created-task item is a route-injected POST plus refetch, because ContactTaskService.CreateManualTask calls the Todoist Quick Add API before it writes anything local")
	RegisterNone("CAD-033", "unlink and the absent complete/dismiss controls are asserted over a route-injected linked row; the CRM-link DELETE is intercepted too, so no seeded task ever participates")
	RegisterNone("TDS-035", "marker stripping and the Todoist deep link are pure rendering over route-injected task content")
	RegisterNone("CAD-027", "the three citing tests sit in a describe whose helper fulfils /contacts/overdue with a hand-built four-card envelope; the sort is client-side over that envelope and the block references no testApi at all")

	// Two overdue contacts whose relative urgency is known at declaration time,
	// so the endpoint's ordering claim is asserted against a named pair rather
	// than against whatever else the shared database holds. The amounts differ by
	// more than the source history lag both of them carry, so the ordering holds
	// in every environment even though the RENDERED day count does not equal the
	// declared one under the compressed cadence table.
	Register(Declaration{
		Behavior: "CAD-023",
		Entities: []Entity{
			Contact("first", Cadence("weekly"), OverdueBy(Days(3))),
			Contact("second", Cadence("monthly"), OverdueBy(Days(10))),
		},
	})

	// A mark-contacted target plus a SENTINEL that stays overdue. The sentinel is
	// the data-derived settle signal for the dashboard's overdue list: its card
	// renders only once the list has rendered from data, so the target's absence
	// can be asserted without racing a loading frame.
	Register(Declaration{
		Behavior: "CAD-028",
		Entities: []Entity{
			Contact("target", Cadence("weekly"), OverdueBy(Days(5))),
			Contact("sentinel", Cadence("weekly"), OverdueBy(Days(4))),
		},
	})

	// One contact per recent-activity state the block can render: a mutual
	// meeting (which bumps outreach AND response), an outbound with no reply
	// (outreach only), an outbound with a live follow-up loop (the awaiting-reply
	// indicator), and a contact with no interactions at all (the explicit
	// no-activity branch). The awaiting contact carries its outbound BEFORE the
	// follow-up because a follow-up loop is opened BY an outbound.
	Register(Declaration{
		Behavior: "CAD-029",
		Entities: []Entity{
			Contact("mutual", Cadence("monthly"), MutualMeeting(Days(3))),
			Contact("outbound-only", Cadence("monthly"), Outreach(Days(2))),
			Contact("awaiting", Cadence("monthly"), Outreach(Days(1)), AwaitingReply()),
			Contact("quiet", Cadence("monthly")),
		},
	})
}
