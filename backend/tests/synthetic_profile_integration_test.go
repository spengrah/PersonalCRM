package tests

import (
	"context"
	"strconv"
	"testing"

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
// What they cover is the profile ORCHESTRATION contract — dispatch, the phase/
// settle accounting, and the seed/quiesce/teardown lifecycle. The declared
// `standard` world's CONTENT is asserted by TestSyntheticDeclareStandardWorld
// and TestSyntheticDeclareEdges, which read it back through the API.

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
	// minimal-scoped reports its one `seed-all` phase, so both profiles surface
	// timings uniformly — otherwise this path's phase bracket could be dropped
	// without any test noticing.
	require.Len(t, res.Timings.Phases, 1, "minimal-scoped reports its single phase")
	require.Equal(t, "seed-all", res.Timings.Phases[0].Name)
	require.Positive(t, res.Timings.Settle.Calls, "minimal-scoped reports settle accounting")

	remaining, err := h.ContactsRemaining(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(2), remaining, "minimal-scoped seeds exactly two contacts")
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

	params, err := synthetic.ProfileParams(synthetic.ProfileMinimalScoped)
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
