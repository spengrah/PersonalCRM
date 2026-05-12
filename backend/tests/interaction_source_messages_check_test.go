package tests

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestInteraction_SourceCheckAcceptsMessages locks in the PR3 CHECK
// extension: an interaction with source="messages" inserts successfully.
// Per the spec scope this PR is the migration that lifts the constraint
// from {manual, gcal, todoist, telegram} to also include 'messages'.
func TestInteraction_SourceCheckAcceptsMessages(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL
	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	defer database.Close()

	interactionRepo := repository.NewInteractionRepository(database.Queries)
	contactRepo := repository.NewContactRepository(database.Queries)

	suffix := randomSuffix(t)
	contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
		FullName: "Test Source CHECK " + suffix,
	})
	require.NoError(t, err)
	defer func() { _ = contactRepo.SoftDeleteContact(ctx, contact.ID) }()

	ref := "messages-test-" + suffix
	interaction, err := interactionRepo.CreateInteraction(ctx, repository.CreateInteractionRequest{
		ContactID:  contact.ID,
		Source:     repository.InteractionSourceMessages,
		SourceRef:  &ref,
		OccurredAt: accelerated.GetCurrentTime().Truncate(time.Microsecond),
		Direction:  repository.InteractionDirectionInbound,
	})
	require.NoError(t, err)
	assert.Equal(t, repository.InteractionSourceMessages, interaction.Source)
}

// TestInteraction_SourceCheckRejectsWhatsapp confirms the scope
// boundary: 'whatsapp' is NOT yet in the CHECK list (deferred to a
// future spec PR per .ai/log/plan/mac-daemon-phase-1-pr1-pi-foundation.md).
// Acts as a regression guard against accidentally widening the CHECK
// beyond PR3's plan.
func TestInteraction_SourceCheckRejectsWhatsapp(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL
	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	defer database.Close()

	interactionRepo := repository.NewInteractionRepository(database.Queries)
	contactRepo := repository.NewContactRepository(database.Queries)

	suffix := randomSuffix(t)
	contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
		FullName: "Test Whatsapp Reject " + suffix,
	})
	require.NoError(t, err)
	defer func() { _ = contactRepo.SoftDeleteContact(ctx, contact.ID) }()

	ref := "wa-" + suffix
	_, err = interactionRepo.CreateInteraction(ctx, repository.CreateInteractionRequest{
		ContactID:  contact.ID,
		Source:     "whatsapp",
		SourceRef:  &ref,
		OccurredAt: accelerated.GetCurrentTime().Truncate(time.Microsecond),
		Direction:  repository.InteractionDirectionInbound,
	})
	require.Error(t, err, "whatsapp source must be rejected by interaction_source_check")
	assert.True(t, strings.Contains(err.Error(), "interaction_source_check") ||
		strings.Contains(err.Error(), "check constraint"),
		"error should mention check constraint, got: %v", err)
}
