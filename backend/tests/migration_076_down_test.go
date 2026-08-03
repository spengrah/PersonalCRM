//go:build integration_testdb

package tests

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/testdb"

	migrate "github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// whatsappFoundationsVersion is the golang-migrate version of 076. The
// round-trip positions each clone here first so Steps(-1) rolls down 076
// specifically, robust to later migrations landing above it.
const whatsappFoundationsVersion = 76

// migration076Env is one refusal case's isolated database, positioned at 076.
type migration076Env struct {
	ctx      context.Context
	database *db.Database
	migrator *migrate.Migrate
	comms    *repository.CommsMessageRepository
	wa       *repository.WhatsAppRepository
	contacts *repository.ContactRepository
	inter    *repository.InteractionRepository
}

// newMigration076Env clones the template, opens a pool on the clone and
// positions it at 076. Every case gets its own clone: a refused down leaves the
// migrator dirty, and the seeded row is exactly the state the guard exists to
// protect, so it must not reach the shared package DB.
func newMigration076Env(t *testing.T) *migration076Env {
	t.Helper()
	ctx := context.Background()
	cloneURL, drop := testdb.NewEphemeralClone(t)
	t.Cleanup(drop)

	cfg := config.TestConfig()
	cfg.Database.URL = cloneURL
	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	// LIFO: this Close runs BEFORE drop, so the DROP DATABASE has no
	// connections left to fight.
	t.Cleanup(database.Close)

	m, err := migrate.New(fmt.Sprintf("file://%s", getMigrationsPath()), cloneURL)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = m.Close() })

	if err := m.Migrate(whatsappFoundationsVersion); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		require.NoError(t, err, "position the clone at the whatsapp-foundations tip")
	}

	return &migration076Env{
		ctx:      ctx,
		database: database,
		migrator: m,
		comms:    repository.NewCommsMessageRepository(database.Queries),
		wa:       repository.NewWhatsAppRepository(database.Queries),
		contacts: repository.NewContactRepository(database.Queries),
		inter:    repository.NewInteractionRepository(database.Queries),
	}
}

// TestMigration076Down_RefusalGuards proves each of the four data-loss guards
// FIRES, and — the case that keeps the other four honest — that they are not
// unconditional: a clean database reverts fully.
//
// Migration-subject test: it rolls the schema down, so it stays serial and uses
// an isolated clone per case (never the shared package DB).
func TestMigration076Down_RefusalGuards(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	t.Run("RefusesWithWhatsAppInteraction", func(t *testing.T) {
		env := newMigration076Env(t)
		contact, err := env.contacts.CreateContact(env.ctx, repository.CreateContactRequest{
			FullName: "Migration 076 Guard",
		})
		require.NoError(t, err)
		ref := "whatsapp:chat:msg-1"
		_, err = env.inter.CreateInteraction(env.ctx, repository.CreateInteractionRequest{
			ContactID:  contact.ID,
			Source:     repository.InteractionSourceWhatsApp,
			SourceRef:  &ref,
			OccurredAt: accelerated.GetCurrentTime(),
			Direction:  repository.InteractionDirectionInbound,
		})
		require.NoError(t, err)

		err = env.migrator.Steps(-1)
		require.Error(t, err, "the revert must refuse while a whatsapp interaction exists")
		assert.Contains(t, err.Error(), "cannot revert interaction.source check")
	})

	t.Run("RefusesWithNullContactCommsRow", func(t *testing.T) {
		env := newMigration076Env(t)
		peer := "guard@s.whatsapp.net"
		body := "staged before the peer was known"
		_, err := env.comms.UpsertChatMessage(env.ctx, repository.UpsertChatMessageParams{
			Source:     repository.InteractionSourceWhatsApp,
			ExternalID: "guard-null-contact",
			ThreadID:   peer,
			Body:       &body,
			PeerHandle: &peer,
			Direction:  repository.InteractionDirectionInbound,
			SentAt:     accelerated.GetCurrentTime(),
		})
		require.NoError(t, err)

		err = env.migrator.Steps(-1)
		require.Error(t, err, "the revert must refuse while a contactless staging row exists")
		assert.Contains(t, err.Error(), "cannot restore comms_message.matched_contact_id NOT NULL")
	})

	// Every state the guard names, not just one: with only `pending` seeded,
	// deleting `processing` or `failed` from the down migration's IN list would
	// leave all six subtests green while a live lease or an operator-requeueable
	// chunk lost its only surviving media pointer.
	for _, tc := range []struct {
		state string
		seed  func(t *testing.T, env *migration076Env)
	}{
		{
			state: repository.HistoryNotificationStatePending,
			seed: func(t *testing.T, env *migration076Env) {
				_, err := env.wa.RecordNotification(env.ctx, "guard-pending", []byte("pointer"),
					"FULL", 0, nil, repository.HistoryDispositionProject)
				require.NoError(t, err)
			},
		},
		{
			state: repository.HistoryNotificationStateProcessing,
			seed: func(t *testing.T, env *migration076Env) {
				_, err := env.wa.RecordNotification(env.ctx, "guard-processing", []byte("pointer"),
					"FULL", 0, nil, repository.HistoryDispositionProject)
				require.NoError(t, err)
				claimed, err := env.wa.ClaimNextNotification(env.ctx)
				require.NoError(t, err)
				require.Equal(t, repository.HistoryNotificationStateProcessing, claimed.State)
			},
		},
		{
			state: repository.HistoryNotificationStateFailed,
			seed: func(t *testing.T, env *migration076Env) {
				id, err := env.wa.RecordNotification(env.ctx, "guard-failed", []byte("pointer"),
					"FULL", 0, nil, repository.HistoryDispositionProject)
				require.NoError(t, err)
				claimed, err := env.wa.ClaimNextNotification(env.ctx)
				require.NoError(t, err)
				require.NotNil(t, claimed.ClaimToken)
				ok, err := env.wa.MarkNotificationFailed(env.ctx, id, *claimed.ClaimToken, "media gone")
				require.NoError(t, err)
				require.True(t, ok)
			},
		},
	} {
		t.Run("RefusesWithHistoryNotification_"+tc.state, func(t *testing.T) {
			env := newMigration076Env(t)
			tc.seed(t, env)

			err := env.migrator.Steps(-1)
			require.Errorf(t, err, "a %s chunk still points at undeleted media on WhatsApp's servers", tc.state)
			assert.Contains(t, err.Error(), "cannot drop whatsapp_history_notification")
		})
	}

	// A chunk that genuinely finished holds nothing the revert could destroy,
	// so it must NOT refuse — otherwise the guard would be an unconditional
	// "any row blocks", which is a different (and wrong) rule.
	t.Run("AllowsCompletedHistoryNotification", func(t *testing.T) {
		env := newMigration076Env(t)
		id, err := env.wa.RecordNotification(env.ctx, "guard-done", []byte("pointer"),
			"FULL", 0, nil, repository.HistoryDispositionProject)
		require.NoError(t, err)
		claimed, err := env.wa.ClaimNextNotification(env.ctx)
		require.NoError(t, err)
		require.NotNil(t, claimed.ClaimToken)
		for _, edge := range [][2]string{
			{repository.HistoryPhaseRecorded, repository.HistoryPhaseDownloaded},
			{repository.HistoryPhaseDownloaded, repository.HistoryPhaseProjected},
			{repository.HistoryPhaseProjected, repository.HistoryPhaseAcked},
			{repository.HistoryPhaseAcked, repository.HistoryPhaseDeleted},
		} {
			ok, err := env.wa.AdvancePhase(env.ctx, id, *claimed.ClaimToken, edge[0], edge[1])
			require.NoError(t, err)
			require.True(t, ok)
		}
		ok, err := env.wa.MarkNotificationDone(env.ctx, id, *claimed.ClaimToken)
		require.NoError(t, err)
		require.True(t, ok)

		require.NoError(t, env.migrator.Steps(-1),
			"a done chunk holds no outstanding media, so it must not block the revert")
	})

	t.Run("RefusesWithChatConfigRows", func(t *testing.T) {
		env := newMigration076Env(t)
		_, err := env.wa.UpsertChatConfig(env.ctx, repository.WhatsAppChatConfig{
			ChatJID: "guard@g.us", ChatType: "group", Status: "ignored",
		})
		require.NoError(t, err)

		err = env.migrator.Steps(-1)
		require.Error(t, err, "per-chat overrides are user decisions and are not reconstructible")
		assert.Contains(t, err.Error(), "cannot drop whatsapp_chat_config")
	})

	t.Run("SucceedsOnCleanDatabase", func(t *testing.T) {
		env := newMigration076Env(t)
		support := repository.NewSyntheticSupportRepository(env.database.Queries)

		present := tablesPresent(env.ctx, t, support,
			[]string{"whatsapp_history_notification", "whatsapp_chat_config"})
		require.True(t, present["whatsapp_history_notification"])
		require.True(t, present["whatsapp_chat_config"])
		require.True(t, constraintExists(env.ctx, t, env.database, "comms_message_contact_source_check"))

		require.NoError(t, env.migrator.Steps(-1),
			"the guards must not be unconditional — a clean database reverts")

		present = tablesPresent(env.ctx, t, support,
			[]string{"whatsapp_history_notification", "whatsapp_chat_config"})
		assert.False(t, present["whatsapp_history_notification"], "the history inbox is dropped")
		assert.False(t, present["whatsapp_chat_config"], "the chat gate is dropped")
		assert.False(t, constraintExists(env.ctx, t, env.database, "comms_message_contact_source_check"),
			"the source-scoped CHECK is dropped with the relaxation it bounded")

		// The narrowed source CHECK is back: a whatsapp interaction no longer
		// inserts. This is what proves the CHECK was really restored rather
		// than merely dropped.
		contact, err := env.contacts.CreateContact(env.ctx, repository.CreateContactRequest{
			FullName: "Migration 076 Reverted",
		})
		require.NoError(t, err)
		ref := "whatsapp:chat:after-revert"
		_, err = env.inter.CreateInteraction(env.ctx, repository.CreateInteractionRequest{
			ContactID:  contact.ID,
			Source:     repository.InteractionSourceWhatsApp,
			SourceRef:  &ref,
			OccurredAt: accelerated.GetCurrentTime(),
			Direction:  repository.InteractionDirectionInbound,
		})
		require.Error(t, err, "after the revert the CHECK must reject whatsapp again")
	})
}
