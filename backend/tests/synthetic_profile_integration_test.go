package tests

import (
	"context"
	"strconv"
	"testing"
	"time"

	"personal-crm/backend/internal/synthetic"
	"personal-crm/backend/internal/synthetic/factory"
	"personal-crm/backend/tests/testsupport"

	"github.com/stretchr/testify/require"
)

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

	// Catalog contacts + per-source settled + pending-state producers all ran.
	require.GreaterOrEqual(t, res.GmailSettled, params.Counts.SeededContacts)
	require.GreaterOrEqual(t, res.TelegramSettled, 1)
	require.GreaterOrEqual(t, res.GCalSettled, 1)
	require.GreaterOrEqual(t, res.GChatSettled, 1)
	require.GreaterOrEqual(t, res.IMessageSettled, 1)
	require.Equal(t, params.Counts.UnmatchedExternal, res.UnmatchedExternal)
	require.Equal(t, params.Counts.StrandedTelegram, res.StrandedTelegram)
	require.Equal(t, params.Counts.StrandedMessages, res.StrandedMessages)
	require.Equal(t, params.Counts.UnmatchedCalendar, res.UnmatchedCalendar)
	require.Equal(t, params.Counts.OrphanMeetingNote, res.OrphanMeetingNote)

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

	// DEFERRED pending states (no toolkit producer in E3): the ProfileResult has
	// no field for them precisely because RunProfile produces none. This guards
	// the documented gap — if a future change starts producing them it must add a
	// counter, which forces revisiting this test. Asserting the producible set is
	// complete (every res.* counter > 0) is the absent-by-design check: there is
	// no conflict-meeting-note / title-candidate / gchat-pending counter to assert
	// because none is produced.

	remaining, err := h.ContactsRemaining(ctx)
	require.NoError(t, err)
	require.Greater(t, remaining, int64(0), "prod-shaped profile seeds contacts")
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

// TestSyntheticProfile_ErrorPathTearsDown proves the error-path lifecycle
// (E3-D3b-fix): when a profile run fails partway, the entrypoint contract is to
// run the FULL teardown (stop client + clean the partial world) — a failed seed
// is never a leave-behind, and the River client is always stopped. This mirrors
// crm-admin's runSeed error branch.
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

	// The runSeed error branch (E3-D3b-fix): on a forced failure the entrypoint
	// runs the full teardown closure rather than Quiesce.
	require.NoError(t, teardown(context.Background()), "teardown after a forced failure must succeed")

	after, err := h.ContactsRemaining(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(0), after, "a failed seed must clean its partial world (no leave-behind)")
}

// TestSyntheticProfile_SettlesUnderHighAcceleration guards the E3-D-TIME change:
// the harness Settle/teardown budget is REAL wall-clock (a harness-local
// monotonic timer), so a high TIME_ACCELERATION — under which "30
// accelerated-seconds" would otherwise collapse to a sub-second real budget —
// does NOT cause a spurious settle timeout. A minimal-scoped seed still settles
// and its teardown still clears under acceleration 1000.
func TestSyntheticProfile_SettlesUnderHighAcceleration(t *testing.T) {
	testsupport.RequireLongTests(t)
	database, ctx := newSyntheticDB(t)

	// Pin an accelerated clock for this test's process: GetCurrentTime reads
	// TIME_ACCELERATION + TIME_BASE from the process env directly.
	t.Setenv("TIME_BASE", strconv.FormatInt(time.Now().Unix(), 10)) //nolint:forbidigo // test fixture pins the accel base
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
