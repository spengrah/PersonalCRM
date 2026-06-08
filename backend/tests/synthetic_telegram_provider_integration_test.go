package tests

import (
	"strconv"
	"testing"

	"personal-crm/backend/internal/synthetic"
	"personal-crm/backend/internal/synthetic/factory"
	"personal-crm/backend/tests/testsupport"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// Telegram PROVIDER-level integration tests. The private full-pipeline path is
// already covered by E1's ReplayTelegram smoke, so the private cases here are a
// thin incremental layer (coalescing, stranded→discovery, idempotency) the
// engine-direct telegram_*.go suite + the single-message E1 smoke do not make.
// The GROUP cases are the structurally-new flow: the real HandleNewMessage group
// path through the harness's bus + matching + settle. Slow-gated (TestSynthetic
// prefix + RequireLongTests); unique namespace per sub-test.

// --- Private path (thin incremental layer over the E1 smoke) ----------------

func TestSyntheticTelegramProvider_PrivateCoalescesThroughPipeline(t *testing.T) {
	testsupport.RequireLongTests(t)
	database, ctx := newSyntheticDB(t)

	h := synthetic.NewHarnessForNamespace(t, ctx, database, syntheticNS(t), factory.DefaultSeed)
	gen := h.Generator()
	spec := gen.Contact(factory.WithTelegram())
	contact, err := h.SeedContact(ctx, spec)
	require.NoError(t, err)

	// Two inbound messages from the SAME peer (same peer id + handle + chat),
	// distinct message ids — coalesce into one interaction through the full
	// pipeline + River settle (the aggregation tests assert this engine-direct
	// with a nil bus; this asserts it through the real bus).
	msg1 := gen.TelegramMessage(spec, factory.MatchSeeded)
	msg2 := msg1
	msg2.TelegramMessageID = msg1.TelegramMessageID + 1

	res1, err := h.ReplayTelegram(ctx, contact.ID, msg1)
	require.NoError(t, err)
	require.True(t, res1.Matched)
	_, err = h.ReplayTelegram(ctx, contact.ID, msg2)
	require.NoError(t, err)

	// Coalesced: exactly one telegram interaction for the contact.
	require.Equal(t, 1, countInteractionsBySource(t, ctx, h, contact.ID, "telegram"),
		"two messages from the same peer must coalesce into one telegram interaction")
}

func TestSyntheticTelegramProvider_StrandedProducesDiscoveryCandidate(t *testing.T) {
	testsupport.RequireLongTests(t)
	database, ctx := newSyntheticDB(t)

	h := synthetic.NewHarnessForNamespace(t, ctx, database, syntheticNS(t), factory.DefaultSeed)
	gen := h.Generator()

	// Unknown peer; send enough messages to cross the discovery threshold (the
	// harness's matcher uses min=3), so UpdateDiscoveryCandidatesForPeer upserts
	// an external_contact discovery candidate.
	base := gen.TelegramMessage(gen.Contact(factory.WithTelegram()), factory.MatchUnknown)
	for i := 0; i < 3; i++ {
		spec := base
		spec.TelegramMessageID = base.TelegramMessageID + int32(i)
		res, err := h.ReplayTelegram(ctx, uuid.Nil, spec)
		require.NoError(t, err)
		require.False(t, res.Matched)
	}

	// The discovery candidate exists, keyed by source='telegram', source_id =
	// the bare peer id.
	cand, err := h.ExternalContactRepo().GetBySource(ctx, "telegram", strconv.FormatInt(base.PeerUserID, 10), nil)
	require.NoError(t, err)
	require.NotNil(t, cand, "an unknown peer over the discovery threshold must produce a discovery candidate")
	require.Equal(t, "unmatched", string(cand.MatchStatus))
}

func TestSyntheticTelegramProvider_IdempotentReReplay(t *testing.T) {
	testsupport.RequireLongTests(t)
	database, ctx := newSyntheticDB(t)

	h := synthetic.NewHarnessForNamespace(t, ctx, database, syntheticNS(t), factory.DefaultSeed)
	gen := h.Generator()
	spec := gen.Contact(factory.WithTelegram())
	contact, err := h.SeedContact(ctx, spec)
	require.NoError(t, err)

	// Re-replay the SAME private message (stable peer id + message id) — no
	// duplicate telegram_message row, no duplicate interaction.
	msg := gen.TelegramMessage(spec, factory.MatchSeeded)
	_, err = h.ReplayTelegram(ctx, contact.ID, msg)
	require.NoError(t, err)
	_, err = h.ReplayTelegram(ctx, contact.ID, msg)
	require.NoError(t, err)

	stored, err := h.GroupMessageStored(ctx, msg.TelegramChatID, msg.TelegramMessageID)
	require.NoError(t, err)
	require.True(t, stored, "the message row must exist")
	require.Equal(t, 1, countInteractionsBySource(t, ctx, h, contact.ID, "telegram"),
		"re-replay of the same message must not add a duplicate interaction")
}

// --- Group path (structurally-new full-pipeline flow) -----------------------

func TestSyntheticTelegramGroup_TrackedMatchedInteraction(t *testing.T) {
	testsupport.RequireLongTests(t)
	database, ctx := newSyntheticDB(t)

	h := synthetic.NewHarnessForNamespace(t, ctx, database, syntheticNS(t), factory.DefaultSeed)
	gen := h.Generator()
	spec := gen.Contact(factory.WithTelegram())
	contact, err := h.SeedContact(ctx, spec)
	require.NoError(t, err)

	// A tracked group (at-or-under the harness size threshold); the sender is the
	// seeded contact's telegram handle.
	members := h.GroupMaxMembers()
	g := gen.TelegramGroupMessage(spec, factory.MatchSeeded, members)
	res, err := h.ReplayTelegramGroup(ctx, contact.ID, g)
	require.NoError(t, err)
	require.True(t, res.Matched)
	require.True(t, res.Tracked)

	// The group message row is stored + linked to a telegram interaction, and the
	// chat config row exists with the observed member count.
	stored, err := h.GroupMessageStored(ctx, g.ChatID, g.TelegramMessageID)
	require.NoError(t, err)
	require.True(t, stored)
	require.Equal(t, 1, countInteractionsBySource(t, ctx, h, contact.ID, "telegram"))
	mc, err := h.GroupConfigMemberCount(ctx, g.ChatID)
	require.NoError(t, err)
	require.NotNil(t, mc)
	require.Equal(t, int32(members), *mc)
}

func TestSyntheticTelegramGroup_UntrackedOverSizeNotStored(t *testing.T) {
	testsupport.RequireLongTests(t)
	database, ctx := newSyntheticDB(t)

	h := synthetic.NewHarnessForNamespace(t, ctx, database, syntheticNS(t), factory.DefaultSeed)
	gen := h.Generator()
	spec := gen.Contact(factory.WithTelegram())
	contact, err := h.SeedContact(ctx, spec)
	require.NoError(t, err)

	// Over the harness size threshold → shouldTrackChat returns false → the
	// message is NOT stored, but the config row was upserted (the gate ran).
	overSize := h.GroupMaxMembers()*2 + 1
	g := gen.TelegramGroupMessage(spec, factory.MatchSeeded, overSize)
	res, err := h.ReplayTelegramGroup(ctx, contact.ID, g)
	require.NoError(t, err)
	require.False(t, res.Tracked)

	stored, err := h.GroupMessageStored(ctx, g.ChatID, g.TelegramMessageID)
	require.NoError(t, err)
	require.False(t, stored, "an over-size group must not store the message")
	mc, err := h.GroupConfigMemberCount(ctx, g.ChatID)
	require.NoError(t, err)
	require.NotNil(t, mc)
	require.Equal(t, int32(overSize), *mc, "the size gate must upsert the config with the observed member count")
	require.Equal(t, 0, countInteractionsBySource(t, ctx, h, contact.ID, "telegram"))
}

func TestSyntheticTelegramGroup_ParticipantCountRefresh(t *testing.T) {
	testsupport.RequireLongTests(t)
	database, ctx := newSyntheticDB(t)

	h := synthetic.NewHarnessForNamespace(t, ctx, database, syntheticNS(t), factory.DefaultSeed)
	gen := h.Generator()
	spec := gen.Contact(factory.WithTelegram())
	contact, err := h.SeedContact(ctx, spec)
	require.NoError(t, err)

	// First land a tracked group message so the config row exists.
	members := h.GroupMaxMembers()
	g := gen.TelegramGroupMessage(spec, factory.MatchSeeded, members)
	_, err = h.ReplayTelegramGroup(ctx, contact.ID, g)
	require.NoError(t, err)

	// Drive HandleChatParticipant via the stub-invoker client (the one api-using
	// path) → the config's member_count is refreshed to a DIFFERENT participant
	// list size, proving the refresh ran (not the original ingest count).
	refreshed := members + 2
	require.NoError(t, h.RefreshGroupMemberCount(ctx, g.ChatID, refreshed))

	mc, err := h.GroupConfigMemberCount(ctx, g.ChatID)
	require.NoError(t, err)
	require.NotNil(t, mc)
	require.Equal(t, int32(refreshed), *mc, "HandleChatParticipant must refresh member_count from the stub full-chat participant list")
}

func TestSyntheticTelegramGroup_UnknownSenderStrandedDiscovery(t *testing.T) {
	testsupport.RequireLongTests(t)
	database, ctx := newSyntheticDB(t)

	h := synthetic.NewHarnessForNamespace(t, ctx, database, syntheticNS(t), factory.DefaultSeed)
	gen := h.Generator()

	// Tracked group, unknown sender, enough messages to cross the discovery
	// threshold (min=3). All messages share ONE chat id + sender (a group
	// conversation), built via TelegramGroupMessageInChat.
	first := gen.TelegramGroupMessage(gen.Contact(factory.WithTelegram()), factory.MatchUnknown, h.GroupMaxMembers())
	specs := []factory.TelegramGroupMessageSpec{first}
	for i := 1; i < 3; i++ {
		next := first
		next.TelegramMessageID = first.TelegramMessageID + int32(i)
		specs = append(specs, next)
	}

	res, err := h.ReplayTelegramGroupMessages(ctx, uuid.Nil, specs)
	require.NoError(t, err)
	require.False(t, res.Matched)
	require.True(t, res.Tracked)

	// Stranded sender + discovery candidate keyed by the bare sender peer id.
	cand, err := h.ExternalContactRepo().GetBySource(ctx, "telegram", strconv.FormatInt(first.SenderUserID, 10), nil)
	require.NoError(t, err)
	require.NotNil(t, cand, "an unknown group sender over the threshold must produce a discovery candidate")
	require.Equal(t, "unmatched", string(cand.MatchStatus))
}
