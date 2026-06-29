package tests

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/synthetic"
	"personal-crm/backend/internal/synthetic/factory"
	"personal-crm/backend/tests/testsupport"

	"github.com/stretchr/testify/require"
)

// trailingSeqRE extracts the generator's monotonic sourceIDSeq embedded as the
// trailing "-<digits>" of a synthetic value_text / entity normalized_name (e.g.
// "synth-<ns>-organization-Alice-42" → 42). The seq is namespace-INDEPENDENT (a
// pure counter, unlike the PRNG-drawn name component), so it is a stable
// cross-run/cross-namespace ordering witness. Returns ok=false for values with no
// trailing seq (e.g. the how_met "synth-<ns>-met-<stem>" or a place
// "synth-<ns>-place-<stem>"), which the caller skips.
var trailingSeqRE = regexp.MustCompile(`-(\d+)$`)

func trailingSeq(s string) (int, bool) {
	m := trailingSeqRE.FindStringSubmatch(s)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return n, true
}

// fixedAccelBaseUnix is a deterministic TIME_BASE (a fixed Unix second) for the
// high-acceleration settle-budget test, so the fixture makes no wall-clock call.
const fixedAccelBaseUnix int64 = 1735689600 // 2025-01-01T00:00:00Z

// These profile-orchestration integration tests are SLOW-gated
// (testsupport.RequireLongTests) and routed to the nightly slow suite via the
// TestSynthetic name prefix. Each sub-test uses a UNIQUE namespace so
// shared-test-DB reuse cannot collide; assertions read namespace-scoped harness
// repos (NEVER global counts — the shared DB accumulates).
//
// The profiles run with SMALL overridden counts so a settle-heavy prod-shaped
// world does not blow the CI/pre-push budget; the volume is a profile default
// the orchestration multiplies over, not a behavior the orchestration depends
// on, so a small count exercises the same code paths.

// TestSyntheticProfile_MinimalScopedMatchesSeedAll proves the minimal-scoped
// profile is byte-stable == today's SeedAll + DefaultParams (the golden-scenario
// regression depends on this).
func TestSyntheticProfile_MinimalScopedMatchesSeedAll(t *testing.T) {
	testsupport.RequireLongTests(t)
	database, ctx := newSyntheticDB(t)

	params, err := synthetic.ProfileParams(synthetic.ProfileMinimalScoped)
	require.NoError(t, err)
	params.Namespace = syntheticNS(t)

	h := synthetic.NewHarnessForNamespace(t, ctx, database, params.Namespace, params.Seed)
	res, err := synthetic.RunProfile(ctx, h, params)
	require.NoError(t, err)
	require.Equal(t, synthetic.ProfileMinimalScoped, res.Profile)
	// minimal-scoped == SeedAll: one Gmail-settled + one Telegram-settled contact.
	require.Equal(t, 1, res.GmailSettled)
	require.Equal(t, 1, res.TelegramSettled)
	require.Equal(t, 2, res.Contacts)

	remaining, err := h.ContactsRemaining(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(2), remaining, "minimal-scoped seeds exactly two contacts")
}

// TestSyntheticProfile_DevCoversCatalog asserts the dev profile produces the
// producible edge-case + pending coverage. Counts are scoped to the harness's
// namespace.
func TestSyntheticProfile_DevCoversCatalog(t *testing.T) {
	testsupport.RequireLongTests(t)
	database, ctx := newSyntheticDB(t)

	params, err := synthetic.ProfileParams(synthetic.ProfileDev)
	require.NoError(t, err)
	params.Namespace = syntheticNS(t)

	h := synthetic.NewHarnessForNamespace(t, ctx, database, params.Namespace, params.Seed)
	res, err := synthetic.RunProfile(ctx, h, params)
	require.NoError(t, err)

	// Catalog contacts are seeded WITHOUT a settling replay, so the per-source
	// settled counts reflect the DEDICATED settled contacts (≥1 each), NOT the
	// catalog size. Total contacts = catalog (SeededContacts) + the settled set.
	require.Greater(t, res.Contacts, params.Counts.SeededContacts, "catalog + dedicated settled contacts")
	require.GreaterOrEqual(t, res.GmailSettled, 1)
	require.GreaterOrEqual(t, res.TelegramSettled, 1)
	require.GreaterOrEqual(t, res.GCalSettled, 1)
	require.GreaterOrEqual(t, res.GChatSettled, 1)
	require.GreaterOrEqual(t, res.IMessageSettled, 1)
	require.Equal(t, params.Counts.UnmatchedExternal, res.UnmatchedExternal)
	require.Equal(t, params.Counts.StrandedTelegram, res.StrandedTelegram)
	require.Equal(t, params.Counts.StrandedMessages, res.StrandedMessages)
	require.Equal(t, params.Counts.UnmatchedCalendar, res.UnmatchedCalendar)
	require.Equal(t, params.Counts.OrphanMeetingNote, res.OrphanMeetingNote)
	require.Equal(t, params.Counts.SeededAssertions, res.SeededAssertions)
	require.Equal(t, 2, res.SeededAssertions, "dev profile seeds graph assertions")
	// Bio facts (birthday / how_met) ride on the contact-create authority flip,
	// spread by catalog index; the dev catalog is large enough to carry ≥1 of each.
	require.GreaterOrEqual(t, res.ContactsWithBirthday, 1, "dev profile seeds birthday bio facts")
	require.GreaterOrEqual(t, res.ContactsWithHowMet, 1, "dev profile seeds how_met bio facts")
	// Cadence tasks: the dev catalog is cadence-bearing, so ReplayTodoist seeds a
	// managed task per contact (res.SeededTasks is the actual rows, a catalog-wide
	// count, not the >0 gate value in Counts.SeededTasks).
	require.GreaterOrEqual(t, res.SeededTasks, 1, "dev profile seeds cadence tasks")
	// Value-type + edge graph rows: the dev catalog (18) far exceeds the small
	// dev knob counts, so the seeded counts are exact (no catalog-size bounding),
	// and the bool-fact gate produces exactly one toolkit date fact.
	require.Equal(t, params.Counts.SeededBoolFacts, res.SeededBoolFacts, "dev profile seeds bool facts")
	require.Equal(t, params.Counts.SeededRelationships, res.SeededRelationships, "dev profile seeds person→person edges")
	require.Equal(t, 1, res.SeededDateFacts, "dev profile seeds one toolkit date fact")
	// Entity pool + person→entity edges: the dev catalog (18) far exceeds the small
	// dev knob counts, so the seeded counts are exact (no catalog-size bounding).
	require.Equal(t, params.Counts.SeededEntities, res.SeededEntities, "dev profile seeds the entity pool")
	require.Equal(t, params.Counts.SeededEntityEdges, res.SeededEntityEdges, "dev profile seeds person→entity edges")
	// relationship_signal rows: the dev catalog (18) exceeds the small dev signal
	// knob, so the seeded count is exact (no catalog-size bounding).
	require.Equal(t, params.Counts.SeededSignals, res.SeededSignals, "dev profile seeds relationship signals")
	// lives_in locations ride the contact-create authority flip, spread by catalog
	// index; the dev catalog is large enough to carry ≥1.
	require.GreaterOrEqual(t, res.ContactsWithLocation, 1, "dev profile seeds location bio facts")

	remaining, err := h.ContactsRemaining(ctx)
	require.NoError(t, err)
	require.Greater(t, remaining, int64(0), "dev profile seeds catalog contacts")
}

// TestSyntheticProfile_ProdShapedCoverageCheck is the load-bearing coverage
// assertion for #380: a prod-shaped seed (bounded counts) must populate every
// PRODUCIBLE load-bearing UI bucket, and must leave the three DEFERRED
// (no-toolkit-producer) pending states absent-by-design so the gap is documented
// rather than silently claimed.
func TestSyntheticProfile_ProdShapedCoverageCheck(t *testing.T) {
	testsupport.RequireLongTests(t)
	database, ctx := newSyntheticDB(t)

	params, err := synthetic.ProfileParams(synthetic.ProfileProdShaped)
	require.NoError(t, err)
	params.Namespace = syntheticNS(t)
	// Bound the prod-shaped volume for CI: the orchestration is the same at a
	// smaller scale, and the coverage buckets each still get ≥1 representative.
	params.Counts.SeededContacts = 9 // ≥3 so the unicode/descender catalog slots fill
	params.Counts.UnmatchedExternal = 2
	params.Counts.StrandedTelegram = 1
	params.Counts.StrandedMessages = 1
	params.Counts.UnmatchedCalendar = 1
	params.Counts.OrphanMeetingNote = 1
	// 5 assertions walk the full text-fact predicate cycle once (CI-safe), so every
	// predicate — incl. the always-confirm ones (health_condition, occurrence →
	// proposed) and the accepted ones (home_address/job_title/preference) — is
	// exercised.
	params.Counts.SeededAssertions = 5
	// One full bool-fact cycle (job_seeking/on_sabbatical/traveling) + one full
	// edge cycle (knows/introduced_by/sibling_of) so every new predicate — incl.
	// the always-confirm sibling_of (→ proposed) and the auto-if-confident rest
	// (→ accepted) — is exercised. The bool-fact gate also seeds one toolkit date
	// fact.
	params.Counts.SeededBoolFacts = 3
	params.Counts.SeededRelationships = 3
	// Enable cadence-task seeding (>0 gate). The 9-contact catalog is all
	// cadence-bearing, so reconcile creates 9 `managed` tasks and three are
	// transitioned to completed/dismissed/unmanaged — every surface state present.
	params.Counts.SeededTasks = 1
	// Entity pool of 3 (1 org + 1 topic + 1 tag) + one full person→entity edge cycle
	// (works_at/interested_in/tagged_as) so every entity subtype + edge type is
	// exercised. lives_in rides WithLocation (index-spread, present at this catalog
	// size) — not a count knob.
	params.Counts.SeededEntities = 3
	params.Counts.SeededEntityEdges = 3
	// A few relationship_signal rows (SP1 derived storage) across distinct catalog
	// person nodes, one full signal-key cycle (closeness/real_cadence_days/trend).
	params.Counts.SeededSignals = 3

	h := synthetic.NewHarnessForNamespace(t, ctx, database, params.Namespace, params.Seed)
	res, err := synthetic.RunProfile(ctx, h, params)
	require.NoError(t, err)

	// PRODUCIBLE buckets all have ≥1 representative.
	require.GreaterOrEqual(t, res.Contacts, params.Counts.SeededContacts, "catalog contacts seeded")
	require.GreaterOrEqual(t, res.GmailSettled, 1, "gmail-settled present")
	require.GreaterOrEqual(t, res.TelegramSettled, 1, "telegram-settled present")
	require.GreaterOrEqual(t, res.GCalSettled, 1, "gcal-settled present")
	require.GreaterOrEqual(t, res.GChatSettled, 1, "gchat-settled present")
	require.GreaterOrEqual(t, res.IMessageSettled, 1, "imessage-settled present")
	require.GreaterOrEqual(t, res.UnmatchedExternal, 1, "unmatched external (Imports queue) present")
	require.GreaterOrEqual(t, res.StrandedTelegram, 1, "stranded telegram present")
	require.GreaterOrEqual(t, res.StrandedMessages, 1, "stranded iMessage present")
	require.GreaterOrEqual(t, res.UnmatchedCalendar, 1, "unmatched calendar attendee present")
	require.GreaterOrEqual(t, res.OrphanMeetingNote, 1, "orphan meeting-note present")

	// DEFERRED pending states (no toolkit producer yet): the ProfileResult has
	// no field for them precisely because RunProfile produces none. This guards
	// the documented gap — if a future change starts producing them it must add a
	// counter, which forces revisiting this test. Asserting the producible set is
	// complete (every res.* counter > 0) is the absent-by-design check: there is
	// no conflict-meeting-note / title-candidate / gchat-pending counter to assert
	// because none is produced.

	// DB-LEVEL cadence/no-method bucket check: the catalog contacts must NOT be
	// clobbered by a settling replay (a MatchSeeded inbound would let the cadence
	// updater overwrite last_contacted). Scoped to the namespace prefix.
	support := repository.NewSyntheticSupportRepository(database.Queries)
	buckets, err := support.ListContactBucketsByNamePrefix(ctx, h.Generator().Prefix())
	require.NoError(t, err)

	// "Overdue" must mean a last_contacted WELL in the past (WithOverdue seeds
	// ~90 days ago), NOT merely before now — a recent contact (WithRecent seeds
	// <48h ago) is also before now. Using a 14-day-ago floor distinguishes the
	// overdue bucket from the recent bucket, so a clobber-to-recent regression
	// (the bug this guards) would FAIL here.
	var overdue, neverContacted, noMethod int
	now := accelerated.GetCurrentTime()
	overdueFloor := now.Add(-14 * 24 * time.Hour)
	for _, b := range buckets {
		if b.MethodCount == 0 {
			noMethod++
		}
		if b.Cadence != nil && *b.Cadence != "" {
			switch {
			case b.LastContacted == nil:
				neverContacted++
			case b.LastContacted.Before(overdueFloor):
				overdue++
			}
		}
	}
	require.GreaterOrEqual(t, overdue, 1, "≥1 cadence-bearing contact with a far-past last_contacted (overdue bucket survived a settling replay)")
	require.GreaterOrEqual(t, neverContacted, 1, "≥1 cadence-bearing never-contacted contact (NULL last_contacted survived)")
	require.GreaterOrEqual(t, noMethod, 1, "≥1 no-method contact (the no-method bucket exists)")

	// Bio facts (how_met, location, real-year birthdays) ride on the contact-create
	// authority flip; the catalog carries ≥1 of each. A location additionally mints
	// a place entity node + a lives_in edge (asserted below).
	require.GreaterOrEqual(t, res.ContactsWithBirthday, 1, "≥1 contact with a birthday bio fact")
	require.GreaterOrEqual(t, res.ContactsWithHowMet, 1, "≥1 contact with a how_met bio fact")
	require.GreaterOrEqual(t, res.ContactsWithLocation, 1, "≥1 contact with a location bio fact")

	// Accept/pending coverage (D6) + full text-fact spread: assert the SPECIFIC
	// (predicate, review-policy) outcome for EVERY predicate in the Phase-4 cycle,
	// not merely that some assertion of each status exists — so a predicate
	// silently dropping out of the spread (e.g. occurrence) or no longer landing
	// its expected status is caught. health_condition + occurrence (always-confirm)
	// land PROPOSED (pending-review surface); home_address / job_title / preference
	// (auto-if-confident) land ACCEPTED (accepted-knowledge surface). Scoped to the
	// namespace.
	assertions, err := support.ListAssertionsByNodePrefix(ctx, h.Generator().Prefix())
	require.NoError(t, err)
	seen := make(map[string]bool) // "<predicate>/<status>"
	var realYearBirthday bool
	var boolFactTrue bool
	for _, a := range assertions {
		seen[a.PredicateKey+"/"+a.Status] = true
		// Positive guard: EnsurePlaceTx mints FLAT place nodes (no synonym /
		// hierarchy resolution), so the seed must produce ZERO place→place `within`
		// edges. A surviving `within` would FK-block the entity-node teardown sweep;
		// catching it here makes the regression loud at seed time. (A `within`
		// subject is an ns-prefixed place node, so it would appear in this list.)
		require.NotEqual(t, "within", a.PredicateKey, "no place→place within hierarchy assertion (flat place nodes)")
		// item 5: a real-year birthday from the spread must exist, not just the
		// 1900 month/day-only sentinel (which alone would satisfy
		// ContactsWithBirthday even if the real-year spread regressed).
		if a.PredicateKey == "birthday" && a.ValueDate != nil && !strings.HasPrefix(*a.ValueDate, "1900-") {
			realYearBirthday = true
		}
		// A bool fact must carry value_bool (proves the ValueBool plumbing routes
		// the scalar, not just the predicate).
		if a.ValueBool != nil && *a.ValueBool {
			boolFactTrue = true
		}
	}
	for _, want := range []string{
		"health_condition/" + repository.AssertionStatusProposed,
		"occurrence/" + repository.AssertionStatusProposed,
		"home_address/" + repository.AssertionStatusAccepted,
		"job_title/" + repository.AssertionStatusAccepted,
		"preference/" + repository.AssertionStatusAccepted,
		// bool facts — all auto-if-confident → accepted.
		"job_seeking/" + repository.AssertionStatusAccepted,
		"on_sabbatical/" + repository.AssertionStatusAccepted,
		"traveling/" + repository.AssertionStatusAccepted,
		// person→person edges — knows/introduced_by accepted, sibling_of
		// (always-confirm) proposed, so BOTH edge review surfaces are present.
		"knows/" + repository.AssertionStatusAccepted,
		"introduced_by/" + repository.AssertionStatusAccepted,
		"sibling_of/" + repository.AssertionStatusProposed,
		// person→entity edges — all auto-if-confident → accepted. lives_in rides the
		// contact-create authority flip (WithLocation); works_at/interested_in/
		// tagged_as ride the entity pool + edge spread.
		"lives_in/" + repository.AssertionStatusAccepted,
		"works_at/" + repository.AssertionStatusAccepted,
		"interested_in/" + repository.AssertionStatusAccepted,
		"tagged_as/" + repository.AssertionStatusAccepted,
	} {
		require.True(t, seen[want], "graph value-type/edge spread must land %s", want)
	}
	require.True(t, realYearBirthday, "≥1 real-year (non-1900-sentinel) birthday from the spread")
	require.True(t, boolFactTrue, "≥1 bool fact carrying value_bool=true")

	// Value-type + edge result counts (bool facts + the toolkit date fact +
	// person→person edges). The CI override seeds one full cycle of each.
	require.GreaterOrEqual(t, res.SeededBoolFacts, 1, "bool facts seeded")
	require.GreaterOrEqual(t, res.SeededRelationships, 1, "person→person edges seeded")
	require.GreaterOrEqual(t, res.SeededDateFacts, 1, "toolkit-authored date fact seeded")

	// Entity layer: the org/topic/tag pool + the place nodes from WithLocation must
	// all be present, and the person→entity edges seeded. Scoped to the namespace via
	// the node label prefix.
	require.GreaterOrEqual(t, res.SeededEntities, 1, "entity pool seeded")
	require.GreaterOrEqual(t, res.SeededEntityEdges, 1, "person→entity edges seeded")
	prefix := h.Generator().Prefix()
	entityNames, err := support.ListEntityNamesByNodePrefix(ctx, prefix)
	require.NoError(t, err)
	subtypesSeen := make(map[string]bool)
	for _, e := range entityNames {
		subtypesSeen[e.Subtype] = true
	}
	for _, subtype := range []string{
		repository.EntitySubtypePlace, // from WithLocation (contact-create authority flip)
		repository.EntitySubtypeOrganization,
		repository.EntitySubtypeTopic,
		repository.EntitySubtypeTag,
	} {
		require.True(t, subtypesSeen[subtype], "≥1 %q entity node present", subtype)
	}

	// Positive guard: tags are modeled as `tagged_as` graph edges (asserted above),
	// NOT legacy contact_tag rows — the legacy table MUST stay zero for the
	// namespace, so a future reader does not "fix" a perceived gap by re-seeding it.
	legacyContactTags, err := support.CountContactTagsByContactNamePrefix(ctx, prefix)
	require.NoError(t, err)
	require.Equal(t, int64(0), legacyContactTags, "tags seeded as tagged_as edges, not legacy contact_tag rows")

	// relationship_signal (SP1 derived storage): the seed writes per-node scalar
	// signals on a subset of catalog person nodes through the production upsert path.
	// Assert the result count and that the rows actually landed in the DB (scoped to
	// THIS run's tracked signal nodes). The harness teardown (t.Cleanup) deletes them
	// before the node deletes; the explicit signals-remaining==0 check lives in the
	// determinism test, which tears down inline.
	require.GreaterOrEqual(t, res.SeededSignals, 1, "relationship signals seeded")
	signalsLanded, err := h.SignalsRemaining(ctx)
	require.NoError(t, err)
	require.GreaterOrEqual(t, signalsLanded, int64(1), "≥1 relationship_signal row present for the seeded nodes")

	// Cadence tasks: ReplayTodoist seeds a `managed` cadence task on each
	// cadence-bearing catalog contact, and the profile transitions a deterministic
	// three to completed/dismissed/unmanaged. Assert ≥1 row in EACH state the seed
	// produces (namespace-scoped), so every cadence-task surface the staging UI
	// renders has content. pending_remote_create is out of scope (a transient
	// create-in-flight state the seed does not produce).
	require.GreaterOrEqual(t, res.SeededTasks, 1, "cadence tasks seeded")
	for _, state := range []repository.ContactTaskState{
		repository.ContactTaskStateManaged,
		repository.ContactTaskStateUnmanaged,
		repository.ContactTaskStateCompleted,
		repository.ContactTaskStateDismissed,
	} {
		count, err := support.CountContactTasksByStateAndNamePrefix(ctx, string(state), prefix)
		require.NoError(t, err)
		require.GreaterOrEqual(t, count, int64(1), "≥1 contact_task in state %q", state)
	}

	remaining, err := h.ContactsRemaining(ctx)
	require.NoError(t, err)
	require.Greater(t, remaining, int64(0), "prod-shaped profile seeds contacts")
}

// TestSyntheticProfile_ProdShapedDeterministic proves the prod-shaped seed is
// deterministic: two runs at the same (namespace, seed, anchor) produce
// byte-identical ProfileResult counts AND an identical fingerprint of the
// generated assertion values. Count-pinning alone is insufficient — counts can
// match while a non-deterministic source (map iteration, wall-clock leak) shifts
// generated values — so the (value_text, value_date, value_bool) fingerprint is
// the drift detector.
//
// Scope (what this catches vs. where): the "seed gen.X() LAST" rule is covered by
// THREE complementary guards. (1) This run-to-run fingerprint comparison detects
// NON-deterministic drift only (map iteration, wall-clock leak); on its own it
// cannot see a DETERMINISTIC mis-ordering (both runs shift identically and still
// match), and a full-VALUE golden is precluded — syntheticNS(t) randomizes the
// namespace per run and the PRNG is seeded with fnv(namespace), so the fingerprinted
// values have no run-stable form to pin. (2) The ordering guard below pins the one
// thing that IS run-stable: the monotonic sourceIDSeq counter (namespace-independent),
// asserting every gen.Entity-pool seq exceeds every text-fact seq — so a deterministic
// mis-ordering of the entity block (the case (1) can't see) fails there. (3) The
// coverage test's Gate-A settle catches a mis-placed gen.X() that shifts later
// source-replay ids into a cross-contact peer-id collision that fails the settle
// loudly (the original SP1 mis-order regression surfaced exactly this way). A
// mis-ordering that shifts NO subsequent source-replay draw is harmless
// (distinct-but-valid synthetic data), so the three guards together cover the
// load-bearing class.
//
// Both runs share the SAME namespace (with a full teardown
// between) so the ns-prefix + PRNG stream line up, AND a PINNED generator anchor
// so the anchor-relative timestamps (birthday date) are byte-identical too —
// without the pin, birthday value_date would legitimately drift between runs
// (anchor-relative per the factory contract) and could not be fingerprinted.
// proposition_key and the edge object node id are NOT fingerprinted (they embed
// the per-run subject/object UUID, so they are not run-stable); a person→person
// edge therefore contributes only its (predicate_key, status) to the fingerprint.
func TestSyntheticProfile_ProdShapedDeterministic(t *testing.T) {
	testsupport.RequireLongTests(t)
	database, ctx := newSyntheticDB(t)

	params, err := synthetic.ProfileParams(synthetic.ProfileProdShaped)
	require.NoError(t, err)
	params.Namespace = syntheticNS(t)
	// Bound the volume for CI: determinism is a property of the orchestration, not
	// of the scale. 5 assertions walk the full predicate cycle.
	params.Counts.SeededContacts = 5
	params.Counts.SeededAssertions = 5
	// One full bool-fact + edge cycle each (bounded, deterministic at this scale).
	params.Counts.SeededBoolFacts = 3
	params.Counts.SeededRelationships = 3
	// Entity pool (1 org + 1 topic + 1 tag) + one full person→entity edge cycle, so
	// the entity normalized_name fingerprint covers org/topic/tag/place subtypes.
	params.Counts.SeededEntities = 3
	params.Counts.SeededEntityEdges = 3
	// A few relationship_signal rows so the run-to-run ProfileResult count
	// (res.SeededSignals) is exercised and the teardown sweep is asserted below.
	params.Counts.SeededSignals = 3
	params.Counts.UnmatchedExternal = 1
	params.Counts.StrandedTelegram = 1
	params.Counts.StrandedMessages = 1
	params.Counts.UnmatchedCalendar = 1
	params.Counts.OrphanMeetingNote = 1

	// Capture the generator anchor ONCE and reuse it for both runs: identical
	// across runs (so timestamped output incl. birthday value_date is
	// byte-identical and the fingerprint covers it) yet ≈now so the settle
	// pipeline — which runs on the real system clock — is not skewed by a far-past
	// anchor (a far-past anchor starves Gate A: the consumers never link the
	// stale-dated messages within the real-time budget).
	anchor := accelerated.GetCurrentTime()
	support := repository.NewSyntheticSupportRepository(database.Queries)

	// run seeds the profile, captures (ProfileResult, assertion fingerprint, entity
	// normalized_name fingerprint), then fully tears down so the next run starts from
	// a clean namespace. It also asserts the teardown swept every entity node (pool
	// + place) — a leaked entity node would FK-block or duplicate-violate the next
	// run, and a surviving place node from a `within` edge would FK-block the sweep.
	run := func() (synthetic.ProfileResult, []repository.AssertionSummary, []repository.EntityNameSummary) {
		h, teardown, err := synthetic.NewHarnessWithDBForNamespaceAt(ctx, database, params.Namespace, params.Seed, anchor)
		require.NoError(t, err)
		res, err := synthetic.RunProfile(ctx, h, params)
		require.NoError(t, err)
		prefix := h.Generator().Prefix()
		fp, err := support.ListAssertionsByNodePrefix(ctx, prefix)
		require.NoError(t, err)
		entFP, err := support.ListEntityNamesByNodePrefix(ctx, prefix)
		require.NoError(t, err)
		require.NoError(t, teardown(context.Background()))
		// Teardown correctness: the entity nodes (org/topic/tag pool + place nodes)
		// are swept, so the namespace is clean for the next run.
		remaining, err := support.ListEntityNamesByNodePrefix(ctx, prefix)
		require.NoError(t, err)
		require.Empty(t, remaining, "entity nodes (pool + place) swept by teardown")
		// Teardown correctness: the relationship_signal rows (FK→node, NO ACTION) are
		// swept BEFORE the node deletes, so none remain for the seeded subject nodes —
		// a leaked signal would FK-block the next run's person-node delete.
		sigRemaining, err := h.SignalsRemaining(ctx)
		require.NoError(t, err)
		require.Zero(t, sigRemaining, "relationship_signal rows swept by teardown")
		return res, fp, entFP
	}

	res1, fp1, ent1 := run()
	res2, fp2, ent2 := run()

	require.Equal(t, res1, res2, "prod-shaped ProfileResult must be deterministic across runs")
	require.NotEmpty(t, fp1, "the seed must produce assertions to fingerprint")
	require.Equal(t, fp1, fp2, "assertion (value_text, value_date, value_bool) fingerprint must be deterministic across runs")
	require.NotEmpty(t, ent1, "the seed must produce entity nodes to fingerprint")
	require.Equal(t, ent1, ent2, "entity (subtype, normalized_name) fingerprint must be deterministic across runs")

	// Ordering guard for the "seed gen.X() LAST" rule. The run-to-run
	// comparison above catches NON-deterministic drift but NOT a DETERMINISTIC
	// mis-ordering of the gen.Entity pool (both runs would shift identically). A
	// literal name golden is precluded here — syntheticNS(t) randomizes the namespace
	// per run and the PRNG is seeded fnv(namespace), so the name component is not
	// run-stable. The embedded sourceIDSeq IS run-stable (a pure counter, namespace-
	// independent), so it is the pinnable witness: gen.Entity is seeded LAST, after
	// the Phase-4 text-fact spread, so every gen.Entity-pool seq must exceed every
	// text-fact value seq. A mis-ordered entity block (moved before the spread, or
	// earlier among the source replays) inverts this and fails loudly — the
	// regression class a same-impl two-run comparison cannot see. (place nodes ride
	// WithLocation, not gen.Entity, and carry a stem not a seq, so they are excluded.)
	maxTextFactSeq := -1
	for _, a := range fp1 {
		if a.ValueText == nil {
			continue
		}
		if seq, ok := trailingSeq(*a.ValueText); ok && seq > maxTextFactSeq {
			maxTextFactSeq = seq
		}
	}
	minEntitySeq := -1
	for _, e := range ent1 {
		if e.Subtype == repository.EntitySubtypePlace {
			continue // place names carry a stem, not a seq
		}
		seq, ok := trailingSeq(e.NormalizedName)
		require.True(t, ok, "gen.Entity normalized_name %q must carry a trailing sourceIDSeq", e.NormalizedName)
		if minEntitySeq == -1 || seq < minEntitySeq {
			minEntitySeq = seq
		}
	}
	require.GreaterOrEqual(t, maxTextFactSeq, 0, "the seed must produce text-fact assertions with a seq witness")
	require.GreaterOrEqual(t, minEntitySeq, 0, "the seed must produce gen.Entity pool nodes with a seq witness")
	require.Greater(t, minEntitySeq, maxTextFactSeq,
		"gen.Entity pool must be seeded LAST (after the text-fact spread): entity seq %d must exceed max text-fact seq %d — a smaller entity seq means a mis-ordered gen.X() insertion",
		minEntitySeq, maxTextFactSeq)
}

// TestSyntheticProfile_QuiesceLeavesRows proves the seed-and-leave lifecycle:
// after RunProfile + Quiesce, the namespace's rows STILL EXIST. The test cleans
// its OWN namespace via the harness's namespace-scoped delete (NOT the global
// reset, which would nuke parallel tests on the shared DB).
func TestSyntheticProfile_QuiesceLeavesRows(t *testing.T) {
	testsupport.RequireLongTests(t)
	database, ctx := newSyntheticDB(t)

	params, err := synthetic.ProfileParams(synthetic.ProfileMinimalScoped)
	require.NoError(t, err)
	params.Namespace = syntheticNS(t)

	// Use the explicit-teardown constructor so we can clean our own namespace
	// after asserting the leave-behind (the *testing.T constructor auto-cleans on
	// t.Cleanup, which would mask the leave-behind assertion).
	h, teardown, err := synthetic.NewHarnessWithDBForNamespace(ctx, database, params.Namespace, params.Seed)
	require.NoError(t, err)

	_, err = synthetic.RunProfile(ctx, h, params)
	require.NoError(t, err)

	// Success path: Quiesce stops the client but LEAVES the rows.
	require.NoError(t, h.Quiesce(ctx))
	remaining, err := h.ContactsRemaining(ctx)
	require.NoError(t, err)
	require.Greater(t, remaining, int64(0), "Quiesce must leave the seeded rows standing")

	// Clean our own namespace so the shared DB does not accumulate.
	require.NoError(t, teardown(context.Background()))
	gone, err := h.ContactsRemaining(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(0), gone, "the namespace's own teardown removes its rows")
}

// TestSyntheticProfile_ErrorPathTearsDown proves the error-path lifecycle: when
// a profile run fails partway, the entrypoint contract is to run the FULL
// teardown (stop client + clean the partial world) — a failed seed is never a
// leave-behind, and the River client is always stopped. This mirrors crm-admin's
// runSeed error branch.
func TestSyntheticProfile_ErrorPathTearsDown(t *testing.T) {
	testsupport.RequireLongTests(t)
	database, ctx := newSyntheticDB(t)

	params, err := synthetic.ProfileParams(synthetic.ProfileDev)
	require.NoError(t, err)
	params.Namespace = syntheticNS(t)

	h, teardown, err := synthetic.NewHarnessWithDBForNamespace(ctx, database, params.Namespace, params.Seed)
	require.NoError(t, err)

	// Seed a PARTIAL world so there is something to clean, then exercise the
	// teardown contract the orchestration's error branch invokes: a failed seed
	// runs the FULL teardown (stop client + clean the partial namespace).
	gen := h.Generator()
	spec := gen.Contact(factory.WithEmail())
	contact, err := h.SeedContact(ctx, spec)
	require.NoError(t, err)
	_, err = h.ReplayGmail(ctx, contact.ID, gen.GmailMessage(spec, factory.MatchSeeded))
	require.NoError(t, err)

	before, err := h.ContactsRemaining(ctx)
	require.NoError(t, err)
	require.Greater(t, before, int64(0))

	// The runSeed error branch: on a forced failure the entrypoint runs the full
	// teardown closure rather than Quiesce.
	require.NoError(t, teardown(context.Background()), "teardown after a forced failure must succeed")

	after, err := h.ContactsRemaining(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(0), after, "a failed seed must clean its partial world (no leave-behind)")
}

// TestSyntheticProfile_SettlesUnderHighAcceleration guards the real-wall-clock
// settle/teardown budget: the harness uses a harness-local monotonic timer, so a
// high TIME_ACCELERATION — under which "30 accelerated-seconds" would otherwise
// collapse to a sub-second real budget — does NOT cause a spurious settle
// timeout. A minimal-scoped seed still settles and its teardown still clears
// under acceleration 1000.
func TestSyntheticProfile_SettlesUnderHighAcceleration(t *testing.T) {
	testsupport.RequireLongTests(t)
	database, ctx := newSyntheticDB(t)

	// Pin an accelerated clock for this test's process: GetCurrentTime reads
	// TIME_ACCELERATION + TIME_BASE from the process env directly. A FIXED
	// Unix-second base keeps the fixture deterministic (no wall-clock call — the
	// core rule is absolute for tests). The test only asserts that the
	// real-wall-clock settle/teardown budget does not collapse under acceleration;
	// the accelerated domain "now" the base produces is irrelevant to that.
	t.Setenv("TIME_BASE", strconv.FormatInt(fixedAccelBaseUnix, 10))
	t.Setenv("TIME_ACCELERATION", "1000")

	params, err := synthetic.ProfileParams(synthetic.ProfileMinimalScoped)
	require.NoError(t, err)
	params.Namespace = syntheticNS(t)

	// Explicit teardown so the settle+teardown budget is exercised end-to-end.
	h, teardown, err := synthetic.NewHarnessWithDBForNamespace(ctx, database, params.Namespace, params.Seed)
	require.NoError(t, err)

	_, err = synthetic.RunProfile(ctx, h, params)
	require.NoError(t, err, "a minimal-scoped seed must settle even under high acceleration")

	// Teardown (quiesce + Gate-B-gated cleanup) must clear, not spuriously time
	// out on the real-time budget.
	require.NoError(t, teardown(context.Background()))
	gone, err := h.ContactsRemaining(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(0), gone, "teardown must clear under high acceleration")
}
