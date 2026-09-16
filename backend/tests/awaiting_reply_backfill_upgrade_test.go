//go:build integration_testdb

package tests

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/cadence"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/testdb"

	migrate "github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAwaitingReplyBackfill_Upgrade082(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	ctx := context.Background()
	cloneURL, drop := testdb.NewEphemeralClone(t)
	t.Cleanup(drop)

	cfg := config.TestConfig()
	cfg.Database.URL = cloneURL
	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	t.Cleanup(database.Close)

	m, err := migrate.New(fmt.Sprintf("file://%s", getMigrationsPath()), cloneURL)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = m.Close() })

	contactRepo := repository.NewContactRepository(database.Queries)
	contactRepo.SetPool(database.Pool)
	now := accelerated.GetCurrentTime()
	localNoonDaysAgo := func(days int) time.Time {
		return cadence.Today(now).AddDate(0, 0, -days).Add(12 * time.Hour)
	}
	monthly := "monthly"
	createContact := func(name string, cadenceName *string, outreach, response *time.Time) *repository.Contact {
		t.Helper()
		contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: name,
			Cadence:  cadenceName,
		})
		require.NoError(t, err)
		seed := repository.TestCadenceSeed{LastOutreachAt: outreach, LastResponseAt: response}
		require.NoError(t, contactRepo.TestSeedContactCadenceFields(ctx, contact.ID, seed))
		return contact
	}

	openOutreach, openResponse := localNoonDaysAgo(2), localNoonDaysAgo(10)
	expiredOutreach, expiredResponse := localNoonDaysAgo(30), localNoonDaysAgo(40)
	answeredOutreach, answeredResponse := localNoonDaysAgo(2), localNoonDaysAgo(1)
	open := createContact("Awaiting Reply Backfill Open "+syntheticNS(t), &monthly, &openOutreach, &openResponse)
	expired := createContact("Awaiting Reply Backfill Expired "+syntheticNS(t), &monthly, &expiredOutreach, &expiredResponse)
	answered := createContact("Awaiting Reply Backfill Answered "+syntheticNS(t), &monthly, &answeredOutreach, &answeredResponse)
	noCadence := createContact("Awaiting Reply Backfill No Cadence "+syntheticNS(t), nil, &openOutreach, nil)
	deleted := createContact("Awaiting Reply Backfill Deleted "+syntheticNS(t), &monthly, &openOutreach, nil)
	require.NoError(t, contactRepo.SoftDeleteContact(ctx, deleted.ID))

	if err := m.Migrate(awaitingReplyUntilPreVersion); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		require.NoError(t, err, "position the clone before migration 082")
	}
	require.NoError(t, m.Steps(1))
	// 083 is additive; every contact-returning sqlc query expands to the head
	// column list, so reads happen at head.
	require.NoError(t, m.Up())

	assertBackfill := func(contact *repository.Contact, outreach time.Time, response *time.Time, wantAwaiting bool) {
		t.Helper()
		got, err := contactRepo.GetContact(ctx, contact.ID)
		require.NoError(t, err)
		require.NotNil(t, got.AwaitingReplyUntil)
		wantUntil := cadence.AwaitingReplyUntil(outreach, 7)
		assert.Equal(t, cadence.CalendarDate(wantUntil), cadence.CalendarDate(*got.AwaitingReplyUntil))
		assert.Equal(t, wantAwaiting, cadence.IsAwaitingReply(&outreach, response, got.AwaitingReplyUntil, accelerated.GetCurrentTime()))
	}
	assertBackfill(open, openOutreach, &openResponse, true)
	assertBackfill(expired, expiredOutreach, &expiredResponse, false)
	assertBackfill(answered, answeredOutreach, &answeredResponse, false)

	noCadenceLoaded, err := contactRepo.GetContact(ctx, noCadence.ID)
	require.NoError(t, err)
	assert.Nil(t, noCadenceLoaded.AwaitingReplyUntil)

	deletedExpiry, err := database.Queries.TestGetContactAwaitingReplyUntilIncludingDeleted(ctx, deleted.ID)
	require.NoError(t, err)
	assert.Nil(t, deletedExpiry)
}
