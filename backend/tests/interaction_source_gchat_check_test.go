package tests

import (
	"context"
	"os"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestInteraction_SourceCheckAcceptsGchat locks in the migration-061 CHECK
// extension: an interaction with source="gchat" inserts successfully. The
// migration lifts interaction_source_check from the 8-value email set to also
// include 'gchat'. The upper-boundary regression (a non-listed source is
// rejected) is already guarded by TestInteraction_SourceCheckRejectsUnknownSource.
func TestInteraction_SourceCheckAcceptsGchat(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}
	t.Parallel()
	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL
	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	defer database.Close()

	interactionRepo := repository.NewInteractionRepository(database.Queries)
	contactRepo := repository.NewContactRepository(database.Queries)

	suffix := syntheticNS(t)
	contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
		FullName: "Test GChat Source CHECK " + suffix,
	})
	require.NoError(t, err)
	// Hard-delete any gchat-source rows before closing the DB handle. The
	// down migration in 061 refuses to drop interaction_source_check while
	// any row uses source='gchat' (data-loss guard), and the guard counts
	// rows regardless of deleted_at — so a soft-delete is insufficient.
	defer func() {
		_ = interactionRepo.HardDeleteInteractionsBySourceRefPrefix(ctx, repository.InteractionSourceGChat, "gchat-test-"+suffix+"%")
		_ = contactRepo.SoftDeleteContact(ctx, contact.ID)
	}()

	ref := "gchat-test-" + suffix
	interaction, err := interactionRepo.CreateInteraction(ctx, repository.CreateInteractionRequest{
		ContactID:  contact.ID,
		Source:     repository.InteractionSourceGChat,
		SourceRef:  &ref,
		OccurredAt: accelerated.GetCurrentTime().Truncate(time.Microsecond),
		Direction:  repository.InteractionDirectionInbound,
	})
	require.NoError(t, err)
	assert.Equal(t, repository.InteractionSourceGChat, interaction.Source)
}
