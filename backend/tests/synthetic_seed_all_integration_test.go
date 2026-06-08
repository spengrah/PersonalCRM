package tests

import (
	"context"
	"testing"

	"personal-crm/backend/internal/synthetic"
	"personal-crm/backend/internal/synthetic/factory"
	"personal-crm/backend/tests/testsupport"

	"github.com/stretchr/testify/require"
)

// TestSyntheticSeedAll exercises the mode-(b) SeedAll: it builds, settles, and
// the harness cleanup empties its namespaced rows. Slow-gated (TestSynthetic
// name prefix routes it into the nightly slow suite via BACKEND_SLOW_TESTS_REGEX).
func TestSyntheticSeedAll(t *testing.T) {
	testsupport.RequireLongTests(t)
	database, ctx := newSyntheticDB(t)

	params := synthetic.DefaultParams()
	params.Namespace = syntheticNS(t) // unique per run for shared-DB isolation

	// Construct the harness for the SAME namespace so identifiers + cleanup align.
	h := synthetic.NewHarnessForNamespace(t, ctx, database, params.Namespace, params.Seed)

	res, err := synthetic.SeedAll(ctx, h, params)
	require.NoError(t, err)
	require.NotEmpty(t, res.GmailContactIDs)
	require.NotEmpty(t, res.TelegramContactIDs)

	// The seeded contacts exist before cleanup.
	remaining, err := h.ContactsRemaining(ctx)
	require.NoError(t, err)
	require.Greater(t, remaining, int64(0), "SeedAll should have created contacts")

	// Idempotency: re-replaying SeedAll's captured Gmail source payload (stable
	// source-ids) must NOT create a duplicate comms_message row. SeedAll's
	// contact creation is not upsert-idempotent, so the idempotency contract is
	// at the source-message level — this re-replays the exact same payload.
	require.NotNil(t, res.GmailIdempotencyProbe)
	probe := res.GmailIdempotencyProbe
	rowsBefore, err := h.CommsRepo().ListByContact(ctx, probe.ContactID)
	require.NoError(t, err)
	_, err = h.ReplayGmail(ctx, probe.ContactID, probe.Spec)
	require.NoError(t, err)
	rowsAfter, err := h.CommsRepo().ListByContact(ctx, probe.ContactID)
	require.NoError(t, err)
	require.Equal(t, len(rowsBefore), len(rowsAfter), "re-replaying the same Gmail payload must not add a duplicate row")
}

// TestSyntheticSeedAll_CleanupEmptiesNamespace proves the harness's cleanup
// closure removes the namespace's contacts while leaving a sentinel contact
// seeded by a DIFFERENT namespace intact (non-destructive scoping). It drives
// the teardown explicitly via NewHarnessWithDB so it can assert post-cleanup.
func TestSyntheticSeedAll_CleanupEmptiesNamespace(t *testing.T) {
	testsupport.RequireLongTests(t)
	database, ctx := newSyntheticDB(t)

	// Sentinel namespace: seed a contact + settled replay, and DO NOT tear it
	// down until the end, so we can prove the target's cleanup leaves it alone.
	sentinel := synthetic.NewHarnessForNamespace(t, ctx, database, syntheticNS(t), 12345)
	sgen := sentinel.Generator()
	sspec := sgen.Contact(factory.WithEmail())
	sentinelContact, err := sentinel.SeedContact(ctx, sspec)
	require.NoError(t, err)

	// Target namespace via the explicit-teardown constructor so we can run the
	// closure and then assert.
	target, teardown, err := synthetic.NewHarnessWithDB(ctx, database)
	require.NoError(t, err)
	tgen := target.Generator()
	tspec := tgen.Contact(factory.WithEmail())
	targetContact, err := target.SeedContact(ctx, tspec)
	require.NoError(t, err)
	_, err = target.ReplayGmail(ctx, targetContact.ID, tgen.GmailMessage(tspec, factory.MatchSeeded))
	require.NoError(t, err)

	// Run the target's teardown (quiesce + Gate-B-gated cleanup).
	require.NoError(t, teardown(context.Background()))

	// The target's contacts are gone.
	gone, err := target.ContactsRemaining(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(0), gone, "target namespace cleanup should remove its contacts")

	// The sentinel's contact survives (non-destructive scoping).
	surviving, err := sentinel.ContactsRemaining(ctx)
	require.NoError(t, err)
	require.Greater(t, surviving, int64(0), "a different namespace's contact must survive the target's cleanup")
	_ = sentinelContact
}
