//go:build integration_testdb

package tests

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"personal-crm/backend/internal/cadence"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/testdb"

	migrate "github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

const connectedOutboundRepairPreVersion = 83

// spec: ING-039.connected-outbound
func TestConnectedOutboundCallRepair_Upgrade084(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	ctx := context.Background()
	cloneURL, drop := testdb.NewEphemeralClone(t)
	t.Cleanup(drop)
	cloneURLWithChicago, err := url.Parse(cloneURL)
	require.NoError(t, err)
	query := cloneURLWithChicago.Query()
	query.Set("options", "-c TimeZone=America/Chicago")
	cloneURLWithChicago.RawQuery = query.Encode()
	connectionURL := cloneURLWithChicago.String()

	cfg := config.TestConfig()
	cfg.Database.URL = connectionURL
	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	t.Cleanup(database.Close)

	m, err := migrate.New(fmt.Sprintf("file://%s", getMigrationsPath()), connectionURL)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = m.Close() })
	require.NoError(t, m.Migrate(connectedOutboundRepairPreVersion))

	contactRepo := repository.NewContactRepository(database.Queries)
	contactRepo.SetPool(database.Pool)
	interactionRepo := repository.NewInteractionRepository(database.Queries)
	phoneCallRepo := repository.NewPhoneCallRepository(database.Queries)

	// Keep the call near UTC midnight so a session-local date conversion would
	// disagree with the UTC contract asserted below.
	now := time.Date(2030, 2, 10, 0, 30, 0, 0, time.UTC)
	monthly := "monthly"
	oldLastContacted := now.Add(-20 * 24 * time.Hour)
	connectedAt := now.Add(-10 * 24 * time.Hour)
	oldAutomaticContactBy := oldLastContacted.AddDate(0, 0, 30)
	overriddenContactBy := oldLastContacted.AddDate(0, 0, 3)

	newFixtureContact := func(t *testing.T, suffix string, contactBy time.Time) *repository.Contact {
		t.Helper()
		createdAt := now.Add(-90 * 24 * time.Hour)
		contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
			FullName:  "Connected call repair fixture " + suffix,
			Cadence:   &monthly,
			CreatedAt: &createdAt,
		})
		require.NoError(t, err)
		require.NoError(t, contactRepo.TestSeedContactCadenceFields(ctx, contact.ID, repository.TestCadenceSeed{
			LastContacted: &oldLastContacted,
			ContactBy:     &contactBy,
		}))
		return contact
	}

	automatic := newFixtureContact(t, "automatic", oldAutomaticContactBy)
	awaitingUntil := now.AddDate(0, 0, 7)
	require.NoError(t, contactRepo.TestSeedContactCadenceFields(ctx, automatic.ID, repository.TestCadenceSeed{
		LastContacted:      &oldLastContacted,
		LastOutreachAt:     &connectedAt,
		ContactBy:          &oldAutomaticContactBy,
		AwaitingReplyUntil: &awaitingUntil,
	}))
	overridden := newFixtureContact(t, "override", overriddenContactBy)
	missed := newFixtureContact(t, "missed", oldAutomaticContactBy)
	newer := newFixtureContact(t, "newer", oldAutomaticContactBy)
	newerLastContacted := now.Add(-5 * 24 * time.Hour)
	newerLastInteraction := now.Add(-4 * 24 * time.Hour)
	newerLastOutreach := now.Add(-3 * 24 * time.Hour)
	newerLastResponse := now.Add(-2 * 24 * time.Hour)
	newerContactBy := newerLastContacted.AddDate(0, 0, 30)
	require.NoError(t, contactRepo.TestSeedContactCadenceFields(ctx, newer.ID, repository.TestCadenceSeed{
		LastContacted:     &newerLastContacted,
		LastInteractionAt: &newerLastInteraction,
		LastOutreachAt:    &newerLastOutreach,
		LastResponseAt:    &newerLastResponse,
		ContactBy:         &newerContactBy,
	}))
	deleted := newFixtureContact(t, "deleted", oldAutomaticContactBy)
	require.NoError(t, contactRepo.SoftDeleteContact(ctx, deleted.ID))
	deletedInteraction := newFixtureContact(t, "deleted-interaction", oldAutomaticContactBy)
	skipped := newFixtureContact(t, "skipped", oldAutomaticContactBy)
	skippedAt := now.Add(-24 * time.Hour)
	skippedTo := now.AddDate(0, 0, 90)
	require.NoError(t, contactRepo.TestSeedContactSkipState(ctx, skipped.ID, skippedTo, skippedAt, "fixture"))

	insertCall := func(t *testing.T, contact *repository.Contact, suffix string, duration int32) {
		t.Helper()
		interactionID := uuid.New()
		ref := "repair-fixture-" + suffix
		_, err := interactionRepo.TestInsertInteraction(ctx, interactionID, contact.ID,
			repository.InteractionSourcePhoneCalls, &ref, connectedAt,
			repository.InteractionDirectionOutbound)
		require.NoError(t, err)
		_, err = phoneCallRepo.TestInsertPhoneCallLinked(ctx, repository.TestInsertPhoneCallLinkedParams{
			CallUniqueID:     ref,
			PeerHandle:       "fixture-peer",
			PeerNormalized:   "fixture-peer",
			Service:          repository.PhoneCallServiceVoice,
			Direction:        repository.PhoneCallDirectionOutbound,
			DurationSeconds:  duration,
			StartedAt:        connectedAt,
			MatchedContactID: &contact.ID,
			InteractionID:    &interactionID,
		})
		require.NoError(t, err)
	}

	insertCall(t, automatic, "automatic", 60)
	insertCall(t, overridden, "override", 60)
	insertCall(t, missed, "missed", 0)
	insertCall(t, newer, "newer", 60)
	insertCall(t, deleted, "deleted", 60)
	insertCall(t, skipped, "skipped", 60)
	insertCall(t, deletedInteraction, "deleted-interaction", 60)
	deletedRow, err := interactionRepo.FindBySourceRef(ctx, deletedInteraction.ID, repository.InteractionSourcePhoneCalls, "repair-fixture-deleted-interaction")
	require.NoError(t, err)
	require.NoError(t, interactionRepo.SoftDeleteInteraction(ctx, deletedRow.ID))
	require.NoError(t, m.Steps(1))

	connected, err := interactionRepo.FindBySourceRef(ctx, automatic.ID, repository.InteractionSourcePhoneCalls, "repair-fixture-automatic")
	require.NoError(t, err)
	require.Equal(t, repository.InteractionDirectionMutual, connected.Direction)
	connectedContact, err := contactRepo.GetContact(ctx, automatic.ID)
	require.NoError(t, err)
	require.NotNil(t, connectedContact.LastContacted)
	require.NotNil(t, connectedContact.LastInteractionAt)
	require.NotNil(t, connectedContact.LastOutreachAt)
	require.NotNil(t, connectedContact.LastResponseAt)
	require.NotNil(t, connectedContact.ContactBy)
	require.Equal(t, connectedAt.UTC(), connectedContact.LastContacted.UTC())
	require.Equal(t, connectedAt.UTC(), connectedContact.LastInteractionAt.UTC())
	require.Equal(t, connectedAt.UTC(), connectedContact.LastOutreachAt.UTC())
	require.Equal(t, connectedAt.UTC(), connectedContact.LastResponseAt.UTC())
	require.NotNil(t, connectedContact.AwaitingReplyUntil)
	require.Equal(t, cadence.CalendarDate(awaitingUntil), cadence.CalendarDate(*connectedContact.AwaitingReplyUntil))
	require.False(t, cadence.IsAwaitingReply(connectedContact.LastOutreachAt, connectedContact.LastResponseAt, connectedContact.AwaitingReplyUntil, now))
	require.Equal(t, connectedAt.AddDate(0, 0, 30).UTC().Format("2006-01-02"), connectedContact.ContactBy.UTC().Format("2006-01-02"))

	overrideContact, err := contactRepo.GetContact(ctx, overridden.ID)
	require.NoError(t, err)
	require.Equal(t, repository.InteractionDirectionMutual, mustFindInteractionDirection(t, interactionRepo, overridden.ID, "repair-fixture-override"))
	require.Equal(t, overriddenContactBy.UTC().Format("2006-01-02"), overrideContact.ContactBy.UTC().Format("2006-01-02"))

	require.Equal(t, repository.InteractionDirectionOutbound, mustFindInteractionDirection(t, interactionRepo, missed.ID, "repair-fixture-missed"))
	require.Equal(t, repository.InteractionDirectionMutual, mustFindInteractionDirection(t, interactionRepo, newer.ID, "repair-fixture-newer"))
	newerContact, err := contactRepo.GetContact(ctx, newer.ID)
	require.NoError(t, err)
	require.Equal(t, newerLastContacted.UTC(), newerContact.LastContacted.UTC(), "a newer cadence timestamp wins")
	require.Equal(t, newerLastInteraction.UTC(), newerContact.LastInteractionAt.UTC(), "a newer interaction timestamp wins")
	require.Equal(t, newerLastOutreach.UTC(), newerContact.LastOutreachAt.UTC(), "a newer outreach timestamp wins")
	require.Equal(t, newerLastResponse.UTC(), newerContact.LastResponseAt.UTC(), "a newer response timestamp wins")
	require.Equal(t, newerContactBy.UTC().Format("2006-01-02"), newerContact.ContactBy.UTC().Format("2006-01-02"), "a newer cadence date wins")
	require.Equal(t, repository.InteractionDirectionOutbound, mustFindInteractionDirection(t, interactionRepo, deleted.ID, "repair-fixture-deleted"))
	require.Equal(t, repository.InteractionDirectionMutual, mustFindInteractionDirection(t, interactionRepo, skipped.ID, "repair-fixture-skipped"))
	skippedContact, err := contactRepo.GetContact(ctx, skipped.ID)
	require.NoError(t, err)
	require.Equal(t, skippedTo.UTC().Format("2006-01-02"), skippedContact.ContactBy.UTC().Format("2006-01-02"))
	require.Equal(t, oldAutomaticContactBy.UTC().Format("2006-01-02"), skippedContact.LastSkippedContactBy.UTC().Format("2006-01-02"))
	require.Equal(t, skippedAt.UTC(), skippedContact.LastSkippedAt.UTC())
	require.Equal(t, "fixture", *skippedContact.LastSkipReason)
	for _, timestamp := range []*time.Time{skippedContact.LastContacted, skippedContact.LastInteractionAt, skippedContact.LastOutreachAt, skippedContact.LastResponseAt} {
		require.NotNil(t, timestamp)
		require.Equal(t, connectedAt.UTC(), timestamp.UTC())
	}
	// Deleted interactions remain history and are deliberately outside the
	// repair selector.
	deletedInteractionRow, err := interactionRepo.TestGetInteractionIncludingDeleted(ctx, deletedInteraction.ID, repository.InteractionSourcePhoneCalls, "repair-fixture-deleted-interaction")
	require.NoError(t, err)
	require.Equal(t, repository.InteractionDirectionOutbound, deletedInteractionRow.Direction)

	// The down migration is intentionally a no-op; stepping down and up again
	// proves the repair selector is harmless after the rows are already mutual.
	require.NoError(t, m.Steps(-1))
	require.NoError(t, m.Steps(1))
	require.Equal(t, repository.InteractionDirectionMutual, mustFindInteractionDirection(t, interactionRepo, automatic.ID, "repair-fixture-automatic"))
}

func mustFindInteractionDirection(t *testing.T, repo *repository.InteractionRepository, contactID uuid.UUID, sourceRef string) string {
	t.Helper()
	interaction, err := repo.FindBySourceRef(context.Background(), contactID, repository.InteractionSourcePhoneCalls, sourceRef)
	require.NoError(t, err)
	return interaction.Direction
}
