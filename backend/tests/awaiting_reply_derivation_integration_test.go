//go:build integration_testdb

package tests

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/cadence"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/consumer"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type awaitingReplyDerivationEnv struct {
	ctx             context.Context
	database        *db.Database
	contactRepo     *repository.ContactRepository
	interactionRepo *repository.InteractionRepository
	cadenceUpdater  *consumer.CadenceUpdater
	watchdog        config.WatchdogConfig
}

func newAwaitingReplyDerivationEnv(t *testing.T) *awaitingReplyDerivationEnv {
	t.Helper()
	t.Parallel()
	ctx := context.Background()
	database, _ := newSharedTestDB(t, ctx)
	contactRepo := repository.NewContactRepository(database.Queries)
	contactRepo.SetPool(database.Pool)
	claims := repository.NewEventConsumerClaimRepository(database.Queries)
	watchdog := config.TestConfig().Watchdog
	return &awaitingReplyDerivationEnv{
		ctx:             ctx,
		database:        database,
		contactRepo:     contactRepo,
		interactionRepo: repository.NewInteractionRepository(database.Queries),
		cadenceUpdater:  consumer.NewCadenceUpdater(claims, contactRepo, database.Queries, consumer.CadenceModeCutover, false, watchdog),
		watchdog:        watchdog,
	}
}

func (e *awaitingReplyDerivationEnv) createMonthlyContact(t *testing.T, suffix string) *repository.Contact {
	t.Helper()
	monthly := "monthly"
	contact, err := e.contactRepo.CreateContact(e.ctx, repository.CreateContactRequest{
		FullName: "Awaiting Reply " + syntheticNS(t) + " " + suffix,
		Cadence:  &monthly,
	})
	require.NoError(t, err)
	return contact
}

func (e *awaitingReplyDerivationEnv) applyInteraction(t *testing.T, contactID uuid.UUID, direction, source string, occurredAt time.Time) *repository.Interaction {
	t.Helper()
	tx, err := e.database.Pool.Begin(e.ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(e.ctx) }()
	interaction, err := e.interactionRepo.CreateInteractionTx(e.ctx, tx, repository.CreateInteractionRequest{
		ContactID:  contactID,
		Source:     source,
		OccurredAt: occurredAt,
		Direction:  direction,
	})
	require.NoError(t, err)
	require.NoError(t, e.cadenceUpdater.ApplyInteraction(e.ctx, tx, repository.ApplyInteractionRequest{
		ContactID:  contactID,
		Direction:  direction,
		Source:     source,
		OccurredAt: occurredAt,
	}))
	require.NoError(t, tx.Commit(e.ctx))
	return interaction
}

func (e *awaitingReplyDerivationEnv) deleteInteractionAndRecompute(t *testing.T, contactID, interactionID uuid.UUID, occurredAt time.Time) {
	t.Helper()
	tx, err := e.database.Pool.Begin(e.ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(e.ctx) }()
	require.NoError(t, e.interactionRepo.SoftDeleteInteractionTx(e.ctx, tx, interactionID))
	require.NoError(t, e.contactRepo.RecomputeContactDatesAfterDeleteTx(e.ctx, tx, contactID, occurredAt, e.watchdog))
	require.NoError(t, tx.Commit(e.ctx))
}

func assertExpiryDate(t *testing.T, contact *repository.Contact, want time.Time) {
	t.Helper()
	require.NotNil(t, contact.AwaitingReplyUntil)
	assert.Equal(t, cadence.CalendarDate(want), cadence.CalendarDate(*contact.AwaitingReplyUntil))
}

func TestAwaitingReplyDerivation_OutboundWindowIsForwardOnly(t *testing.T) {
	e := newAwaitingReplyDerivationEnv(t)
	contact := e.createMonthlyContact(t, "forward-only")
	now := accelerated.GetCurrentTime()
	t1 := now.AddDate(0, 0, -5)
	t2 := now.AddDate(0, 0, -2)

	e.applyInteraction(t, contact.ID, repository.InteractionDirectionOutbound, repository.InteractionSourceTelegram, t1)
	first, err := e.contactRepo.GetContact(e.ctx, contact.ID)
	require.NoError(t, err)
	firstUntil := cadence.AwaitingReplyUntil(t1, e.watchdog.DaysForCadence("monthly"))
	assertExpiryDate(t, first, firstUntil)

	e.applyInteraction(t, contact.ID, repository.InteractionDirectionOutbound, repository.InteractionSourceTelegram, t2)
	second, err := e.contactRepo.GetContact(e.ctx, contact.ID)
	require.NoError(t, err)
	secondUntil := cadence.AwaitingReplyUntil(t2, e.watchdog.DaysForCadence("monthly"))
	assertExpiryDate(t, second, secondUntil)
	assert.True(t, cadence.CalendarDate(secondUntil).After(cadence.CalendarDate(firstUntil)))

	e.applyInteraction(t, contact.ID, repository.InteractionDirectionOutbound, repository.InteractionSourceTelegram, t1.Add(-24*time.Hour))
	lateIngest, err := e.contactRepo.GetContact(e.ctx, contact.ID)
	require.NoError(t, err)
	assertExpiryDate(t, lateIngest, secondUntil)
}

func TestAwaitingReplyDerivation_InboundLeavesExpiryAndEndsAwaiting(t *testing.T) {
	e := newAwaitingReplyDerivationEnv(t)
	contact := e.createMonthlyContact(t, "inbound")
	outreach := accelerated.GetCurrentTime().AddDate(0, 0, -2)
	e.applyInteraction(t, contact.ID, repository.InteractionDirectionOutbound, repository.InteractionSourceTelegram, outreach)
	afterOutbound, err := e.contactRepo.GetContact(e.ctx, contact.ID)
	require.NoError(t, err)
	wantUntil := cadence.AwaitingReplyUntil(outreach, e.watchdog.DaysForCadence("monthly"))
	assertExpiryDate(t, afterOutbound, wantUntil)

	response := outreach.Add(24 * time.Hour)
	e.applyInteraction(t, contact.ID, repository.InteractionDirectionInbound, repository.InteractionSourceTelegram, response)
	afterInbound, err := e.contactRepo.GetContact(e.ctx, contact.ID)
	require.NoError(t, err)
	assertExpiryDate(t, afterInbound, wantUntil)
	assert.False(t, cadence.IsAwaitingReply(afterInbound.LastOutreachAt, afterInbound.LastResponseAt, afterInbound.AwaitingReplyUntil, accelerated.GetCurrentTime()))
}

func TestAwaitingReplyDerivation_MutualDoesNotSetExpiry(t *testing.T) {
	e := newAwaitingReplyDerivationEnv(t)
	contact := e.createMonthlyContact(t, "mutual")
	e.applyInteraction(t, contact.ID, repository.InteractionDirectionMutual, repository.InteractionSourceTelegram, accelerated.GetCurrentTime())
	got, err := e.contactRepo.GetContact(e.ctx, contact.ID)
	require.NoError(t, err)
	assert.Nil(t, got.AwaitingReplyUntil)
}

func TestAwaitingReplyDerivation_ManualOutboundWritesUnconditionally(t *testing.T) {
	e := newAwaitingReplyDerivationEnv(t)
	contact := e.createMonthlyContact(t, "manual")
	base := cadence.Today(accelerated.GetCurrentTime())
	futureExpiry := base.AddDate(0, 0, 30)
	require.NoError(t, e.contactRepo.TestSeedContactCadenceFields(e.ctx, contact.ID, repository.TestCadenceSeed{AwaitingReplyUntil: &futureExpiry}))
	outreach := accelerated.GetCurrentTime().AddDate(0, 0, -5)
	e.applyInteraction(t, contact.ID, repository.InteractionDirectionOutbound, repository.InteractionSourceManual, outreach)
	got, err := e.contactRepo.GetContact(e.ctx, contact.ID)
	require.NoError(t, err)
	assertExpiryDate(t, got, cadence.AwaitingReplyUntil(outreach, e.watchdog.DaysForCadence("monthly")))
}

func TestAwaitingReplyDerivation_BulkApplyTakesForwardMaximum(t *testing.T) {
	e := newAwaitingReplyDerivationEnv(t)
	contact := e.createMonthlyContact(t, "bulk-apply")
	base := cadence.Today(accelerated.GetCurrentTime())
	oldExpiry := base.AddDate(0, 0, -3)
	laterExpiry := base.AddDate(0, 0, 9)
	newerButLowerExpiry := base.AddDate(0, 0, 4)
	require.NoError(t, e.contactRepo.TestSeedContactCadenceFields(e.ctx, contact.ID, repository.TestCadenceSeed{AwaitingReplyUntil: &oldExpiry}))

	require.NoError(t, pgx.BeginTxFunc(e.ctx, e.database.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		return e.cadenceUpdater.BulkApply(e.ctx, tx, contact.ID, repository.ContactCadenceFields{AwaitingReplyUntil: &laterExpiry})
	}))
	got, err := e.contactRepo.GetContact(e.ctx, contact.ID)
	require.NoError(t, err)
	assertExpiryDate(t, got, laterExpiry)

	require.NoError(t, pgx.BeginTxFunc(e.ctx, e.database.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		return e.cadenceUpdater.BulkApply(e.ctx, tx, contact.ID, repository.ContactCadenceFields{AwaitingReplyUntil: &newerButLowerExpiry})
	}))
	got, err = e.contactRepo.GetContact(e.ctx, contact.ID)
	require.NoError(t, err)
	assertExpiryDate(t, got, laterExpiry)
}

func TestAwaitingReplyDerivation_DeleteRollbackRecomputesExpiry(t *testing.T) {
	e := newAwaitingReplyDerivationEnv(t)
	contact := e.createMonthlyContact(t, "delete-recompute")
	now := accelerated.GetCurrentTime()
	t1 := now.AddDate(0, 0, -5)
	t2 := now.AddDate(0, 0, -2)
	one := e.applyInteraction(t, contact.ID, repository.InteractionDirectionOutbound, repository.InteractionSourceTelegram, t1)
	two := e.applyInteraction(t, contact.ID, repository.InteractionDirectionOutbound, repository.InteractionSourceTelegram, t2)

	e.deleteInteractionAndRecompute(t, contact.ID, two.ID, t2)
	got, err := e.contactRepo.GetContact(e.ctx, contact.ID)
	require.NoError(t, err)
	assertExpiryDate(t, got, cadence.AwaitingReplyUntil(t1, e.watchdog.DaysForCadence("monthly")))

	e.deleteInteractionAndRecompute(t, contact.ID, one.ID, t1)
	got, err = e.contactRepo.GetContact(e.ctx, contact.ID)
	require.NoError(t, err)
	assert.Nil(t, got.LastOutreachAt)
	assert.Nil(t, got.AwaitingReplyUntil)
}

func TestAwaitingReplyDerivation_ParityMatrix(t *testing.T) {
	e := newAwaitingReplyDerivationEnv(t)
	namespace := syntheticNS(t)
	asOf := cadence.Today(accelerated.GetCurrentTime()).AddDate(0, 0, -45)
	noon := func(day time.Time) time.Time { return day.Add(12 * time.Hour) }
	baseOutreach := noon(asOf.AddDate(0, 0, -5))
	olderResponse := noon(asOf.AddDate(0, 0, -10))
	boundaryExpiry := asOf
	dayBeforeExpiry := asOf.AddDate(0, 0, -1)
	openExpiry := asOf.AddDate(0, 0, 1)

	rows := []struct {
		name     string
		outreach *time.Time
		response *time.Time
		until    *time.Time
		want     bool
	}{
		{name: "expiry-equals-as-of", outreach: &baseOutreach, response: &olderResponse, until: &boundaryExpiry, want: true},
		{name: "expiry-before-as-of", outreach: &baseOutreach, response: &olderResponse, until: &dayBeforeExpiry, want: false},
		{name: "outreach-null", response: &olderResponse, until: &openExpiry, want: false},
		{name: "response-null-open-window", outreach: &baseOutreach, until: &boundaryExpiry, want: true},
		{name: "expiry-null", outreach: &baseOutreach, response: &olderResponse, want: false},
		{name: "outreach-equals-response", outreach: &baseOutreach, response: &baseOutreach, until: &boundaryExpiry, want: false},
		{name: "outreach-after-response-open-window", outreach: &baseOutreach, response: &olderResponse, until: &boundaryExpiry, want: true},
	}

	for _, row := range rows {
		row := row
		t.Run(row.name, func(t *testing.T) {
			name := fmt.Sprintf("awaiting-%s-%s", namespace, row.name)
			monthly := "monthly"
			contact, err := e.contactRepo.CreateContact(e.ctx, repository.CreateContactRequest{FullName: name, Cadence: &monthly})
			require.NoError(t, err)
			require.NoError(t, e.contactRepo.TestSeedContactCadenceFields(e.ctx, contact.ID, repository.TestCadenceSeed{
				LastOutreachAt:     row.outreach,
				LastResponseAt:     row.response,
				AwaitingReplyUntil: row.until,
			}))

			appRule := cadence.IsAwaitingReply(row.outreach, row.response, row.until, noon(asOf))
			assert.Equal(t, row.want, appRule, "Go rule must match the stated row expectation")
			for _, filter := range []struct {
				value string
				want  bool
			}{
				{value: "has_followup", want: row.want},
				{value: "no_followup", want: !row.want},
			} {
				params := repository.ListContactsParams{
					Query: name, Limit: 20, FollowupFilter: filter.value, AsOfDate: asOf,
				}
				contacts, err := e.contactRepo.ListContacts(e.ctx, params)
				require.NoError(t, err)
				listed := false
				for _, listedContact := range contacts {
					if listedContact.ID == contact.ID {
						listed = true
					}
				}
				assert.Equal(t, filter.want, listed, "%s list membership must match the stated expectation", filter.value)

				count, err := e.contactRepo.CountContacts(e.ctx, params)
				require.NoError(t, err)
				assert.EqualValues(t, boolInt(filter.want), count, "%s count must match the stated expectation", filter.value)

				ids, err := e.contactRepo.ListContactIDs(e.ctx, repository.ListContactIDsParams{
					Search: name, FollowupFilter: filter.value, AsOfDate: asOf,
				})
				require.NoError(t, err)
				idListed := false
				for _, id := range ids {
					if id == contact.ID {
						idListed = true
					}
				}
				assert.Equal(t, filter.want, idListed, "%s ID membership must match the stated expectation", filter.value)
			}
		})
	}

	today := cadence.Today(accelerated.GetCurrentTime())
	monthly := "monthly"
	zeroOpenContact, err := e.contactRepo.CreateContact(e.ctx, repository.CreateContactRequest{
		FullName: "awaiting-" + namespace + "-zero-as-of-open", Cadence: &monthly,
	})
	require.NoError(t, err)
	outreach := noon(today.AddDate(0, 0, -1))
	require.NoError(t, e.contactRepo.TestSeedContactCadenceFields(e.ctx, zeroOpenContact.ID, repository.TestCadenceSeed{
		LastOutreachAt: &outreach, AwaitingReplyUntil: &today,
	}))
	expiredContact, err := e.contactRepo.CreateContact(e.ctx, repository.CreateContactRequest{
		FullName: "awaiting-" + namespace + "-zero-as-of-expired", Cadence: &monthly,
	})
	require.NoError(t, err)
	expiredOutreach := noon(today.AddDate(0, 0, -8))
	expiredUntil := today.AddDate(0, 0, -1)
	require.NoError(t, e.contactRepo.TestSeedContactCadenceFields(e.ctx, expiredContact.ID, repository.TestCadenceSeed{
		LastOutreachAt: &expiredOutreach, AwaitingReplyUntil: &expiredUntil,
	}))
	searchPrefix := strings.ReplaceAll("awaiting-"+namespace+"-zero-as-of", "-", " ")
	zeroParams := repository.ListContactsParams{
		Query: searchPrefix, Limit: 20, FollowupFilter: "has_followup",
	}
	todayParams := zeroParams
	todayParams.AsOfDate = today
	zeroList, err := e.contactRepo.ListContacts(e.ctx, zeroParams)
	require.NoError(t, err)
	todayList, err := e.contactRepo.ListContacts(e.ctx, todayParams)
	require.NoError(t, err)
	assert.Equal(t, contactIDs(zeroList), contactIDs(todayList), "zero AsOfDate must use the app-clock calendar date")
	zeroParams.FollowupFilter = "no_followup"
	noFollowupList, err := e.contactRepo.ListContacts(e.ctx, zeroParams)
	require.NoError(t, err)
	assert.Contains(t, contactIDs(zeroList), zeroOpenContact.ID)
	assert.NotContains(t, contactIDs(zeroList), expiredContact.ID)
	assert.Contains(t, contactIDs(noFollowupList), expiredContact.ID)
	assert.NotContains(t, contactIDs(noFollowupList), zeroOpenContact.ID)
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func contactIDs(contacts []repository.Contact) []uuid.UUID {
	ids := make([]uuid.UUID, len(contacts))
	for i := range contacts {
		ids[i] = contacts[i].ID
	}
	return ids
}
