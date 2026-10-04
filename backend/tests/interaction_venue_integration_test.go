//go:build integration_testdb

package tests

import (
	"context"
	"os"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/synthetic"
	"personal-crm/backend/internal/synthetic/factory"
	"personal-crm/backend/tests/testsupport"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestInteractionVenue_LivePath drives the REAL recorder pipeline (telegram)
// and asserts the live ResolveVenueForInteraction path sets venue_id, that two
// messages in the SAME chat share ONE venue node, and that adding venue_id does
// not perturb the contact's cadence columns (the headline-risk regression).
func TestInteractionVenue_LivePath(t *testing.T) {
	testsupport.RequireLongTests(t)
	database, ctx := newSyntheticDB(t)
	t.Parallel()

	h := synthetic.NewHarnessForNamespace(t, ctx, database, syntheticNS(t), factory.DefaultSeed)
	gen := h.Generator()
	spec := gen.Contact(factory.WithTelegram())
	contact, err := h.SeedContact(ctx, spec)
	require.NoError(t, err)

	// First telegram message in the chat → matched interaction with a venue.
	tgMsg1 := gen.TelegramMessage(spec, factory.MatchSeeded)
	res1, err := h.ReplayTelegram(ctx, contact.ID, tgMsg1)
	require.NoError(t, err)
	require.True(t, res1.Matched)

	// Snapshot cadence columns AFTER the first interaction settled but BEFORE the
	// second — the venue is already populated, so a second same-chat interaction
	// must reuse the node and must not perturb cadence beyond the normal
	// interaction effect.
	afterFirst, err := h.ContactRepo().GetContact(ctx, contact.ID)
	require.NoError(t, err)

	// Second message in the SAME chat (reuse the peer/chat id, bump the message
	// id) so the two interactions share one venue container.
	tgMsg2 := tgMsg1
	tgMsg2.TelegramMessageID = tgMsg1.TelegramMessageID + 1
	res2, err := h.ReplayTelegram(ctx, contact.ID, tgMsg2)
	require.NoError(t, err)
	require.True(t, res2.Matched)

	// Both telegram interactions resolve to the SAME venue node (one container).
	venueIDs := distinctVenueNodeIDs(t, ctx, h, contact.ID, repository.InteractionSourceTelegram)
	require.Len(t, venueIDs, 1, "two messages in one chat must share exactly one venue node")

	// The venue node is a live telegram dm/group_chat venue.
	venue, err := h.VenueRepo().GetVenue(ctx, venueIDs[0])
	require.NoError(t, err)
	require.Equal(t, repository.InteractionSourceTelegram, venue.Source)
	assert.Contains(t, []string{repository.VenueKindDM, repository.VenueKindGroupChat}, venue.Kind)

	// Cadence regression: the venue resolution must not regress the cadence math.
	// last_contacted advances only by the (normal) second-interaction effect, and
	// the venue write touches no cadence column — assert the cadence fields move
	// only as a normal second interaction would (never NULL-ed, never reset).
	afterSecond, err := h.ContactRepo().GetContact(ctx, contact.ID)
	require.NoError(t, err)
	require.NotNil(t, afterSecond.LastInteractionAt, "venue write must not null last_interaction_at")
	if afterFirst.LastInteractionAt != nil {
		require.False(t, afterSecond.LastInteractionAt.Before(*afterFirst.LastInteractionAt),
			"last_interaction_at must not move backward across the venue-bearing second interaction")
	}
}

// --- helpers ---

// distinctVenueNodeIDs returns the distinct venue node ids across a contact's
// interactions of the given source.
func distinctVenueNodeIDs(t *testing.T, ctx context.Context, h *synthetic.Harness, contactID uuid.UUID, source string) []uuid.UUID {
	t.Helper()
	rows, err := h.InteractionRepo().ListContactInteractions(ctx, contactID, 100, 0)
	require.NoError(t, err)
	seen := map[uuid.UUID]struct{}{}
	var out []uuid.UUID
	for _, r := range rows {
		if r.Source != source || r.VenueID == nil {
			continue
		}
		if _, ok := seen[*r.VenueID]; ok {
			continue
		}
		seen[*r.VenueID] = struct{}{}
		out = append(out, *r.VenueID)
	}
	return out
}

// whatsappVenueKindFor stages one comms_message(source='whatsapp') row with the
// given chat JID as its thread id, resolves it through the SAME registry the
// recorder uses, and returns the venue's kind. It proves the reader where every
// other VenueContainerReader is proved — against a staged row, through
// ResolveMessageVenueTx — because no test anywhere exercises
// ContainerForMessageTx directly.
func whatsappVenueKindFor(t *testing.T, ctx context.Context, database *db.Database, chatJID, externalID string) string {
	t.Helper()

	commsRepo := repository.NewCommsMessageRepository(database.Queries)
	body := "whatsapp venue body"
	peer := chatJID
	row, err := commsRepo.UpsertChatMessage(ctx, repository.UpsertChatMessageParams{
		Source:     repository.InteractionSourceWhatsApp,
		ExternalID: externalID,
		ThreadID:   chatJID,
		Body:       &body,
		PeerHandle: &peer,
		Direction:  repository.InteractionDirectionInbound,
		SentAt:     accelerated.GetCurrentTime().Add(-time.Hour).Truncate(time.Microsecond),
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		// LIKE pattern: the trailing % is what makes this a prefix delete.
		_ = commsRepo.HardDeleteBySourceAndExternalIDPrefix(context.Background(), repository.InteractionSourceWhatsApp, externalID+"%")
	})

	venueRepo := repository.NewVenueRepository(database.Queries)
	resolver := repository.NewVenueResolverRegistry(
		venueRepo,
		map[string]repository.VenueContainerReader{
			repository.InteractionSourceWhatsApp: repository.NewWhatsAppVenueContainerReader(),
		},
		nil,
	)

	tx, err := database.Pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()

	venueID, err := resolver.ResolveMessageVenueTx(ctx, tx, repository.InteractionSourceWhatsApp, []uuid.UUID{row.ID})
	require.NoError(t, err)
	require.NotNil(t, venueID, "the whatsapp reader must resolve a venue for a staged row")

	venue, err := repository.NewVenueRepository(db.New(tx)).GetVenue(ctx, *venueID)
	require.NoError(t, err)
	require.Equal(t, chatJID, venue.SourceContainerID)
	return venue.Kind
}

// TestWhatsAppVenue_DirectJIDIsDM: a one-to-one chat JID yields a dm venue.
//
// spec: WHA-043.direct-chat-is-a-dm
func TestWhatsAppVenue_DirectJIDIsDM(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}
	database, ctx := newSyntheticDB(t)
	t.Parallel()

	suffix := uuid.NewString()[:8]
	kind := whatsappVenueKindFor(t, ctx, database, "1204555"+suffix+"@s.whatsapp.net", "wa-venue-dm-"+suffix)
	assert.Equal(t, repository.VenueKindDM, kind)
}

// TestWhatsAppVenue_GroupJIDIsGroupChat: a group chat JID yields a group_chat
// venue. This is the case GChat's reader gets wrong as a template — it
// hard-codes group_chat, which is right for spaces and wrong for WhatsApp.
//
// spec: WHA-043.group-chat-is-a-group-chat
func TestWhatsAppVenue_GroupJIDIsGroupChat(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}
	database, ctx := newSyntheticDB(t)
	t.Parallel()

	suffix := uuid.NewString()[:8]
	kind := whatsappVenueKindFor(t, ctx, database, "1204555"+suffix+"-1690000000@g.us", "wa-venue-group-"+suffix)
	assert.Equal(t, repository.VenueKindGroupChat, kind)
}
