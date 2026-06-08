package tests

import (
	"context"
	"fmt"
	"os"
	"testing"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/synthetic"
	"personal-crm/backend/internal/synthetic/factory"
	"personal-crm/backend/tests/testsupport"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// These replay integration tests are SLOW-gated (testsupport.RequireLongTests):
// skipped in CI's fast PR gate, run pre-push (LONG_TESTS=1) + locally via
// make test-integration, AND nightly via BACKEND_SLOW_TESTS_REGEX (the
// TestSynthetic name prefix). Each sub-test uses a UNIQUE namespace so
// shared-test-DB reuse cannot collide; teardown is the harness's auto-registered
// quiesce + conditional-cleanup closure (D8).

func newSyntheticDB(t *testing.T) (*db.Database, context.Context) {
	t.Helper()
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()
	require.NoError(t, db.RunMigrations(ctx, databaseURL, getMigrationsPath()))
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL
	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	t.Cleanup(database.Close)
	return database, ctx
}

// ns builds a unique per-sub-test namespace (sanitized token segment).
func syntheticNS(t *testing.T) string {
	return "r" + uuid.NewString()[:8]
}

func TestSyntheticReplay_SeededSenderSettled(t *testing.T) {
	testsupport.RequireLongTests(t)
	database, ctx := newSyntheticDB(t)

	t.Run("gmail", func(t *testing.T) {
		h := synthetic.NewHarnessForNamespace(t, ctx, database, syntheticNS(t), factory.DefaultSeed)
		gen := h.Generator()
		spec := gen.Contact(factory.WithEmail())
		contact, err := h.SeedContact(ctx, spec)
		require.NoError(t, err)
		res, err := h.ReplayGmail(ctx, contact.ID, gen.GmailMessage(spec, factory.MatchSeeded))
		require.NoError(t, err)
		require.True(t, res.Matched)
		requireInteractionSource(t, ctx, h, contact.ID, "email")
	})

	t.Run("telegram", func(t *testing.T) {
		h := synthetic.NewHarnessForNamespace(t, ctx, database, syntheticNS(t), factory.DefaultSeed)
		gen := h.Generator()
		spec := gen.Contact(factory.WithTelegram())
		contact, err := h.SeedContact(ctx, spec)
		require.NoError(t, err)
		res, err := h.ReplayTelegram(ctx, contact.ID, gen.TelegramMessage(spec, factory.MatchSeeded))
		require.NoError(t, err)
		require.True(t, res.Matched)
		requireInteractionSource(t, ctx, h, contact.ID, "telegram")
	})

	t.Run("gcal", func(t *testing.T) {
		h := synthetic.NewHarnessForNamespace(t, ctx, database, syntheticNS(t), factory.DefaultSeed)
		gen := h.Generator()
		spec := gen.Contact(factory.WithEmail())
		contact, err := h.SeedContact(ctx, spec)
		require.NoError(t, err)
		res, err := h.ReplayGCal(ctx, contact.ID, gen.GCalEvent(spec, factory.MatchSeeded))
		require.NoError(t, err)
		require.True(t, res.Matched)
		requireInteractionSource(t, ctx, h, contact.ID, "gcal")
	})

	t.Run("gchat", func(t *testing.T) {
		h := synthetic.NewHarnessForNamespace(t, ctx, database, syntheticNS(t), factory.DefaultSeed)
		gen := h.Generator()
		spec := gen.Contact(factory.WithEmail())
		contact, err := h.SeedContact(ctx, spec)
		require.NoError(t, err)
		res, err := h.ReplayGChat(ctx, contact.ID, gen.GChatMessage(spec, factory.MatchSeeded))
		require.NoError(t, err)
		require.True(t, res.Matched)
		requireInteractionSource(t, ctx, h, contact.ID, "gchat")
	})

	t.Run("imessage", func(t *testing.T) {
		h := synthetic.NewHarnessForNamespace(t, ctx, database, syntheticNS(t), factory.DefaultSeed)
		gen := h.Generator()
		spec := gen.Contact(factory.WithPhone())
		contact, err := h.SeedContact(ctx, spec)
		require.NoError(t, err)
		imsg, err := gen.IMessage(spec, factory.MatchSeeded, h.MacHostID())
		require.NoError(t, err)
		res, err := h.ReplayIMessage(ctx, contact.ID, imsg)
		require.NoError(t, err)
		require.True(t, res.Matched)
		requireInteractionSource(t, ctx, h, contact.ID, "messages")
	})

	t.Run("mac_contacts", func(t *testing.T) {
		h := synthetic.NewHarnessForNamespace(t, ctx, database, syntheticNS(t), factory.DefaultSeed)
		gen := h.Generator()
		spec := gen.Contact(factory.WithEmail())
		contact, err := h.SeedContact(ctx, spec)
		require.NoError(t, err)
		mc, err := gen.MacContact(spec, factory.MatchSeeded, h.MacHostID())
		require.NoError(t, err)
		res, err := h.ReplayMacContacts(ctx, contact.ID, mc)
		require.NoError(t, err)
		require.True(t, res.Matched)
	})

	t.Run("todoist", func(t *testing.T) {
		h := synthetic.NewHarnessForNamespace(t, ctx, database, syntheticNS(t), factory.DefaultSeed)
		gen := h.Generator()
		spec := gen.Contact(factory.WithEmail(), factory.WithCadence("weekly"))
		contact, err := h.SeedContact(ctx, spec)
		require.NoError(t, err)
		_, err = h.ReplayTodoist(ctx, []uuid.UUID{contact.ID})
		require.NoError(t, err)
	})
}

func TestSyntheticReplay_UnknownSenderPending(t *testing.T) {
	testsupport.RequireLongTests(t)
	database, ctx := newSyntheticDB(t)

	// Only the four pending-capable sources (D4 matrix).
	t.Run("mac_contacts_unmatched", func(t *testing.T) {
		h := synthetic.NewHarnessForNamespace(t, ctx, database, syntheticNS(t), factory.DefaultSeed)
		gen := h.Generator()
		mc, err := gen.MacContact(gen.Contact(factory.WithEmail()), factory.MatchUnknown, h.MacHostID())
		require.NoError(t, err)
		res, err := h.ReplayMacContacts(ctx, uuid.Nil, mc)
		require.NoError(t, err)
		require.False(t, res.Matched)
	})

	t.Run("imessage_stranded", func(t *testing.T) {
		h := synthetic.NewHarnessForNamespace(t, ctx, database, syntheticNS(t), factory.DefaultSeed)
		gen := h.Generator()
		imsg, err := gen.IMessage(gen.Contact(factory.WithPhone()), factory.MatchUnknown, h.MacHostID())
		require.NoError(t, err)
		res, err := h.ReplayIMessage(ctx, uuid.Nil, imsg)
		require.NoError(t, err)
		require.False(t, res.Matched)
	})

	t.Run("telegram_stranded", func(t *testing.T) {
		h := synthetic.NewHarnessForNamespace(t, ctx, database, syntheticNS(t), factory.DefaultSeed)
		gen := h.Generator()
		res, err := h.ReplayTelegram(ctx, uuid.Nil, gen.TelegramMessage(gen.Contact(factory.WithTelegram()), factory.MatchUnknown))
		require.NoError(t, err)
		require.False(t, res.Matched)
	})

	t.Run("gcal_unmatched_attendee", func(t *testing.T) {
		h := synthetic.NewHarnessForNamespace(t, ctx, database, syntheticNS(t), factory.DefaultSeed)
		gen := h.Generator()
		res, err := h.ReplayGCal(ctx, uuid.Nil, gen.GCalEvent(gen.Contact(factory.WithEmail()), factory.MatchUnknown))
		require.NoError(t, err)
		require.False(t, res.Matched)
	})
}

func TestSyntheticReplay_UnknownSenderMatchOnly(t *testing.T) {
	testsupport.RequireLongTests(t)
	database, ctx := newSyntheticDB(t)

	// Gmail/GChat unknown sender: match-only (no comms_message row written for
	// the unknown correspondent, hence no interaction, no contact create).
	t.Run("gmail_match_only", func(t *testing.T) {
		h := synthetic.NewHarnessForNamespace(t, ctx, database, syntheticNS(t), factory.DefaultSeed)
		gen := h.Generator()
		spec := gen.GmailMessage(gen.Contact(factory.WithEmail()), factory.MatchUnknown)
		res, err := h.ReplayGmail(ctx, uuid.Nil, spec)
		require.NoError(t, err)
		require.False(t, res.Matched)
		exists, err := h.CommsRowExists(ctx, "email", spec.ExternalID)
		require.NoError(t, err)
		require.False(t, exists, "unknown Gmail correspondent must not produce a comms_message row")
	})

	t.Run("gchat_match_only", func(t *testing.T) {
		h := synthetic.NewHarnessForNamespace(t, ctx, database, syntheticNS(t), factory.DefaultSeed)
		gen := h.Generator()
		spec := gen.GChatMessage(gen.Contact(factory.WithEmail()), factory.MatchUnknown)
		res, err := h.ReplayGChat(ctx, uuid.Nil, spec)
		require.NoError(t, err)
		require.False(t, res.Matched)
		exists, err := h.CommsRowExists(ctx, "gchat", spec.ExternalID)
		require.NoError(t, err)
		require.False(t, exists, "unknown GChat sender must not produce a comms_message row")
	})
}

func TestSyntheticReplay_IdempotentReReplay(t *testing.T) {
	testsupport.RequireLongTests(t)
	database, ctx := newSyntheticDB(t)

	h := synthetic.NewHarnessForNamespace(t, ctx, database, syntheticNS(t), factory.DefaultSeed)
	gen := h.Generator()
	spec := gen.Contact(factory.WithEmail())
	contact, err := h.SeedContact(ctx, spec)
	require.NoError(t, err)

	// Same source payload replayed twice ⇒ stable source-ids dedup to one row.
	msg := gen.GmailMessage(spec, factory.MatchSeeded)
	_, err = h.ReplayGmail(ctx, contact.ID, msg)
	require.NoError(t, err)
	_, err = h.ReplayGmail(ctx, contact.ID, msg)
	require.NoError(t, err)

	rows, err := h.CommsRepo().ListByContact(ctx, contact.ID)
	require.NoError(t, err)
	require.Len(t, rows, 1, "re-replay of the same payload must not add a duplicate comms_message row")
}

// requireInteractionSource asserts the contact has at least one interaction with
// the given source after settle.
func requireInteractionSource(t *testing.T, ctx context.Context, h *synthetic.Harness, contactID uuid.UUID, source string) {
	t.Helper()
	rows, err := h.InteractionRepo().ListContactInteractions(ctx, contactID, 100, 0)
	require.NoError(t, err)
	found := false
	for _, r := range rows {
		if r.Source == source {
			found = true
			break
		}
	}
	require.True(t, found, fmt.Sprintf("expected an interaction with source=%s for contact %s", source, contactID))
}
