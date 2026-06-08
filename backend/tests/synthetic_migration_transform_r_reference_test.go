package tests

import (
	"testing"

	"personal-crm/backend/internal/synthetic"
	"personal-crm/backend/internal/synthetic/factory"
	"personal-crm/backend/tests/testsupport"

	"github.com/stretchr/testify/require"
)

// This file is the canonical TRANSFORM-R reference for the suite migration onto
// the synthetic toolkit: the FULL replay-harness pattern, as opposed to the
// lightweight Transform-F/lite path the messages_message exemplar models. A
// Transform-R test seeds a contact via the factory, drives a real source through
// a Replay* adapter (which settles the graph + auto-cleans on teardown), and
// asserts the settled domain graph at/above the harness surface.
//
// It is River-draining and therefore SLOW-gated: RequireLongTests skips it in the
// fast PR gate, and the TestSynthetic name prefix routes it onto the nightly slow
// suite (BACKEND_SLOW_TESTS_REGEX). Do NOT route a fast repo test through this
// pattern — that would drop it from the PR gate (a coverage regression).

// TestSyntheticReplay_GCalDerivesLastContacted drives a past calendar event the
// account attended together with a seeded contact through the REAL
// CalendarSyncProvider via ReplayGCal, and asserts the settled graph derives the
// contact's last_contacted from the attended (mutual) interaction. The seeded
// contact starts with no last_contacted; after settle it is non-nil and lands at
// the event's occurred_at (~1h before the anchor).
func TestSyntheticReplay_GCalDerivesLastContacted(t *testing.T) {
	testsupport.RequireLongTests(t)
	database, ctx := newSyntheticDB(t)

	h := synthetic.NewHarnessForNamespace(t, ctx, database, syntheticNS(t), factory.DefaultSeed)
	gen := h.Generator()

	spec := gen.Contact(factory.WithEmail())
	contact, err := h.SeedContact(ctx, spec)
	require.NoError(t, err)
	require.Nil(t, contact.LastContacted, "seeded contact has no last_contacted before the replay")

	res, err := h.ReplayGCal(ctx, contact.ID, gen.GCalEvent(spec, factory.MatchSeeded))
	require.NoError(t, err)
	require.True(t, res.Matched)

	// The attended event is recorded as a gcal interaction for the contact.
	requireInteractionSource(t, ctx, h, contact.ID, "gcal")

	// And the mutual attended interaction derives the contact's last_contacted.
	updated, err := h.ContactRepo().GetContact(ctx, contact.ID)
	require.NoError(t, err)
	require.NotNil(t, updated.LastContacted, "the attended interaction must derive last_contacted")
}
