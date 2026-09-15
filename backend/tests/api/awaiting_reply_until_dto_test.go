package api

import (
	"context"
	"net/url"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/cadence"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestContactAPI_AwaitingReplyUntilOnPayloads(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()

	router, contactRepo, _, cleanup := setupAwaitingReplyAPIRouter(t)
	defer cleanup()

	now := accelerated.GetCurrentTime().UTC().Truncate(time.Second)
	contactBy := cadence.Today(now).AddDate(0, 0, -10)
	cadenceName := "monthly"
	prefix := "Awaiting Reply DTO " + uuid.NewString()
	awaitingName := prefix + " Awaiting"
	noOutreachName := prefix + " No Outreach"
	awaitingID := createDirectionTestContactWithCadence(t, router, awaitingName, &cadenceName)
	noOutreachID := createDirectionTestContactWithCadence(t, router, noOutreachName, &cadenceName)

	for _, id := range []string{awaitingID, noOutreachID} {
		contactID, err := uuid.Parse(id)
		require.NoError(t, err)
		require.NoError(t, contactRepo.TestSeedContactCadenceFields(context.Background(), contactID, repository.TestCadenceSeed{
			ContactBy: &contactBy,
		}))
	}
	recordDirectionTestInteraction(t, router, awaitingID, "outbound", now.Add(-time.Hour))

	awaitingUUID, err := uuid.Parse(awaitingID)
	require.NoError(t, err)
	stored, err := contactRepo.GetContact(context.Background(), awaitingUUID)
	require.NoError(t, err)
	require.NotNil(t, stored.AwaitingReplyUntil)

	awaitingEntries := []map[string]interface{}{
		fetchContactEntry(t, router, "/api/v1/contacts/"+awaitingID, awaitingID),
		fetchContactEntry(t, router, "/api/v1/contacts?search="+url.QueryEscape(awaitingName), awaitingID),
		fetchContactEntry(t, router, "/api/v1/contacts/overdue", awaitingID),
	}
	for _, entry := range awaitingEntries {
		got := payloadTime(t, entry, "awaiting_reply_until")
		require.NotNil(t, got)
		assert.True(t, got.Equal(*stored.AwaitingReplyUntil))
		assert.Equal(t, cadence.CalendarDate(cadence.AwaitingReplyUntil(now.Add(-time.Hour), 7)), cadence.CalendarDate(*got))
		assert.Equal(t, true, entry["awaiting_reply"])
	}

	noOutreachEntries := []map[string]interface{}{
		fetchContactEntry(t, router, "/api/v1/contacts/"+noOutreachID, noOutreachID),
		fetchContactEntry(t, router, "/api/v1/contacts?search="+url.QueryEscape(noOutreachName), noOutreachID),
		fetchContactEntry(t, router, "/api/v1/contacts/overdue", noOutreachID),
	}
	for _, entry := range noOutreachEntries {
		_, present := entry["awaiting_reply_until"]
		assert.False(t, present)
		assert.Equal(t, false, entry["awaiting_reply"])
	}
}
