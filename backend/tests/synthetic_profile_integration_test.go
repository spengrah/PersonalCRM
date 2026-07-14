package tests

import (
	"context"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/cadence"
	"personal-crm/backend/internal/contacttask"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/synthetic"
	"personal-crm/backend/internal/synthetic/factory"
	"personal-crm/backend/tests/testsupport"

	"github.com/google/uuid"
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
// assertSeedCoherence runs the post-seed coherence gate over one namespace: no
// production-impossible last_contacted (gate a), a non-vacuous interaction-moved
// cohort (gate a-positive), prod-shaped managed follow-ups (gate c) whose marker
// decodes and points at the seeded contact (gate c-Go), a cause-driven overdue
// cohort computed via the production cadence helper (env-robust), the CAD-029
// tour capacity (four distinct assignable contacts), and no stranded
// knowledge-cache columns (gate f: every current-accepted cutover assertion has a
// populated cache column, and — when dateFactID is set — the specific target row,
// the ReplayAssertion date-fact birthday's own contact, has a populated birthday
// cache). Called by both coverage tests. dateFactID is h.DateFactContactID() (the
// contact the date-fact block seeded; uuid.Nil when that block did not run).
func assertSeedCoherence(t *testing.T, ctx context.Context, support *repository.SyntheticSupportRepository, prefix string, dateFactID uuid.UUID) {
	t.Helper()
	now := accelerated.GetCurrentTime()

	// Gate (a): zero production-impossible last_contacted (a non-creation, non-null
	// last_contacted must be backed by a live inbound/mutual interaction at the same
	// occurred_at, with last_interaction_at in lockstep).
	incoherent, err := support.IncoherentLastContactedCountByNamePrefix(ctx, prefix)
	require.NoError(t, err)
	require.Equal(t, int64(0), incoherent, "no contact may carry a moved last_contacted without a backing interaction + lockstep last_interaction_at")

	// Gate (a-positive): the interaction-moved cohort exists (proves gate a is not
	// vacuous) and sets last_interaction_at in lockstep.
	backed, err := support.InteractionBackedLastContactedCountByNamePrefix(ctx, prefix)
	require.NoError(t, err)
	require.GreaterOrEqual(t, backed, int64(1), "≥1 contact whose last_contacted is backed by an inbound/mutual interaction with lockstep last_interaction_at")

	// Gate (c): managed follow-ups carry the production Todoist shape.
	incoherentFU, err := support.IncoherentManagedFollowUpCountByNamePrefix(ctx, prefix)
	require.NoError(t, err)
	require.Equal(t, int64(0), incoherentFU, "every managed follow-up loop must carry an alphanumeric external id + prod metadata + content/marker shape")

	// Gate (c-Go): the follow-up's marker decodes and references its own contact.
	fu, err := support.GetLiveFollowUpByNamePrefix(ctx, prefix)
	require.NoError(t, err)
	var meta map[string]any
	require.NoError(t, json.Unmarshal(fu.Metadata, &meta))
	markerJSON, _ := meta["marker_json"].(string)
	marker, ok := contacttask.DecodeMarker(markerJSON)
	require.True(t, ok, "follow-up marker_json must decode")
	require.Equal(t, fu.ContactID.String(), marker.ContactID, "marker must reference the follow-up's own contact")
	require.Equal(t, contacttask.KindReachOut, marker.Kind)
	require.Equal(t, contacttask.LifecycleFollowUpLoop, marker.Lifecycle)
	instanceID, _ := meta["integration_instance_id"].(string)
	require.NotEmpty(t, marker.Instance, "marker instance must be non-empty")
	require.Equal(t, instanceID, marker.Instance, "marker instance must agree with integration_instance_id metadata")
	content, _ := meta["content"].(string)
	require.Contains(t, content, "/contacts/"+fu.ContactID.String(), "follow-up content must link the contact")

	// Cause-driven overdue cohort + the D1 verification, computed from the bucket
	// rows via the production cadence helper (env-robust: it does not matter whether
	// the harness runs with time acceleration on).
	buckets, err := support.ListContactBucketsByNamePrefix(ctx, prefix)
	require.NoError(t, err)
	overdueFloor := now.Add(-14 * 24 * time.Hour)
	var overdueByHelper, backdatedOverdue int
	for _, b := range buckets {
		if b.Cadence == nil || *b.Cadence == "" || b.CreatedAt == nil {
			continue
		}
		cadenceType, perr := cadence.ParseCadence(*b.Cadence)
		if perr != nil {
			continue
		}
		if cadence.IsOverdueWithConfig(cadenceType, b.LastContacted, *b.CreatedAt, now) {
			overdueByHelper++
		}
		// D1: a backdated (created-long-ago) contact is overdue with an honest empty
		// timeline — created_at far in the past, last_contacted == created_at (the
		// creation stamp), and a computed contact_by already elapsed.
		if b.LastContacted != nil && b.LastContacted.Equal(*b.CreatedAt) &&
			b.CreatedAt.Before(overdueFloor) &&
			b.ContactBy != nil && b.ContactBy.Before(now) {
			backdatedOverdue++
		}
	}
	require.GreaterOrEqual(t, overdueByHelper, 1, "≥1 overdue contact via the production cadence helper")
	require.GreaterOrEqual(t, backdatedOverdue, 1, "≥1 backdated overdue contact (far-past created_at == last_contacted, contact_by elapsed)")

	// Tour capacity (CAD-029): the world must contain FOUR DISTINCT contacts
	// assignable to the outreach / response / pending / none states — the precondition
	// for cadence-followup.tour.ts. Proven via a maximum bipartite matching that must
	// saturate all four states.
	flags, err := support.ListCadenceActivityFlagsByNamePrefix(ctx, prefix)
	require.NoError(t, err)
	require.Equal(t, 4, maxDistinctStateAssignment(flags), "four CAD-029 states must be assignable to four distinct contacts")

	// Gate (f): zero stranded knowledge-cache columns (every current-accepted cutover
	// assertion on a live contact has its derived cache column populated — the
	// production-impossible state where an accepted assertion exists but its cache
	// column is NULL is absent).
	stranded, err := support.StrandedKnowledgeCacheCountByNamePrefix(ctx, prefix, now)
	require.NoError(t, err)
	require.Equal(t, int64(0), stranded, "no live contact may hold a current-accepted cutover assertion (lives_in/birthday/how_met) whose derived cache column is NULL")

	// Gate (f-positive): the specific target row is coherent. A generic ">=1 populated
	// cutover cache" would pass on the contact-create authority-flip bio facts alone, so
	// target the ReplayAssertion date-fact birthday's OWN contact and assert its birthday
	// cache is populated — proving the refresh reaches the exact row that would otherwise
	// be stranded, not just that some cutover cache is populated. (lives_in/how_met
	// non-vacuity rides the existing ContactsWithLocation/ContactsWithHowMet coverage
	// assertions feeding gate f.)
	if dateFactID != uuid.Nil {
		cache, err := support.GetContactCacheColumns(ctx, dateFactID)
		require.NoError(t, err)
		require.NotNil(t, cache.Birthday, "the ReplayAssertion date-fact birthday's contact must have a populated birthday cache (target row)")
	}
}

// maxDistinctStateAssignment returns the size of a maximum matching between the
// four CAD-029 states (outreach, response, pending, none) and the given contacts,
// where a state may be assigned to any contact carrying that flag and each contact
// covers at most one state. Standard augmenting-path (Kuhn's) search — trivial at
// this scale.
func maxDistinctStateAssignment(flags []repository.CadenceActivityFlags) int {
	// state index → the flag each state requires on a contact.
	stateHas := []func(repository.CadenceActivityFlags) bool{
		func(f repository.CadenceActivityFlags) bool { return f.HasOutreach },
		func(f repository.CadenceActivityFlags) bool { return f.HasResponse },
		func(f repository.CadenceActivityFlags) bool { return f.HasPending },
		func(f repository.CadenceActivityFlags) bool { return f.HasNone },
	}
	contactToState := make([]int, len(flags))
	for i := range contactToState {
		contactToState[i] = -1
	}
	var augment func(state int, seen []bool) bool
	augment = func(state int, seen []bool) bool {
		for ci, f := range flags {
			if !stateHas[state](f) || seen[ci] {
				continue
			}
			seen[ci] = true
			if contactToState[ci] == -1 || augment(contactToState[ci], seen) {
				contactToState[ci] = state
				return true
			}
		}
		return false
	}
	matched := 0
	for s := 0; s < len(stateHas); s++ {
		if augment(s, make([]bool, len(flags))) {
			matched++
		}
	}
	return matched
}

// f4MessageSources is the exact message-source allowlist the two-sided
// direction gate (F4, d-positive) asserts non-inbound coverage for. F4 measured 0
// outbound/mutual interactions for EACH of these four sources. It DELIBERATELY
// excludes gcal — the awaiting-reply GCal event is already mutual, so including it
// would let the per-source assertion pass without any message-source direction
// coverage — plus todoist / manual / anarlog_sessions / phone_calls.
var f4MessageSources = []string{
	repository.InteractionSourceEmail,
	repository.InteractionSourceTelegram,
	repository.InteractionSourceGChat,
	repository.InteractionSourceMessages,
}

// assertMessageDirectionCoverage runs the F4 two-sided message-direction gate over
// one namespace: no last_outreach_at without a backing outbound/mutual interaction
// (gate d), each of the four message sources carries ≥1 outbound/mutual interaction
// (gate d-positive, was 0/0/0/0), the seeded cohort counts match (3 outbound-only + 1
// mutual), the telegram-mutual contact's two seeded messages collapsed into exactly
// one promoted mutual row (gate d-mutual-verify), and that promoted mutual set all
// four cadence timestamps non-null and equal. Called by both coverage tests.
func assertMessageDirectionCoverage(t *testing.T, ctx context.Context, support *repository.SyntheticSupportRepository, h *synthetic.Harness, res synthetic.ProfileResult) {
	t.Helper()
	prefix := h.Generator().Prefix()

	// Gate (d): zero last_outreach_at values not backed by a live outbound/mutual
	// interaction at the same occurred_at.
	incoherentOut, err := support.IncoherentOutreachCountByNamePrefix(ctx, prefix)
	require.NoError(t, err)
	require.Equal(t, int64(0), incoherentOut, "no contact may carry a moved last_outreach_at without a backing outbound/mutual interaction")

	// Gate (d, not-vacuous): the zero-violation check above is only meaningful if the
	// world actually contains last_outreach_at values. Assert the PURE-OUTBOUND cohort
	// exists (last_outreach_at set, last_contacted NULL) — the three outbound-only
	// contacts (gmail/gchat/imessage). Without this a world with no last_outreach_at at
	// all would satisfy the == 0 gate vacuously.
	pureOutbound, err := support.PureOutboundContactCountByNamePrefix(ctx, prefix)
	require.NoError(t, err)
	require.GreaterOrEqual(t, pureOutbound, int64(1), "≥1 pure-outbound contact (last_outreach_at set, last_contacted NULL) — the outbound gate is not vacuous")

	// Gate (d-positive): EACH message source has ≥1 outbound/mutual interaction (the
	// F4 fix is per-source; every one measured 0 before this).
	for _, source := range f4MessageSources {
		n, err := support.NonInboundMessageInteractionCountBySourceByNamePrefix(ctx, prefix, source)
		require.NoError(t, err)
		require.GreaterOrEqual(t, n, int64(1), "message source %q must carry ≥1 outbound/mutual interaction (F4)", source)
	}

	// The block seeds a fixed cohort: 3 outbound-only (gmail/gchat/imessage) + 1
	// telegram mutual, in both profiles.
	require.Equal(t, 3, res.OutboundOnlyContacts, "three outbound-only message contacts seeded")
	require.Equal(t, 1, res.MutualMessageContacts, "one reply-bridged telegram mutual contact seeded")

	// Gate (d-mutual-verify): the telegram-mutual contact's two seeded messages must
	// have COLLAPSED into a single promoted mutual row — proving
	// PromoteInteractionToMutualTx ran, not that an outbound and an inbound coexist.
	mutualID := h.MutualMessageContactID()
	require.NotEqual(t, uuid.Nil, mutualID, "the two-sided block must have recorded the mutual contact id")
	dirCounts, err := support.InteractionDirectionCountsForContact(ctx, mutualID, repository.InteractionSourceTelegram)
	require.NoError(t, err)
	require.Equal(t, map[string]int64{repository.InteractionDirectionMutual: 1}, dirCounts,
		"telegram-mutual contact must have exactly one live mutual interaction and no residual outbound/inbound rows")

	// Gate (d-mutual-verify, timestamps): the promoted mutual set all four cadence
	// columns together (mutual writes last_contacted / last_interaction_at /
	// last_outreach_at / last_response_at to the same replyAt).
	ts, err := support.ContactCadenceTimestampsForContact(ctx, mutualID)
	require.NoError(t, err)
	require.NotNil(t, ts.LastContacted, "mutual contact last_contacted must be set")
	require.NotNil(t, ts.LastInteractionAt, "mutual contact last_interaction_at must be set")
	require.NotNil(t, ts.LastOutreachAt, "mutual contact last_outreach_at must be set")
	require.NotNil(t, ts.LastResponseAt, "mutual contact last_response_at must be set")
	require.True(t, ts.LastContacted.Equal(*ts.LastInteractionAt), "mutual last_contacted == last_interaction_at")
	require.True(t, ts.LastContacted.Equal(*ts.LastOutreachAt), "mutual last_contacted == last_outreach_at")
	require.True(t, ts.LastContacted.Equal(*ts.LastResponseAt), "mutual last_contacted == last_response_at")
}

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
	// count, not the >0 gate value in Counts.SeededTasks), and one is driven to
	// `unmanaged` via the real recurring edit. Same F7 coherence gate as the
	// prod-shaped profile: both prod-reachable states present, zero prod-impossible
	// (completed/dismissed or non-finalized external id) cadence_due rows.
	require.GreaterOrEqual(t, res.SeededTasks, 1, "dev profile seeds cadence tasks")
	{
		cadenceSupport := repository.NewSyntheticSupportRepository(database.Queries)
		devPrefix := h.Generator().Prefix()
		managedCadence, err := cadenceSupport.CountCadenceDueByStateByNamePrefix(ctx, string(repository.ContactTaskStateManaged), devPrefix)
		require.NoError(t, err)
		require.GreaterOrEqual(t, managedCadence, int64(1), "≥1 managed cadence_due task (reconcile default)")
		unmanagedCadence, err := cadenceSupport.CountCadenceDueByStateByNamePrefix(ctx, string(repository.ContactTaskStateUnmanaged), devPrefix)
		require.NoError(t, err)
		require.GreaterOrEqual(t, unmanagedCadence, int64(1), "≥1 unmanaged cadence_due task (real recurring-edit path)")
		incoherentCadence, err := cadenceSupport.IncoherentCadenceDueCountByNamePrefix(ctx, devPrefix)
		require.NoError(t, err)
		require.Equal(t, int64(0), incoherentCadence, "no cadence_due task may be completed/dismissed or carry a non-finalized external id")
	}
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
	// MessagesPerContact spreads MULTIPLE settled interactions per dedicated source
	// contact: SettledInteractions == (settled contacts) × MessagesPerContact — PLUS the
	// single GCal event the awaiting-reply scenario replays (one per seeded follow-up).
	// That event is the OUTBOUND half of the scenario's causal chain: mutual direction
	// (CAD-006) gives the contact the last_outreach_at a follow-up requires (CAD-011).
	settledContacts := res.GmailSettled + res.TelegramSettled + res.GCalSettled + res.GChatSettled + res.IMessageSettled
	require.Equal(t, settledContacts*params.Counts.MessagesPerContact+res.SeededPendingFollowUps, res.SettledInteractions,
		"dev profile spreads MessagesPerContact interactions per settled contact, plus the awaiting-reply scenario's GCal event")
	require.Greater(t, res.SettledInteractions, settledContacts, "dev profile seeds >1 interaction per settled contact")
	// Merge + soft-delete scenarios are standalone contacts seeded at full count
	// (independent of catalog size), so the dev result equals the dev knobs.
	require.Equal(t, params.Counts.SeededSoftDeleted, res.SeededSoftDeleted, "dev profile seeds soft-deleted contacts")
	require.Equal(t, params.Counts.SeededMerged, res.SeededMerged, "dev profile seeds merged contact pairs")

	// Import-source candidates: one per subtab per ImportCandidatesPerSource, seeded at
	// full count (independent of catalog size), so each result equals the dev knob.
	require.Equal(t, params.Counts.ImportCandidatesPerSource, res.UnmatchedGContacts, "dev profile seeds gcontacts candidates")
	require.Equal(t, params.Counts.ImportCandidatesPerSource, res.UnmatchedGmailCorrespondence, "dev profile seeds gmail_correspondence candidates")
	require.Equal(t, params.Counts.ImportCandidatesPerSource, res.UnmatchedAnarlogHumans, "dev profile seeds anarlog_humans candidates")
	require.Equal(t, params.Counts.ImportCandidatesPerSource, res.TelegramDiscovery, "dev profile seeds telegram discovery candidates")

	remaining, err := h.ContactsRemaining(ctx)
	require.NoError(t, err)
	require.Greater(t, remaining, int64(0), "dev profile seeds catalog contacts")

	// Post-seed coherence gate (F1/F3 + tour capacity), scoped to the namespace.
	support := repository.NewSyntheticSupportRepository(database.Queries)
	assertSeedCoherence(t, ctx, support, h.Generator().Prefix(), h.DateFactContactID())
	// Two-sided message-direction coverage (F4).
	assertMessageDirectionCoverage(t, ctx, support, h, res)
	// Notes coverage (F6).
	assertNotesCoverage(t, ctx, support, h, res, params.Counts.SeededContacts)
}

// assertNotesCoverage runs the F6 notes gate: the profile seeds a note on a
// deterministic fraction (every third catalog contact by index, ⌈n/3⌉ total), so the
// result count must equal ⌈seededContacts/3⌉ AND the notepad notes must actually be
// present in the DB for that many contacts (namespace-scoped). Tying the expectation to
// the catalog size — not a floor — catches a constant-count regression (e.g. "always
// seed 2") that would leave a prod-shaped catalog near-empty. Called by both coverage
// tests.
func assertNotesCoverage(t *testing.T, ctx context.Context, support *repository.SyntheticSupportRepository, h *synthetic.Harness, res synthetic.ProfileResult, seededContacts int) {
	t.Helper()
	expectedNotes := (seededContacts + 2) / 3 // ⌈seededContacts/3⌉ (the i%3==0 subset)
	require.Equal(t, expectedNotes, res.ContactsWithNotes, "notes seeded on ⌈catalog/3⌉ contacts")
	notepadContacts, err := support.ContactsWithNotepadCountByNamePrefix(ctx, h.Generator().Prefix())
	require.NoError(t, err)
	require.Equal(t, int64(res.ContactsWithNotes), notepadContacts, "non-empty notepad notes present in the DB for exactly the seeded contacts")
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
	// cadence-bearing, so reconcile creates 9 `managed` tasks and one is driven to
	// `unmanaged` via the real recurring-edit path — the two states cadence_due can
	// hold in prod (completed/dismissed are unreachable for this lifecycle).
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
	// Two settled interactions per dedicated source contact (the CI-safe minimum
	// that still exercises the temporal spread): the second message is one
	// contact-indexed spread gap (>= 9 days) older, so the per-contact span clears the
	// multi-day floor asserted below. Kept small to bound the settle budget.
	params.Counts.MessagesPerContact = 2
	// One soft-deleted contact + one merged pair (the CI-safe minimum): enough to
	// assert the tombstone + assertion re-point invariants below.
	params.Counts.SeededSoftDeleted = 1
	params.Counts.SeededMerged = 1
	// One candidate per Imports subtab (gcontacts/gmail_correspondence/anarlog_humans/
	// telegram-discovery): the CI-safe minimum that still gives each subtab ≥1 queue
	// entry. Kept at 1 to bound the settle budget (telegram discovery replays 3 group
	// messages + settles; anarlog ingests + settles).
	params.Counts.ImportCandidatesPerSource = 1

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

	// "Overdue" must mean a last_contacted WELL in the past (the backdated cohort
	// is created ~90 days ago, stamping last_contacted == created_at), NOT merely
	// before now — a recently-created contact (<48h ago) is also before now. Using a
	// 14-day-ago floor distinguishes the overdue bucket from the recent bucket, so a
	// clobber-to-recent regression (the bug this guards) would FAIL here.
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

	// Overdue-cohort DIVERSITY (DSH-010): the overdue surface must show a RANGE of
	// days-overdue and cadences, not a single monthly / ~60-day monoculture the
	// dashboard urgency tiers cannot separate. Select the backdated cohort
	// STRUCTURALLY — cadence set, last_contacted == created_at (the backdated creation
	// stamp), created_at older than a fixed floor (now − 7d, which deterministically
	// excludes the <48h recent cohort in EVERY env) — so this does NOT depend on the
	// env-reading cadence helper, which collapses days-overdue under compressed test
	// durations. Assert ≥3 distinct created-ages AND ≥2 distinct cadences; this is the
	// assertion that would have caught the pre-change monoculture (all overdue slots
	// were monthly + 90d), and it is env-independent by construction.
	diversityFloor := now.Add(-7 * 24 * time.Hour)
	distinctCreatedAges := map[int64]bool{}
	distinctOverdueCadences := map[string]bool{}
	for _, b := range buckets {
		if b.Cadence == nil || *b.Cadence == "" || b.CreatedAt == nil || b.LastContacted == nil {
			continue
		}
		if !b.LastContacted.Equal(*b.CreatedAt) || !b.CreatedAt.Before(diversityFloor) {
			continue
		}
		distinctCreatedAges[b.CreatedAt.UnixNano()] = true
		distinctOverdueCadences[*b.Cadence] = true
	}
	require.GreaterOrEqual(t, len(distinctCreatedAges), 3, "overdue cohort spans ≥3 distinct created-ages (days-overdue diversity, not a monoculture)")
	require.GreaterOrEqual(t, len(distinctOverdueCadences), 2, "overdue cohort spans ≥2 distinct cadences")

	// Post-seed coherence gate (F1/F3 + tour capacity), scoped to the namespace.
	assertSeedCoherence(t, ctx, support, h.Generator().Prefix(), h.DateFactContactID())
	// Two-sided message-direction coverage (F4).
	assertMessageDirectionCoverage(t, ctx, support, h, res)
	// Notes coverage (F6).
	assertNotesCoverage(t, ctx, support, h, res, params.Counts.SeededContacts)

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

	// Cadence tasks: ReplayTodoist seeds a `managed` cadence_due task on each
	// cadence-bearing catalog contact, and ReplayTodoistRecurringEdit drives one to
	// `unmanaged` through the real recurring-edit path. cadence_due has exactly two
	// prod-reachable persistent states — `managed` and `unmanaged` — so assert BOTH
	// are present (namespace-scoped) and that NO cadence_due row is in a prod-
	// impossible shape (gate e): `completed`/`dismissed` states (a completed row is
	// deleted by the next reconcile; a skip routes to a managed replacement, never to
	// dismissed) or a non-finalized external id (a raw UUID / lingering
	// pending_temp_id instead of a Todoist-v1 alphanumeric).
	require.GreaterOrEqual(t, res.SeededTasks, 1, "cadence tasks seeded")
	managedCadence, err := support.CountCadenceDueByStateByNamePrefix(ctx, string(repository.ContactTaskStateManaged), prefix)
	require.NoError(t, err)
	require.GreaterOrEqual(t, managedCadence, int64(1), "≥1 managed cadence_due task (reconcile default)")
	unmanagedCadence, err := support.CountCadenceDueByStateByNamePrefix(ctx, string(repository.ContactTaskStateUnmanaged), prefix)
	require.NoError(t, err)
	require.GreaterOrEqual(t, unmanagedCadence, int64(1), "≥1 unmanaged cadence_due task (real recurring-edit path)")
	incoherentCadence, err := support.IncoherentCadenceDueCountByNamePrefix(ctx, prefix)
	require.NoError(t, err)
	require.Equal(t, int64(0), incoherentCadence, "no cadence_due task may be completed/dismissed or carry a non-finalized external id")

	// The "awaiting reply" state (has_pending_followup). REGRESSION GUARD, not a nicety:
	// a seeded world cannot reach this state through the production path (FollowUpManager
	// is off-mode in the harness; CAD-012 suppresses follow-ups for backdated automated
	// outbounds), so before it was seeded explicitly the state was ABSENT from every
	// seeded world. The tours therefore could not capture it, and the agentic judge —
	// shown only contact pages with no "Awaiting reply" marker — concluded the feature did
	// not exist and emitted a confident, well-cited, FALSE CAD-036 regression on every
	// run. Absence of evidence read as absence of the feature. If this assertion ever goes
	// back to zero, that false verdict returns.
	require.Equal(t, 1, res.SeededPendingFollowUps, "profile seeds exactly one live follow-up")
	liveFollowUps, err := support.CountLiveFollowUpsByNamePrefix(ctx, prefix)
	require.NoError(t, err)
	// The query requires last_outreach_at IS NOT NULL — a follow-up is opened BY an outbound
	// (CAD-011), so one on a contact with no outbound is a state production cannot reach.
	// The first version of this seed produced exactly that incoherent world, and the judge
	// (correctly) failed the contact page for showing "Awaiting reply" with nothing to be
	// awaiting a reply to. Asserting COHERENCE, not just presence, is the point.
	require.GreaterOrEqual(t, liveFollowUps, int64(1),
		"≥1 COHERENT live followup_loop task (on a contact with an outbound) — the has_pending_followup state the contact page renders")

	// Interaction volume: each dedicated source contact carries MessagesPerContact
	// settled interactions, so SettledInteractions == (settled contacts) ×
	// MessagesPerContact — PLUS the single GCal event the awaiting-reply scenario
	// replays (one per seeded follow-up), which is a real settled interaction and is
	// counted as one. It is the OUTBOUND half of that scenario's causal chain: mutual
	// direction (CAD-006) is what gives the contact the last_outreach_at a follow-up
	// requires (CAD-011), so it is not bookkeeping — it is the reason the state is
	// coherent at all.
	settledContacts := res.GmailSettled + res.TelegramSettled + res.GCalSettled + res.GChatSettled + res.IMessageSettled
	require.Equal(t, settledContacts*params.Counts.MessagesPerContact+res.SeededPendingFollowUps, res.SettledInteractions,
		"MessagesPerContact interactions per settled contact, plus the awaiting-reply scenario's GCal event")
	require.Greater(t, res.SettledInteractions, settledContacts, ">1 interaction per settled contact")

	// Interaction temporal spread (DB-level): ≥1 settled contact must carry ≥2
	// interactions whose earliest→latest span clears a multi-day floor — proving the
	// per-message age offsets landed them on distinct days (a prod-like history),
	// not a single ~1h window. Scoped to the namespace; the interaction-free
	// edge-case catalog contacts are excluded by the query's INNER JOIN.
	spreads, err := support.ListInteractionSpreadByContactNamePrefix(ctx, prefix)
	require.NoError(t, err)
	var spread bool
	for _, s := range spreads {
		if s.InteractionCount >= 2 && s.Span >= 7*24*time.Hour {
			spread = true
			break
		}
	}
	require.True(t, spread, "≥1 settled contact has ≥2 interactions spanning ≥7 days (temporal spread, not one window)")

	// Spacing NON-UNIFORMITY (F9): the settled history must not be one uniform gap
	// repeated across every contact (the pre-change shape was a fixed 21-day
	// interval). The spread ladder is contact-indexed, so even at
	// MessagesPerContact=2 (a single gap) different settled contacts carry different
	// spans. Assert ≥2 distinct span values across settled contacts with ≥2
	// interactions — the assertion that would have caught the uniform-spacing shape.
	distinctSpans := map[time.Duration]bool{}
	for _, s := range spreads {
		if s.InteractionCount >= 2 {
			distinctSpans[s.Span] = true
		}
	}
	require.GreaterOrEqual(t, len(distinctSpans), 2, "≥2 distinct interaction spans across settled contacts (non-uniform spacing, not a single fixed gap)")

	// Merge + soft-delete scenarios (item 12). These are standalone contacts seeded
	// at full count (independent of the catalog), so the result equals the override.
	require.Equal(t, params.Counts.SeededSoftDeleted, res.SeededSoftDeleted, "soft-deleted contacts seeded")
	require.Equal(t, params.Counts.SeededMerged, res.SeededMerged, "merged contact pairs seeded")

	// Soft-delete invariant: the person node is tombstoned, so its assertion is
	// RETAINED in the table but DROPS from live graph reads. Scoped to THIS run's
	// tracked soft-delete node ids.
	softNodeIDs := h.SoftDeletedNodeIDs()
	require.Len(t, softNodeIDs, res.SeededSoftDeleted, "tracked soft-delete node ids match the result count")
	softInTable, err := support.CountAssertionsForSubjects(ctx, softNodeIDs)
	require.NoError(t, err)
	require.GreaterOrEqual(t, softInTable, int64(len(softNodeIDs)), "soft-deleted assertions retained in the table")
	softLive, err := support.CountLiveAssertionsForSubjects(ctx, softNodeIDs)
	require.NoError(t, err)
	require.Zero(t, softLive, "soft-deleted assertions dropped from live graph reads")
	softLiveNodes, err := support.CountLiveNodesByIds(ctx, softNodeIDs)
	require.NoError(t, err)
	require.Zero(t, softLiveNodes, "soft-deleted person nodes tombstoned (not live)")

	// Merge invariant: the loser node is tombstoned with merged_into set, its
	// assertions are re-pointed OFF the loser onto the live winner, and the winner
	// stays live carrying its own + the re-pointed assertions. Scoped to THIS run's
	// tracked merge node ids.
	loserIDs := h.MergedLoserNodeIDs()
	winnerIDs := h.MergedWinnerNodeIDs()
	require.Len(t, loserIDs, res.SeededMerged, "tracked merge-loser node ids match the pair count")
	require.Len(t, winnerIDs, res.SeededMerged, "tracked merge-winner node ids match the pair count")
	loserMerged, err := support.CountMergedIntoNodesByIds(ctx, loserIDs)
	require.NoError(t, err)
	require.Equal(t, int64(len(loserIDs)), loserMerged, "every merge-loser node carries merged_into")
	loserLiveNodes, err := support.CountLiveNodesByIds(ctx, loserIDs)
	require.NoError(t, err)
	require.Zero(t, loserLiveNodes, "merge-loser nodes tombstoned (not live)")
	loserAssertions, err := support.CountAssertionsForSubjects(ctx, loserIDs)
	require.NoError(t, err)
	require.Zero(t, loserAssertions, "merge-loser assertions re-pointed off the loser")
	winnerLiveNodes, err := support.CountLiveNodesByIds(ctx, winnerIDs)
	require.NoError(t, err)
	require.Equal(t, int64(len(winnerIDs)), winnerLiveNodes, "merge-winner nodes stay live")
	winnerLiveAssertions, err := support.CountLiveAssertionsForSubjects(ctx, winnerIDs)
	require.NoError(t, err)
	require.GreaterOrEqual(t, winnerLiveAssertions, int64(2*len(winnerIDs)),
		"merge-winner carries its own + the re-pointed loser assertion (≥2 per pair)")

	// Import-source candidates (item 13): EVERY Imports subtab must have ≥1 unmatched
	// candidate so staging's Imports queue is populated. Assert both the result counter
	// and the actual DB row, scoped to THIS namespace. gcontacts/gmail_correspondence/
	// anarlog_humans carry an ns-prefixed source_id; telegram discovery keys on the bare
	// peer id, so it is scoped by the namespace's reserved peer band instead.
	require.GreaterOrEqual(t, res.UnmatchedGContacts, 1, "gcontacts candidate seeded")
	require.GreaterOrEqual(t, res.UnmatchedGmailCorrespondence, 1, "gmail_correspondence candidate seeded")
	require.GreaterOrEqual(t, res.UnmatchedAnarlogHumans, 1, "anarlog_humans candidate seeded")
	require.GreaterOrEqual(t, res.TelegramDiscovery, 1, "telegram discovery candidate seeded")
	for _, source := range []string{"gcontacts", "gmail_correspondence", "anarlog_humans"} {
		count, err := support.CountUnmatchedExternalContactBySourceAndPrefix(ctx, source, prefix)
		require.NoError(t, err)
		require.GreaterOrEqual(t, count, int64(1), "≥1 unmatched %q import candidate present", source)
	}
	tgDiscovery, err := support.CountUnmatchedTelegramDiscoveryInBand(ctx, h.Generator().PeerBandStart(), h.Generator().PeerBandEnd())
	require.NoError(t, err)
	require.GreaterOrEqual(t, tgDiscovery, int64(1), "≥1 unmatched telegram discovery candidate present")

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
	// One settled message per source contact: the temporal-spread timestamps are a
	// pure function of the (pinned) anchor + message index, so they are
	// deterministic by construction and not fingerprinted here (interactions are not
	// in the fingerprint); keeping this at 1 holds the settle load at baseline while
	// res.SettledInteractions still participates in the run-to-run count equality.
	params.Counts.MessagesPerContact = 1
	params.Counts.UnmatchedExternal = 1
	params.Counts.StrandedTelegram = 1
	params.Counts.StrandedMessages = 1
	params.Counts.UnmatchedCalendar = 1
	params.Counts.OrphanMeetingNote = 1
	// One soft-deleted contact + one merged pair so the run-to-run ProfileResult
	// count is exercised and the teardown of the tombstoned + merged rows (the
	// merged_into self-FK is the trickiest sweep) is asserted FK-clean below.
	params.Counts.SeededSoftDeleted = 1
	params.Counts.SeededMerged = 1
	// One candidate per Imports subtab so the import-producer source_ids participate in
	// the determinism fingerprint (the direct-upsert + ingest source_ids are ns-prefixed)
	// and the run-to-run ProfileResult count equality, and the teardown of the
	// external_contact candidates is asserted clean below.
	params.Counts.ImportCandidatesPerSource = 1

	// Capture the generator anchor ONCE and reuse it for both runs: identical
	// across runs (so timestamped output incl. birthday value_date is
	// byte-identical and the fingerprint covers it) yet ≈now so the settle
	// pipeline — which runs on the real system clock — is not skewed by a far-past
	// anchor (a far-past anchor starves Gate A: the consumers never link the
	// stale-dated messages within the real-time budget).
	anchor := accelerated.GetCurrentTime()
	support := repository.NewSyntheticSupportRepository(database.Queries)

	// run seeds the profile, captures (ProfileResult, assertion fingerprint, entity
	// normalized_name fingerprint, import-candidate source_id fingerprint), then fully
	// tears down so the next run starts from a clean namespace. It also asserts the
	// teardown swept every entity node (pool + place) — a leaked entity node would
	// FK-block or duplicate-violate the next run, and a surviving place node from a
	// `within` edge would FK-block the sweep — and every import-candidate row.
	run := func() (synthetic.ProfileResult, []repository.AssertionSummary, []repository.EntityNameSummary, []repository.ExternalContactSummary) {
		h, teardown, err := synthetic.NewHarnessWithDBForNamespaceAt(ctx, database, params.Namespace, params.Seed, anchor)
		require.NoError(t, err)
		res, err := synthetic.RunProfile(ctx, h, params)
		require.NoError(t, err)
		prefix := h.Generator().Prefix()
		fp, err := support.ListAssertionsByNodePrefix(ctx, prefix)
		require.NoError(t, err)
		entFP, err := support.ListEntityNamesByNodePrefix(ctx, prefix)
		require.NoError(t, err)
		// Import-producer fingerprint: the ns-prefixed external_contact source_ids
		// (gcontacts/gmail_correspondence/anarlog_humans + the existing icloud/gcal
		// candidates). The telegram discovery candidate keys on the bare peer id (not
		// the prefix), so it is excluded here; its determinism rides the deterministic
		// peer band + the run-to-run ProfileResult count equality.
		extFP, err := support.ListExternalContactSourceIDsByPrefix(ctx, prefix)
		require.NoError(t, err)
		require.NoError(t, teardown(context.Background()))
		// Teardown correctness: the entity nodes (org/topic/tag pool + place nodes)
		// are swept, so the namespace is clean for the next run.
		remaining, err := support.ListEntityNamesByNodePrefix(ctx, prefix)
		require.NoError(t, err)
		require.Empty(t, remaining, "entity nodes (pool + place) swept by teardown")
		// Teardown correctness: the import-candidate external_contact rows are swept —
		// the ns-prefixed ones (gcontacts/gmail_correspondence/anarlog/icloud/gcal) by
		// the source_id-prefix sweep, the telegram discovery candidate (bare peer id) by
		// the telegram-peer sweep — so none remain for the next run.
		extRemaining, err := support.ListExternalContactSourceIDsByPrefix(ctx, prefix)
		require.NoError(t, err)
		require.Empty(t, extRemaining, "ns-prefixed external_contact candidates swept by teardown")
		tgDiscoveryRemaining, err := support.CountUnmatchedTelegramDiscoveryInBand(ctx, h.Generator().PeerBandStart(), h.Generator().PeerBandEnd())
		require.NoError(t, err)
		require.Zero(t, tgDiscoveryRemaining, "telegram discovery candidate swept by teardown")
		// Teardown correctness: the relationship_signal rows (FK→node, NO ACTION) are
		// swept BEFORE the node deletes, so none remain for the seeded subject nodes —
		// a leaked signal would FK-block the next run's person-node delete.
		sigRemaining, err := h.SignalsRemaining(ctx)
		require.NoError(t, err)
		require.Zero(t, sigRemaining, "relationship_signal rows swept by teardown")
		// Teardown correctness for merge + soft-delete: the require.NoError(teardown)
		// above already proves the merged_into self-FK did not block the
		// single-statement node sweep; this confirms every seeded contact (incl. the
		// tombstoned soft-deleted + merged-loser rows) was hard-deleted, leaving no
		// stranded node/assertion behind.
		contactsRemaining, err := h.ContactsRemaining(ctx)
		require.NoError(t, err)
		require.Zero(t, contactsRemaining, "all seeded contacts (incl. merge/soft-delete) swept by teardown")
		return res, fp, entFP, extFP
	}

	res1, fp1, ent1, ext1 := run()
	res2, fp2, ent2, ext2 := run()

	require.Equal(t, res1, res2, "prod-shaped ProfileResult must be deterministic across runs")
	require.NotEmpty(t, fp1, "the seed must produce assertions to fingerprint")
	require.Equal(t, fp1, fp2, "assertion (value_text, value_date, value_bool) fingerprint must be deterministic across runs")
	require.NotEmpty(t, ent1, "the seed must produce entity nodes to fingerprint")
	require.Equal(t, ent1, ent2, "entity (subtype, normalized_name) fingerprint must be deterministic across runs")
	require.NotEmpty(t, ext1, "the seed must produce import-candidate external_contact rows to fingerprint")
	require.Equal(t, ext1, ext2, "import-candidate (source, source_id) fingerprint must be deterministic across runs")

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
