package synthetic

import (
	"context"
	"fmt"
	"time"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/synthetic/factory"

	"github.com/google/uuid"
)

// Profile names the synthetic world a seed entrypoint builds. It is pure
// orchestration over the existing factory + replay + meeting-note repo — no new
// replay/factory machinery.
type Profile string

const (
	// ProfileMinimalScoped is the smallest viable world: the existing SeedAll
	// shape (a few contacts, one Gmail-settled + one Telegram-settled each). It
	// is the CI/E2E namespacing baseline and the golden-scenario regression
	// pins its shape, so it MUST stay byte-stable == today's SeedAll behavior.
	ProfileMinimalScoped Profile = "minimal-scoped"
	// ProfileDev is a richer-than-minimal but fast local world spanning the
	// contact edge-case catalog + a settled interaction per source + a handful
	// of pending states, so every local UI surface has content.
	ProfileDev Profile = "dev"
	// ProfileProdShaped is the staging / #380 world: hundreds of contacts with a
	// prod-shaped distribution, the full contact-level edge-case catalog, and a
	// representative slice of the PRODUCIBLE pending states. Replay-heavy; slow
	// is fine (one-time staging seed).
	ProfileProdShaped Profile = "prod-shaped"
)

// ProfileResult is the counts-only summary a seed run reports (no PII). The
// crm-admin entrypoints print it; the QA harness (#380) parses it (or checks
// exit 0).
type ProfileResult struct {
	Profile           Profile
	Namespace         string
	Seed              uint64
	Contacts          int
	GmailSettled      int
	TelegramSettled   int
	GCalSettled       int
	IMessageSettled   int
	GChatSettled      int
	UnmatchedExternal int
	StrandedTelegram  int
	StrandedMessages  int
	UnmatchedCalendar int
	OrphanMeetingNote int
	SeededAssertions  int
	// ContactsWithBirthday / ContactsWithHowMet / ContactsWithLocation count the
	// catalog contacts that carried a birthday / how_met / location bio fact
	// (produced through the contact-create authority-flip path, not the Phase-4
	// assertion loop). A location additionally mints a place entity node + a
	// `lives_in` edge. Counts-only / no PII.
	ContactsWithBirthday int
	ContactsWithHowMet   int
	ContactsWithLocation int
	// SeededTasks counts the contact_task rows the profile seeded — one `managed`
	// cadence task per cadence-bearing catalog contact (via ReplayTodoist's
	// reconcile), a deterministic three of which are then transitioned to the
	// completed/dismissed/unmanaged surface states. The state-transitions do not
	// change the row count, so this is the total cadence-task population.
	SeededTasks int
	// SeededBoolFacts / SeededRelationships / SeededDateFacts count the graph rows
	// the value-type + edge plumbing seeded: bool facts (job_seeking etc.) on
	// catalog person nodes, person→person edge assertions among catalog person
	// nodes, and the single toolkit-authored date fact (a birthday asserted
	// directly through the assert path). Counts-only / no PII.
	SeededBoolFacts     int
	SeededRelationships int
	SeededDateFacts     int
	// SeededEntities / SeededEntityEdges count the graph rows the entity layer
	// seeded: the org/topic/tag entity nodes in the pool, and the person→entity edge
	// assertions (works_at/interested_in/tagged_as) drawn from that pool. Counts-only
	// / no PII. (The place entity nodes + lives_in edges from WithLocation are
	// reflected by ContactsWithLocation, not here.)
	SeededEntities    int
	SeededEntityEdges int
	// SeededSignals counts the relationship_signal rows the profile seeded across
	// catalog person nodes (one per node, keys from a small fixed pool). These are
	// SP1 derived storage (per-node scalars), written through the production upsert
	// path. Counts-only / no PII.
	SeededSignals int
	// SettledInteractions counts the settled source replays the profile drove on the
	// dedicated per-source contacts — MessagesPerContact messages each, across the
	// five sources — so the staging history is multi-interaction and spread over
	// time rather than one message per contact. It is the replay-call count, the
	// upper bound on the interaction rows produced (same-day messages on one contact
	// would aggregate, but the spread lands them on distinct days). Counts-only / no PII.
	SettledInteractions int
	// SeededSoftDeleted / SeededMerged count the merge + soft-delete scenarios the
	// profile seeded: soft-deleted contacts (person node tombstoned via DeleteContact,
	// assertion dropped from live reads but retained in the table) and merged pairs
	// (loser node tombstoned via MergeContacts, its assertions re-pointed onto the
	// winner). SeededMerged is the PAIR count (each pair is two contacts). Counts-only
	// / no PII.
	SeededSoftDeleted int
	SeededMerged      int
}

// ProfileParams returns the default SeedParams for a named profile (error on an
// unknown name, including the empty string). The caller may override Namespace /
// Seed / Counts before passing to RunProfile.
func ProfileParams(name Profile) (SeedParams, error) {
	switch name {
	case ProfileMinimalScoped:
		p := DefaultParams()
		// DefaultParams is already minimal-scoped; keep it byte-stable.
		return p, nil
	case ProfileDev:
		return SeedParams{
			Namespace: "dev",
			Seed:      factory.DefaultSeed,
			Profile:   ProfileDev,
			Counts: Counts{
				SeededContacts:    18,
				UnmatchedExternal: 2,
				StrandedTelegram:  1,
				StrandedMessages:  1,
				UnmatchedCalendar: 1,
				OrphanMeetingNote: 1,
				SeededAssertions:  2,
				// Bool facts + person→person edges (small, fast) so the dev graph
				// surfaces have content. Both exercise the value-type/edge assert
				// plumbing; the bool-fact gate also seeds one toolkit date fact.
				SeededBoolFacts:     2,
				SeededRelationships: 2,
				// Enable cadence-task seeding (>0 gate). The dev catalog is all
				// cadence-bearing, so reconcile creates a managed task on each and
				// three are transitioned for surface coverage.
				SeededTasks: 1,
				// Small entity pool (1 org + 1 topic + 1 tag) + a few person→entity
				// edges so the dev graph surfaces show works_at/interested_in/tagged_as.
				SeededEntities:    3,
				SeededEntityEdges: 6,
				// A few relationship_signal rows (SP1 derived storage) so the dev graph
				// has per-node signals. One signal per node, rotating the signal-key pool.
				SeededSignals: 6,
				// Two settled interactions per dedicated source contact, spread over time,
				// so the dev graph shows a small multi-interaction history (fast: the dev
				// per-source settled count is 1).
				MessagesPerContact: 2,
				// One soft-deleted contact + one merged pair so the dev graph surfaces the
				// tombstone + merge re-point scenarios.
				SeededSoftDeleted: 1,
				SeededMerged:      1,
			},
		}, nil
	case ProfileProdShaped:
		return SeedParams{
			Namespace: "prodshaped",
			Seed:      factory.DefaultSeed,
			Profile:   ProfileProdShaped,
			Counts: Counts{
				SeededContacts:    150,
				UnmatchedExternal: 12,
				StrandedTelegram:  6,
				StrandedMessages:  5,
				UnmatchedCalendar: 7,
				OrphanMeetingNote: 4,
				// ~1/3 of 150 catalog contacts carry a bio text fact (the
				// Phase-4 spread cycles home_address/job_title/preference/
				// health_condition across distinct contacts). The coverage test
				// overrides this down to a CI-safe minimum.
				SeededAssertions: 50,
				// Prod-shaped graph slice: ~1/10 of the catalog carry a bool fact and
				// ~12 person→person edges connect the catalog (knows/introduced_by +
				// a few family edges). Bounded by the catalog size at run time; the
				// coverage + determinism tests override these down for CI.
				SeededBoolFacts:     15,
				SeededRelationships: 12,
				// Enable cadence-task seeding (>0 gate). The prod-shaped catalog is
				// all cadence-bearing, so reconcile creates a managed cadence task on
				// each (catalog-wide, like prod) and three are transitioned to the
				// completed/dismissed/unmanaged surface states.
				SeededTasks: 1,
				// Small org/topic/tag pool (3 each) + ~1/5 of the catalog carrying a
				// person→entity edge (works_at/interested_in/tagged_as), drawn from the
				// pool so the few entities repeat across many people (prod-like). The
				// coverage + determinism tests override these down for CI.
				SeededEntities:    9,
				SeededEntityEdges: 30,
				// ~20 relationship_signal rows (SP1 derived storage) across a subset of
				// the catalog person nodes, one signal per node from a small fixed
				// signal-key pool (a few signal kinds repeat across people, prod-like).
				// The coverage + determinism tests override this down for CI.
				SeededSignals: 20,
				// Four settled interactions per dedicated source contact, spread back over
				// ~9 weeks, so staging shows a prod-like multi-interaction history per
				// source instead of a single recent message. The coverage + determinism
				// tests override this down for CI runtime.
				MessagesPerContact: 4,
				// A handful of soft-deleted contacts + merged pairs so staging shows the
				// tombstoned-contact and merge re-point surfaces. The coverage + determinism
				// tests override these down for CI.
				SeededSoftDeleted: 3,
				SeededMerged:      3,
			},
		}, nil
	default:
		return SeedParams{}, fmt.Errorf("synthetic: unknown profile %q (want one of %q, %q, %q)",
			name, ProfileMinimalScoped, ProfileDev, ProfileProdShaped)
	}
}

// RunProfile builds the selected profile's world against the harness. It is the
// single profile definition both crm-admin --seed and --reset-and-seed call; no
// HTTP handler consumes it (the profile world is CLI-only).
//
// The harness must be constructed for the SAME namespace + seed as params so the
// run's identifiers + cleanup scope line up (use NewHarnessWithDBForNamespace).
// RunProfile only SEEDS; the caller decides the lifecycle — Quiesce on success
// (seed-and-leave), the teardown closure on error (stop client + clean the
// partial world).
func RunProfile(ctx context.Context, h *Harness, params SeedParams) (ProfileResult, error) {
	res := ProfileResult{Profile: params.Profile, Namespace: h.Namespace(), Seed: params.Seed}
	switch params.Profile {
	case ProfileMinimalScoped:
		return runMinimalScoped(ctx, h, params, res)
	case ProfileDev, ProfileProdShaped:
		return runCatalogProfile(ctx, h, params, res)
	default:
		return res, fmt.Errorf("synthetic: RunProfile: unknown profile %q", params.Profile)
	}
}

// runMinimalScoped is exactly today's SeedAll (the byte-stable golden shape).
func runMinimalScoped(ctx context.Context, h *Harness, params SeedParams, res ProfileResult) (ProfileResult, error) {
	seedRes, err := SeedAll(ctx, h, params)
	if err != nil {
		return res, err
	}
	res.Contacts = len(seedRes.GmailContactIDs) + len(seedRes.TelegramContactIDs)
	res.GmailSettled = len(seedRes.GmailContactIDs)
	res.TelegramSettled = len(seedRes.TelegramContactIDs)
	return res, nil
}

// runCatalogProfile builds the dev / prod-shaped worlds: the full contact-level
// edge-case catalog at the requested volume, a settled interaction per source,
// and the PRODUCIBLE pending states. The two profiles differ only in volume
// (params.Counts); the orchestration is shared.
//
// PRODUCIBLE pending states (in scope): unmatched external_contact (Imports
// queue, via ReplayMacContacts MatchUnknown), stranded telegram_message
// (ReplayTelegram MatchUnknown), stranded messages_message (ReplayIMessage
// MatchUnknown), unmatched calendar attendee (ReplayGCal MatchUnknown), orphan
// meeting_note (existing meeting-note repo).
//
// DEFERRED coverage gap (NO toolkit producer today — documented, asserted-absent
// by the coverage check, tracked for a follow-on): conflict_pending meeting_note
// (needs a conflict_candidates snapshot referencing real events), title-candidate
// review rows (anarlog_title discovery), and comms_message without interaction_id
// (GChat MatchUnknown is match-only, writes no comms_message). Building these is a
// factory/replay addition deferred to a follow-on.
func runCatalogProfile(ctx context.Context, h *Harness, params SeedParams, res ProfileResult) (ProfileResult, error) {
	gen := h.Generator()
	n := params.Counts.SeededContacts
	if n <= 0 {
		n = 1
	}

	// --- Contact edge-case catalog (deterministic spread across n) ---------
	// These contacts carry the cadence / recency / birthday / name / no-method
	// edge-case shapes the UI + QA tour exercise. They are seeded WITHOUT a
	// settling MatchSeeded replay ON PURPOSE: a settled inbound interaction would
	// make the cadence updater overwrite last_contacted (destroying the overdue /
	// never-contacted intent), and every contact would carry a method (no
	// no-method bucket). The settled per-source interactions live on the
	// dedicated contacts below instead.
	var firstCatalogContactID uuid.UUID
	catalogContactIDs := make([]uuid.UUID, 0, n)
	// cadenceBearingCatalogIDs is the subset whose spec carries a cadence —
	// CreateContact auto-computes contact_by from cadence, making them eligible for
	// ReplayTodoist's reconcile (which seeds one `managed` cadence task per such
	// contact). Tracked off the spec rather than assumed so a future catalog change
	// that drops cadence from a slot is reflected in res.SeededTasks.
	cadenceBearingCatalogIDs := make([]uuid.UUID, 0, n)
	// catalogContactsWithoutBirthday is the subset that carries NO contact-path
	// birthday, so the toolkit-authored date fact (a `birthday`, single-cardinality)
	// can occupy a fresh slot rather than superseding a contact-create birthday.
	catalogContactsWithoutBirthday := make([]uuid.UUID, 0, n)
	for i := 0; i < n; i++ {
		opts := catalogOptionsFor(i, n, gen.Anchor(), gen.Prefix())
		spec := gen.Contact(opts...)
		contact, err := h.SeedContact(ctx, spec)
		if err != nil {
			return res, fmt.Errorf("profile %s: seed catalog contact %d: %w", params.Profile, i, err)
		}
		res.Contacts++
		// Bio facts ride on the contact-create authority flip (SeedContact →
		// CreateContact → birthday/how_met assertions); count them off the spec so
		// the coverage check can assert the bio surfaces are populated.
		if spec.Birthday != nil {
			res.ContactsWithBirthday++
		} else {
			catalogContactsWithoutBirthday = append(catalogContactsWithoutBirthday, contact.ID)
		}
		if spec.HowMet != nil {
			res.ContactsWithHowMet++
		}
		if spec.Location != nil {
			res.ContactsWithLocation++
		}
		catalogContactIDs = append(catalogContactIDs, contact.ID)
		if spec.Cadence != nil && *spec.Cadence != "" {
			cadenceBearingCatalogIDs = append(cadenceBearingCatalogIDs, contact.ID)
		}
		if i == 0 {
			firstCatalogContactID = contact.ID
		}
	}

	// Give the catalog its "≥1 contact with notes" bucket (a notepad note on the
	// first catalog contact, via the existing note repo).
	if firstCatalogContactID != uuid.Nil {
		if err := h.SeedNote(ctx, firstCatalogContactID, "catalog notepad note"); err != nil {
			return res, fmt.Errorf("profile %s: seed catalog note: %w", params.Profile, err)
		}
	}

	// A few settled interactions on DEDICATED contacts per source so every source
	// surface has matched content WITHOUT clobbering the catalog's cadence states.
	// Each settled contact carries a recent last_contacted (its newest message),
	// which is the correct "recently contacted via X" representation.
	//
	// Each contact is replayed messagesPer times with growing per-message ages
	// (interactionSpreadAge), so its interactions land on distinct days spread back
	// over time (a prod-like history) instead of one ~1h window. The newest message
	// (age 0) drives last_contacted; older ones never move it back (the consumers'
	// forward-only occurred_at guard), so the "recently contacted" state holds while
	// the contact also carries a deeper history. The spread targets ONLY these
	// dedicated contacts — the edge-case catalog above stays interaction-free so its
	// overdue / never-contacted / no-method buckets survive.
	perSource := perSourceSettledCount(params.Profile)
	messagesPer := params.Counts.MessagesPerContact
	if messagesPer < 1 {
		messagesPer = 1
	}

	for i := 0; i < perSource; i++ {
		// Gmail-settled (email match).
		gmSpec := gen.Contact(factory.WithEmail())
		gmContact, err := h.SeedContact(ctx, gmSpec)
		if err != nil {
			return res, fmt.Errorf("profile %s: seed gmail contact %d: %w", params.Profile, i, err)
		}
		res.Contacts++
		for j := 0; j < messagesPer; j++ {
			if _, err := h.ReplayGmail(ctx, gmContact.ID, gen.GmailMessage(gmSpec, factory.MatchSeeded, factory.WithMessageAge(interactionSpreadAge(j)))); err != nil {
				return res, fmt.Errorf("profile %s: replay gmail %d msg %d: %w", params.Profile, i, j, err)
			}
			res.SettledInteractions++
		}
		res.GmailSettled++

		// Telegram-settled (handle match).
		tgSpec := gen.Contact(factory.WithTelegram())
		tgContact, err := h.SeedContact(ctx, tgSpec)
		if err != nil {
			return res, fmt.Errorf("profile %s: seed telegram contact %d: %w", params.Profile, i, err)
		}
		res.Contacts++
		for j := 0; j < messagesPer; j++ {
			if _, err := h.ReplayTelegram(ctx, tgContact.ID, gen.TelegramMessage(tgSpec, factory.MatchSeeded, factory.WithMessageAge(interactionSpreadAge(j)))); err != nil {
				return res, fmt.Errorf("profile %s: replay telegram %d msg %d: %w", params.Profile, i, j, err)
			}
			res.SettledInteractions++
		}
		res.TelegramSettled++

		// GCal-settled (email match).
		gcSpec := gen.Contact(factory.WithEmail())
		gcContact, err := h.SeedContact(ctx, gcSpec)
		if err != nil {
			return res, fmt.Errorf("profile %s: seed gcal contact %d: %w", params.Profile, i, err)
		}
		res.Contacts++
		for j := 0; j < messagesPer; j++ {
			if _, err := h.ReplayGCal(ctx, gcContact.ID, gen.GCalEvent(gcSpec, factory.MatchSeeded, factory.WithMessageAge(interactionSpreadAge(j)))); err != nil {
				return res, fmt.Errorf("profile %s: replay gcal %d msg %d: %w", params.Profile, i, j, err)
			}
			res.SettledInteractions++
		}
		res.GCalSettled++

		// GChat-settled (email match).
		gchatSpec := gen.Contact(factory.WithEmail())
		gchatContact, err := h.SeedContact(ctx, gchatSpec)
		if err != nil {
			return res, fmt.Errorf("profile %s: seed gchat contact %d: %w", params.Profile, i, err)
		}
		res.Contacts++
		for j := 0; j < messagesPer; j++ {
			if _, err := h.ReplayGChat(ctx, gchatContact.ID, gen.GChatMessage(gchatSpec, factory.MatchSeeded, factory.WithMessageAge(interactionSpreadAge(j)))); err != nil {
				return res, fmt.Errorf("profile %s: replay gchat %d msg %d: %w", params.Profile, i, j, err)
			}
			res.SettledInteractions++
		}
		res.GChatSettled++

		// iMessage-settled (phone match).
		imSpec := gen.Contact(factory.WithPhone())
		imContact, err := h.SeedContact(ctx, imSpec)
		if err != nil {
			return res, fmt.Errorf("profile %s: seed imessage contact %d: %w", params.Profile, i, err)
		}
		res.Contacts++
		for j := 0; j < messagesPer; j++ {
			imageSpec, err := gen.IMessage(imSpec, factory.MatchSeeded, h.MacHostID(), factory.WithMessageAge(interactionSpreadAge(j)))
			if err != nil {
				return res, fmt.Errorf("profile %s: build imessage spec %d msg %d: %w", params.Profile, i, j, err)
			}
			if _, err := h.ReplayIMessage(ctx, imContact.ID, imageSpec); err != nil {
				return res, fmt.Errorf("profile %s: replay imessage %d msg %d: %w", params.Profile, i, j, err)
			}
			res.SettledInteractions++
		}
		res.IMessageSettled++
	}

	// --- PRODUCIBLE pending states -----------------------------------------
	// Unmatched external_contact (Imports queue) via ReplayMacContacts MatchUnknown.
	for i := 0; i < params.Counts.UnmatchedExternal; i++ {
		spec, err := gen.MacContact(gen.Contact(factory.WithEmail()), factory.MatchUnknown, h.MacHostID())
		if err != nil {
			return res, fmt.Errorf("profile %s: build unmatched mac contact %d: %w", params.Profile, i, err)
		}
		if _, err := h.ReplayMacContacts(ctx, uuid.Nil, spec); err != nil {
			return res, fmt.Errorf("profile %s: replay unmatched mac contact %d: %w", params.Profile, i, err)
		}
		res.UnmatchedExternal++
	}
	// Stranded telegram_message via ReplayTelegram MatchUnknown.
	for i := 0; i < params.Counts.StrandedTelegram; i++ {
		spec := gen.TelegramMessage(gen.Contact(factory.WithTelegram()), factory.MatchUnknown)
		if _, err := h.ReplayTelegram(ctx, uuid.Nil, spec); err != nil {
			return res, fmt.Errorf("profile %s: replay stranded telegram %d: %w", params.Profile, i, err)
		}
		res.StrandedTelegram++
	}
	// Stranded messages_message via ReplayIMessage MatchUnknown.
	for i := 0; i < params.Counts.StrandedMessages; i++ {
		spec, err := gen.IMessage(gen.Contact(factory.WithPhone()), factory.MatchUnknown, h.MacHostID())
		if err != nil {
			return res, fmt.Errorf("profile %s: build stranded imessage %d: %w", params.Profile, i, err)
		}
		if _, err := h.ReplayIMessage(ctx, uuid.Nil, spec); err != nil {
			return res, fmt.Errorf("profile %s: replay stranded imessage %d: %w", params.Profile, i, err)
		}
		res.StrandedMessages++
	}
	// Unmatched calendar attendee via ReplayGCal MatchUnknown.
	for i := 0; i < params.Counts.UnmatchedCalendar; i++ {
		spec := gen.GCalEvent(gen.Contact(factory.WithEmail()), factory.MatchUnknown)
		if _, err := h.ReplayGCal(ctx, uuid.Nil, spec); err != nil {
			return res, fmt.Errorf("profile %s: replay unmatched calendar %d: %w", params.Profile, i, err)
		}
		res.UnmatchedCalendar++
	}
	// Orphan meeting_note via the existing meeting-note repo.
	for i := 0; i < params.Counts.OrphanMeetingNote; i++ {
		if _, err := h.SeedOrphanMeetingNote(ctx,
			fmt.Sprintf("synthetic orphan note %d", i),
			"synthetic orphan meeting summary"); err != nil {
			return res, fmt.Errorf("profile %s: seed orphan meeting note %d: %w", params.Profile, i, err)
		}
		res.OrphanMeetingNote++
	}

	// Graph (SP1) text-fact assertions spread across the catalog person nodes
	// (node.id == contact.id). Seeded LAST so the generator-PRNG advancement these
	// FactAssertion calls cause does not shift the deterministic peer-id / handle
	// sequence the earlier source replays depend on (a mid-sequence insert would
	// collide a "stranded" peer with a seeded contact's identity).
	//
	// The predicate cycle covers BOTH review surfaces (D6): home_address /
	// job_title / preference are auto-if-confident → accepted; health_condition and
	// occurrence are always-confirm → they land proposed/pending. health_condition
	// leads the cycle so even a single seeded assertion exercises the
	// pending-review surface. Each FactAssertion produces a distinct value +
	// proposition_key, and successive assertions land on distinct contacts
	// (i % len), so the single-cardinality predicates (home_address/job_title)
	// never supersede a same-subject prior.
	if len(catalogContactIDs) > 0 {
		for i := 0; i < params.Counts.SeededAssertions; i++ {
			subject := catalogContactIDs[i%len(catalogContactIDs)]
			predicate := catalogTextFactPredicates[i%len(catalogTextFactPredicates)]
			if _, err := h.ReplayAssertion(ctx, subject, gen.FactAssertion(predicate)); err != nil {
				return res, fmt.Errorf("profile %s: replay assertion %d: %w", params.Profile, i, err)
			}
			res.SeededAssertions++
		}
	}

	// Graph (SP1) value-type + edge assertions. These ride the SAME "seeded LAST"
	// rule as the text-fact spread above: the gen.BoolFact / gen.DateFact /
	// gen.EdgeAssertion constructors bump the generator's sourceIDSeq, so they run
	// AFTER the source replays whose ids depend on the sequence. (They draw NO name
	// PRNG — only sourceIDSeq — so they cannot shift the name/handle stream; the
	// determinism fingerprint guards any drift regardless.)
	//
	// Bool facts (job_seeking/on_sabbatical/traveling) on distinct catalog person
	// nodes — all auto-if-confident → accepted. Bounded by the catalog size so a
	// distinct subject per fact keeps the single-cardinality bool predicates from
	// superseding one another.
	boolFacts := params.Counts.SeededBoolFacts
	if boolFacts > len(catalogContactIDs) {
		boolFacts = len(catalogContactIDs)
	}
	for i := 0; i < boolFacts; i++ {
		subject := catalogContactIDs[i]
		predicate := catalogBoolFactPredicates[i%len(catalogBoolFactPredicates)]
		if _, err := h.ReplayAssertion(ctx, subject, gen.BoolFact(predicate, true)); err != nil {
			return res, fmt.Errorf("profile %s: replay bool fact %d: %w", params.Profile, i, err)
		}
		res.SeededBoolFacts++
	}

	// One toolkit-authored date fact (a `birthday`) asserted DIRECTLY through the
	// assert path (distinct from the contact-create authority-flip birthday path),
	// to exercise the new ValueDate plumbing end-to-end. Seeded on a catalog
	// contact that carries no contact-path birthday so it occupies a fresh
	// single-cardinality slot. Gated with the bool facts (both demonstrate the new
	// value-type routing). Anchor-relative so no time.Now().
	if boolFacts > 0 && len(catalogContactsWithoutBirthday) > 0 {
		bday := time.Date(gen.Anchor().Year()-40, time.April, 12, 0, 0, 0, 0, time.UTC)
		if _, err := h.ReplayAssertion(ctx, catalogContactsWithoutBirthday[0], gen.DateFact("birthday", bday)); err != nil {
			return res, fmt.Errorf("profile %s: replay date fact: %w", params.Profile, err)
		}
		res.SeededDateFacts++
	}

	// Person→person EDGE assertions among already-seeded catalog person nodes (no
	// new entity nodes — those are a later PR). knows/introduced_by are
	// auto-if-confident → accepted; sibling_of is always-confirm → proposed, so the
	// spread covers BOTH the accepted and pending review surfaces. The object is the
	// NEXT catalog contact (wrapping), so there is never a self-edge; bounded by the
	// catalog size so each subject is distinct (the single-cardinality introduced_by
	// never supersedes). Both endpoints are person nodes, so the existing per-node
	// assertion teardown sweep (subject OR object) already removes them.
	relationships := params.Counts.SeededRelationships
	if len(catalogContactIDs) < 2 {
		relationships = 0 // an edge needs two distinct nodes
	}
	if relationships > len(catalogContactIDs) {
		relationships = len(catalogContactIDs)
	}
	for i := 0; i < relationships; i++ {
		subject := catalogContactIDs[i]
		object := catalogContactIDs[(i+1)%len(catalogContactIDs)]
		predicate := catalogEdgePredicates[i%len(catalogEdgePredicates)]
		if _, err := h.ReplayAssertion(ctx, subject, gen.EdgeAssertion(predicate, object)); err != nil {
			return res, fmt.Errorf("profile %s: replay relationship %d: %w", params.Profile, i, err)
		}
		res.SeededRelationships++
	}

	// Entity nodes (org/topic/tag pool) + person→entity EDGE assertions. Seeded
	// LAST because gen.Entity DRAWS the name PRNG (givenName) — appending it after
	// the source replays + the person-subject assertion spread above keeps the
	// deterministic id/handle sequence those earlier replays depend on unshifted.
	// (The person→entity gen.EdgeAssertion calls draw no name PRNG, only sourceIDSeq.)
	//
	// The pool is small and round-robins org/topic/tag, so the edges below draw
	// objects from it repeatedly — prod-like (many people share a few orgs/topics/
	// tags). place entity nodes + lives_in edges are NOT seeded here; they ride the
	// contact-create authority flip via WithLocation (Phase 1 above).
	var orgNodeIDs, topicNodeIDs, tagNodeIDs []uuid.UUID
	for j := 0; j < params.Counts.SeededEntities; j++ {
		subtype := catalogEntitySubtypes[j%len(catalogEntitySubtypes)]
		entityID, err := h.SeedEntity(ctx, gen.Entity(subtype))
		if err != nil {
			return res, fmt.Errorf("profile %s: seed entity %d: %w", params.Profile, j, err)
		}
		switch subtype {
		case repository.EntitySubtypeOrganization:
			orgNodeIDs = append(orgNodeIDs, entityID)
		case repository.EntitySubtypeTopic:
			topicNodeIDs = append(topicNodeIDs, entityID)
		case repository.EntitySubtypeTag:
			tagNodeIDs = append(tagNodeIDs, entityID)
		}
		res.SeededEntities++
	}

	// Person→entity edges cycle works_at→org / interested_in→topic / tagged_as→tag,
	// all auto-if-confident → accepted. The subject is a distinct catalog contact
	// per edge (works_at is single-cardinality, so a distinct subject keeps it from
	// superseding); the object is drawn from the matching subtype pool (round-robin,
	// so the small pool repeats across subjects). Both review surfaces for these
	// predicates are accepted; the proposed surface is covered by the person→person
	// sibling_of edge above. A subtype with an empty pool is skipped (only possible
	// when SeededEntities < 3, which the profiles avoid).
	entityEdges := params.Counts.SeededEntityEdges
	if entityEdges > len(catalogContactIDs) {
		entityEdges = len(catalogContactIDs)
	}
	for i := 0; i < entityEdges; i++ {
		subject := catalogContactIDs[i]
		// cursor is the per-predicate occurrence index (0,0,0,1,1,1,2,...): the loop
		// picks a predicate by i%3, so for a fixed predicate i advances in steps of 3.
		// Indexing the object pool by i would therefore stride by 3 and pin every edge
		// of a predicate to one entity whenever the pool size divides 3 (the prod pool
		// is 3 per subtype). Indexing by i/3 advances one slot per occurrence, so the
		// edges walk the whole pool and wrap once the catalog outgrows it (prod-like:
		// many people share a few orgs/topics/tags).
		cursor := i / len(catalogEntityEdgePredicates)
		var predicate string
		var object uuid.UUID
		switch i % len(catalogEntityEdgePredicates) {
		case 0:
			if len(orgNodeIDs) == 0 {
				continue
			}
			predicate, object = "works_at", orgNodeIDs[cursor%len(orgNodeIDs)]
		case 1:
			if len(topicNodeIDs) == 0 {
				continue
			}
			predicate, object = "interested_in", topicNodeIDs[cursor%len(topicNodeIDs)]
		default:
			if len(tagNodeIDs) == 0 {
				continue
			}
			predicate, object = "tagged_as", tagNodeIDs[cursor%len(tagNodeIDs)]
		}
		if _, err := h.ReplayAssertion(ctx, subject, gen.EdgeAssertion(predicate, object)); err != nil {
			return res, fmt.Errorf("profile %s: replay entity edge %d: %w", params.Profile, i, err)
		}
		res.SeededEntityEdges++
	}

	// relationship_signal rows (SP1 derived storage) on a subset of the catalog
	// person nodes. In prod these are computed from comms analysis; SP1 has no
	// signal generators yet, so the seed direct-writes them through the production
	// UpsertRelationshipSignal path. SeedRelationshipSignal READS the already-seeded
	// person nodes (no generator PRNG draw), so its position relative to the gen.X()
	// blocks above is free; the subject_node_id → node FK (NO ACTION) means the
	// teardown clears these BEFORE the node deletes. Each node gets one signal with a
	// rotated key from a small fixed pool (a few signal kinds repeat across people,
	// prod-like) and a deterministic value; as_of is the generator anchor (no
	// time.Now()).
	signals := params.Counts.SeededSignals
	if signals > len(catalogContactIDs) {
		signals = len(catalogContactIDs)
	}
	signalAsOf := gen.Anchor()
	for i := 0; i < signals; i++ {
		node := catalogContactIDs[i]
		key := catalogSignalKeys[i%len(catalogSignalKeys)]
		// Deterministic value in [0.50, 0.99] so signals carry varied non-zero scalars.
		value := float64(50+i%50) / 100.0
		if err := h.SeedRelationshipSignal(ctx, node, key, value, signalAsOf, syntheticSignalMethodVersion); err != nil {
			return res, fmt.Errorf("profile %s: seed relationship signal %d: %w", params.Profile, i, err)
		}
		res.SeededSignals++
	}

	// Cadence tasks (Todoist) on the cadence-bearing catalog contacts. ReplayTodoist
	// drives the REAL CadenceSyncProvider reconcile, which reads
	// ListContactsWithContactBy (contact_by auto-computed from cadence by
	// CreateContact) and creates one `managed` cadence-due task per cadence-bearing
	// contact — the live cadence-task state. It draws NO generator PRNG (it only
	// READS the seeded contacts), so it runs after the gen.X() assertion spread above
	// without shifting the deterministic id sequence the earlier replays depend on.
	//
	// The empty fake-Todoist sync produces only `managed` rows, so the remaining
	// surface states are seeded by transitioning a deterministic three of the
	// just-created tasks via the production UpdateContactTaskState path:
	// completed / dismissed / unmanaged. pending_remote_create (a transient
	// create-in-flight state) is out of scope.
	if params.Counts.SeededTasks > 0 && len(cadenceBearingCatalogIDs) > 0 {
		if _, err := h.ReplayTodoist(ctx, cadenceBearingCatalogIDs); err != nil {
			return res, fmt.Errorf("profile %s: replay todoist: %w", params.Profile, err)
		}
		// Each transition flips one managed task's state; the row count is unchanged,
		// so res.SeededTasks (the cadence-bearing population) still reflects every row.
		for idx, state := range nonManagedTaskStates {
			if idx >= len(cadenceBearingCatalogIDs) {
				break
			}
			if _, err := h.TransitionTodoistCadenceTaskState(ctx, cadenceBearingCatalogIDs[idx], state); err != nil {
				return res, fmt.Errorf("profile %s: transition cadence task %d: %w", params.Profile, idx, err)
			}
		}
		res.SeededTasks = len(cadenceBearingCatalogIDs)
	}

	// Merge + soft-delete scenarios. Seeded LAST: each draws gen.Contact (name PRNG)
	// + gen.BoolFact, so appending them after every other generator-driven block keeps
	// the deterministic id/handle sequence the earlier source replays depend on
	// unshifted. They are STANDALONE contacts (not catalog slots) that route through
	// the production ContactService.DeleteContact / MergeContacts paths; SeedContact
	// already tracks their ids, so the existing by-id teardown sweeps clean the
	// tombstoned + merged rows (the merged_into self-FK is NO ACTION, dropped in one
	// statement). The assertions are BOOL facts (value_bool, not value_text) so they
	// add no live value_text on the surviving merge winner — keeping the determinism
	// ordering guard's "entity pool seq > text-fact seq" invariant intact.
	for i := 0; i < params.Counts.SeededSoftDeleted; i++ {
		if _, err := h.SeedSoftDeletedContact(ctx, gen.Contact(),
			gen.BoolFact(softDeleteFactPredicate, true)); err != nil {
			return res, fmt.Errorf("profile %s: seed soft-deleted contact %d: %w", params.Profile, i, err)
		}
		res.SeededSoftDeleted++
	}
	for i := 0; i < params.Counts.SeededMerged; i++ {
		// Distinct winner/loser predicates so the re-pointed loser fact lands beside the
		// winner's own without single-cardinality supersession.
		if _, _, err := h.SeedMergedContact(ctx,
			gen.Contact(), gen.Contact(),
			gen.BoolFact(mergeWinnerFactPredicate, true),
			gen.BoolFact(mergeLoserFactPredicate, true)); err != nil {
			return res, fmt.Errorf("profile %s: seed merged contact %d: %w", params.Profile, i, err)
		}
		res.SeededMerged++
	}

	return res, nil
}

// softDeleteFactPredicate / mergeWinnerFactPredicate / mergeLoserFactPredicate are
// the bool-fact predicates the merge + soft-delete scenarios assert (all
// auto-if-confident → accepted, migration-066 catalog bool predicates). The merge
// winner + loser use DISTINCT predicates so the re-pointed loser fact does not
// supersede the winner's own (each bool predicate is single-cardinality).
const (
	softDeleteFactPredicate  = "job_seeking"
	mergeWinnerFactPredicate = "traveling"
	mergeLoserFactPredicate  = "on_sabbatical"
)

// nonManagedTaskStates is the deterministic spread of non-`managed` contact_task
// surface states the profile transitions cadence tasks into (one each), so staging
// shows every state the cadence-task UI renders. `managed` is the reconcile
// default (the remaining, un-transitioned tasks); pending_remote_create is a
// transient create-in-flight state and is out of scope.
var nonManagedTaskStates = []repository.ContactTaskState{
	repository.ContactTaskStateCompleted,
	repository.ContactTaskStateDismissed,
	repository.ContactTaskStateUnmanaged,
}

// catalogTextFactPredicates is the predicate cycle the Phase-4 assertion spread
// walks. health_condition (always-confirm → proposed) leads so any non-zero
// SeededAssertions count covers the pending-review surface; home_address /
// job_title / preference are auto-if-confident → accepted; occurrence is also
// always-confirm → proposed (seeded without a valid_from in PR1; the dated
// valid_from is a later PR). All are migration-066 catalog text predicates.
var catalogTextFactPredicates = []string{
	"health_condition",
	"home_address",
	"job_title",
	"preference",
	"occurrence",
}

// catalogBoolFactPredicates is the bool-fact predicate cycle the value-type
// spread walks. All three are auto-if-confident → accepted (migration-066
// catalog bool predicates). Asserted with value=true ("currently X").
var catalogBoolFactPredicates = []string{
	"job_seeking",
	"on_sabbatical",
	"traveling",
}

// catalogEdgePredicates is the person→person edge predicate cycle. knows and
// introduced_by are auto-if-confident → accepted; sibling_of is always-confirm →
// proposed, so a full cycle covers both the accepted and pending edge surfaces.
// All are migration-066 catalog person→person edge predicates.
var catalogEdgePredicates = []string{
	"knows",
	"introduced_by",
	"sibling_of",
}

// catalogEntitySubtypes is the round-robin order the entity pool is built in, so a
// SeededEntities value ≥3 yields ≥1 of each subtype. They are migration-066
// curated entity subtypes.
var catalogEntitySubtypes = []string{
	repository.EntitySubtypeOrganization,
	repository.EntitySubtypeTopic,
	repository.EntitySubtypeTag,
}

// catalogEntityEdgePredicates is the person→entity edge predicate cycle, aligned
// 1:1 with catalogEntitySubtypes (works_at→org, interested_in→topic,
// tagged_as→tag). All three are auto-if-confident → accepted (migration-066
// catalog person→entity edge predicates). NOTE the alignment with
// catalogEntitySubtypes is load-bearing: the edge loop's `i % 3` branch picks the
// object pool for the matching subtype.
var catalogEntityEdgePredicates = []string{
	"works_at",
	"interested_in",
	"tagged_as",
}

// catalogSignalKeys is the small fixed pool of relationship_signal keys the seed
// rotates over (one key per seeded node) so a few signal kinds repeat across many
// person nodes (prod-like). These are the keys the app expects for SP1 derived
// storage (closeness / real_cadence_days / trend, per repository.RelationshipSignal),
// so seeded rows match what consumers read; teardown deletes by subject node id
// (not key), so they need no namespace prefix.
var catalogSignalKeys = []string{
	"closeness",
	"real_cadence_days",
	"trend",
}

// syntheticSignalMethodVersion tags the method_version of the seeded
// relationship_signal rows so they are identifiable as toolkit-authored.
const syntheticSignalMethodVersion = "synthetic-seed"

// catalogLocationStems is the small fixed pool the lives_in location rotates over
// so locations repeat across contacts (prod-like) while staying deterministic. The
// values are flat (no comma/hierarchy) so EnsurePlaceTx mints a flat place node
// with no `within` parent edge.
var catalogLocationStems = []string{
	"riverton",
	"lakeside",
	"hillcrest",
	"meadowbrook",
}

// catalogOptionsFor returns the contact-builder options for catalog contact i of
// n, deterministically spreading the edge-case shapes across the population:
// cadence states (overdue / recent / never-contacted), the 1900 birthday
// sentinel, a unicode name, a descender name, a no-method contact, plus a
// fraction carrying real-year birthdays and how_met bio facts. Catalog contacts
// get NO settling replay, so these last_contacted states survive (a MatchSeeded
// inbound would otherwise let the cadence updater overwrite them).
//
// anchor is the generator's clock (real-year birthdays are anchor-relative, so
// no time.Now()); prefix is the namespace prefix the how_met value carries so it
// stays obviously synthetic. Neither draws from the PRNG, so the options are
// ordering-safe in place (they only set config on the existing gen.Contact call).
//
// The no-method slot (i == 3) carries WithNoMethods, which overrides the email
// default — so it must NOT also request a method; it returns early and gets no
// bio facts. The name edge cases (unicode/descender) are mutually exclusive by i.
func catalogOptionsFor(i, n int, anchor time.Time, prefix string) []factory.ContactOption {
	var opts []factory.ContactOption

	// The no-method bucket: a cadence-bearing contact with NO contact_method.
	if i == 3 && n > 3 {
		opts = append(opts, factory.WithNoMethods(), factory.WithCadence("monthly"), factory.WithOverdue())
		return opts
	}

	opts = append(opts, factory.WithEmail())

	// Cadence + recency spread: every third contact overdue, every third recent,
	// the rest never-contacted (no LastContacted).
	switch i % 3 {
	case 0:
		opts = append(opts, factory.WithCadence("monthly"), factory.WithOverdue())
	case 1:
		opts = append(opts, factory.WithCadence("weekly"), factory.WithRecent())
	default:
		// never-contacted: a cadence but no last_contacted.
		opts = append(opts, factory.WithCadence("quarterly"))
	}

	// One representative of each rendering / data edge case (only when the
	// population is large enough to carry them without crowding out the spread).
	switch {
	case i == 0:
		opts = append(opts, factory.WithBirthday1900Sentinel(3, 14))
	case i == 1 && n > 2:
		opts = append(opts, factory.WithUnicodeName())
	case i == 2 && n > 3:
		opts = append(opts, factory.WithDescenderName())
	}

	// Real-year birthdays on ~1/5 of contacts (excluding i == 0, which carries the
	// 1900 sentinel above). Anchor-relative ages 25–54, deterministic month/day.
	if i != 0 && i%5 == 2 {
		birthYear := anchor.Year() - (25 + i%30)
		bday := time.Date(birthYear, time.Month(1+i%12), 1+i%28, 0, 0, 0, 0, time.UTC)
		opts = append(opts, factory.WithBirthday(bday))
	}

	// how_met on ~1/4 of contacts (ns-prefixed synthetic text, rotated stem).
	if i%4 == 1 {
		stem := catalogHowMetStems[(i/4)%len(catalogHowMetStems)]
		opts = append(opts, factory.WithHowMet(prefix+"met-"+stem))
	}

	// lives_in location on ~1/5 of contacts (ns-prefixed flat label → place entity
	// node + lives_in edge via the contact-create authority flip). Rotated stem so
	// locations repeat across contacts (prod-like); ns-prefixed so the entity
	// teardown's label-prefix sweep catches the auto-created place node. Avoids
	// i == 3 (the no-method slot returns early above).
	if i%5 == 4 {
		stem := catalogLocationStems[(i/5)%len(catalogLocationStems)]
		opts = append(opts, factory.WithLocation(prefix+"place-"+stem))
	}

	return opts
}

// catalogHowMetStems is the small fixed pool the how_met bio fact rotates over so
// the value repeats across contacts (prod-like) while staying deterministic.
var catalogHowMetStems = []string{
	"at-a-conference",
	"through-a-mutual-friend",
	"at-work",
	"online-community",
}

// perSourceSettledCount bounds the per-source settled-interaction count by
// profile, keeping the dev profile fast (replay-light) while prod-shaped carries
// a thicker slice.
func perSourceSettledCount(p Profile) int {
	if p == ProfileProdShaped {
		return 3
	}
	return 1
}

// interactionSpreadInterval is the deterministic gap between successive settled
// interactions on one contact. It is wider than a day so the messaging-aggregate
// sources (gchat / messages / telegram), which collapse same-day messages into a
// single interaction, still yield one interaction per replayed message; ~3 weeks
// gives a months-scale history at the profile message counts.
const interactionSpreadInterval = 21 * 24 * time.Hour

// interactionSpreadAge is the backward age of the j-th settled message on a
// contact: 0 for the newest (so it drives last_contacted), then one
// interactionSpreadInterval older per step. Deterministic (a pure function of j),
// anchor-relative via the source factories' WithMessageAge (no time.Now()).
func interactionSpreadAge(j int) time.Duration {
	return time.Duration(j) * interactionSpreadInterval
}

// SeedAllowed is the single chokepoint guard the seed/reset entrypoints route
// through. It returns an error iff CRM_ENV is a production alias (production /
// prod), so a destructive reset or shippable fake-fetcher seed can never run in
// production. Checked PRE-DB by the entrypoints.
func SeedAllowed(cfg *config.Config) error {
	switch cfg.Runtime.CRMEnvironment {
	case "production", "prod":
		return fmt.Errorf("synthetic: seed/reset refused — CRM_ENV is %q (production); the synthetic seed entrypoints are non-production only", cfg.Runtime.CRMEnvironment)
	default:
		return nil
	}
}
