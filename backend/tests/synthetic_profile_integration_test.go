package tests

import (
	"context"
	"strconv"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/synthetic"
	"personal-crm/backend/internal/synthetic/factory"
	"personal-crm/backend/tests/testsupport"

	"github.com/stretchr/testify/require"
)

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

	// Bio facts (item 4 how_met + item 5 real-year birthdays) ride on the
	// contact-create authority flip; the catalog carries ≥1 of each.
	require.GreaterOrEqual(t, res.ContactsWithBirthday, 1, "≥1 contact with a birthday bio fact")
	require.GreaterOrEqual(t, res.ContactsWithHowMet, 1, "≥1 contact with a how_met bio fact")

	// Accept/pending coverage (D6): assert the SPECIFIC review-policy outcomes, not
	// merely that some assertion of each status exists — health_condition
	// (always-confirm) must land PROPOSED (pending-review surface) and an
	// auto-if-confident text predicate (home_address/job_title/preference) must
	// land ACCEPTED (accepted-knowledge surface). Scoped to the namespace.
	assertions, err := support.ListAssertionsByNodePrefix(ctx, h.Generator().Prefix())
	require.NoError(t, err)
	acceptedTextPredicates := map[string]bool{"home_address": true, "job_title": true, "preference": true}
	var healthProposed, textAccepted bool
	for _, a := range assertions {
		if a.PredicateKey == "health_condition" && a.Status == repository.AssertionStatusProposed {
			healthProposed = true
		}
		if acceptedTextPredicates[a.PredicateKey] && a.Status == repository.AssertionStatusAccepted {
			textAccepted = true
		}
	}
	require.True(t, healthProposed, "health_condition (always-confirm) lands proposed (pending-review surface)")
	require.True(t, textAccepted, "an auto-if-confident text predicate lands accepted (accepted-knowledge surface)")

	remaining, err := h.ContactsRemaining(ctx)
	require.NoError(t, err)
	require.Greater(t, remaining, int64(0), "prod-shaped profile seeds contacts")
}

// TestSyntheticProfile_ProdShapedDeterministic proves the prod-shaped seed is
// deterministic: two runs at the same (namespace, seed, anchor) produce
// byte-identical ProfileResult counts AND an identical fingerprint of the
// generated assertion values. Count-pinning alone is insufficient — counts can
// match while a non-deterministic source (map iteration, wall-clock leak) shifts
// generated values — so the (value_text, value_date) fingerprint is the drift
// detector. Both runs share the SAME namespace (with a full teardown between) so
// the ns-prefix + PRNG stream line up, AND a PINNED generator anchor so the
// anchor-relative timestamps (birthday date) are byte-identical too — without the
// pin, birthday value_date would legitimately drift between runs (anchor-relative
// per the factory contract) and could not be fingerprinted. proposition_key is
// NOT fingerprinted (it embeds the per-run subject UUID, so it is not run-stable).
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

	// run seeds the profile, captures (ProfileResult, assertion fingerprint), then
	// fully tears down so the next run starts from a clean namespace.
	run := func() (synthetic.ProfileResult, []repository.AssertionSummary) {
		h, teardown, err := synthetic.NewHarnessWithDBForNamespaceAt(ctx, database, params.Namespace, params.Seed, anchor)
		require.NoError(t, err)
		res, err := synthetic.RunProfile(ctx, h, params)
		require.NoError(t, err)
		fp, err := support.ListAssertionsByNodePrefix(ctx, h.Generator().Prefix())
		require.NoError(t, err)
		require.NoError(t, teardown(context.Background()))
		return res, fp
	}

	res1, fp1 := run()
	res2, fp2 := run()

	require.Equal(t, res1, res2, "prod-shaped ProfileResult must be deterministic across runs")
	require.NotEmpty(t, fp1, "the seed must produce assertions to fingerprint")
	require.Equal(t, fp1, fp2, "assertion (value_text, value_date) fingerprint must be deterministic across runs")
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
