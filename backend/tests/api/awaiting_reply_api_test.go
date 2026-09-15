package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/cadence"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// spec: CON-005.live-contact-flag, CON-005.list-entries-carry-flag
// spec: CON-018.followup-filter-closed-set
func TestContactAPI_FourConsumersAgree(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()

	router, contactRepo, _, cleanup := setupAwaitingReplyAPIRouter(t)
	defer cleanup()

	now := accelerated.GetCurrentTime().UTC().Truncate(time.Second)
	contactBy := cadence.Today(now).AddDate(0, 0, -10)
	cadenceName := "monthly"
	prefix := "Awaiting Reply Consumers " + uuid.NewString()[:8]
	awaitingName := prefix + " Awaiting"
	closedName := prefix + " Closed"
	awaitingID := createDirectionTestContactWithCadence(t, router, awaitingName, &cadenceName)
	closedID := createDirectionTestContactWithCadence(t, router, closedName, &cadenceName)

	for _, id := range []string{awaitingID, closedID} {
		contactUUID, err := uuid.Parse(id)
		require.NoError(t, err)
		require.NoError(t, contactRepo.TestSeedContactCadenceFields(context.Background(), contactUUID, repository.TestCadenceSeed{
			ContactBy: &contactBy,
		}))
	}
	recordDirectionTestInteraction(t, router, awaitingID, "outbound", now.Add(-time.Hour))
	recordDirectionTestInteraction(t, router, closedID, "outbound", now.AddDate(0, 0, -30))

	for _, candidate := range []struct {
		id   string
		name string
	}{
		{id: awaitingID, name: awaitingName},
		{id: closedID, name: closedName},
	} {
		t.Run(candidate.name, func(t *testing.T) {
			detail := fetchContactEntry(t, router, "/api/v1/contacts/"+candidate.id, candidate.id)
			contactUUID, err := uuid.Parse(candidate.id)
			require.NoError(t, err)
			contact, err := contactRepo.GetContact(context.Background(), contactUUID)
			require.NoError(t, err)
			expected := cadence.IsAwaitingReply(
				payloadTime(t, detail, "last_outreach_at"),
				payloadTime(t, detail, "last_response_at"),
				contact.AwaitingReplyUntil,
				accelerated.GetCurrentTime(),
			)
			assert.Equal(t, expected, detail["awaiting_reply"])

			listEntry := fetchContactEntry(t, router,
				"/api/v1/contacts?search="+url.QueryEscape(candidate.name), candidate.id)
			assert.Equal(t, expected, listEntry["awaiting_reply"])

			overdueEntry := fetchContactEntry(t, router, "/api/v1/contacts/overdue", candidate.id)
			assert.Equal(t, expected, overdueEntry["awaiting_reply"])

			hasFollowup := contactEntryIfPresent(t, router,
				"/api/v1/contacts?followup_filter=has_followup&search="+url.QueryEscape(candidate.name), candidate.id)
			noFollowup := contactEntryIfPresent(t, router,
				"/api/v1/contacts?followup_filter=no_followup&search="+url.QueryEscape(candidate.name), candidate.id)
			assert.Equal(t, expected, hasFollowup != nil)
			assert.Equal(t, !expected, noFollowup != nil)
			if hasFollowup != nil {
				assert.Equal(t, expected, hasFollowup["awaiting_reply"])
			}
			if noFollowup != nil {
				assert.Equal(t, expected, noFollowup["awaiting_reply"])
			}
		})
	}
}

func fetchContactEntry(t *testing.T, router http.Handler, path, contactID string) map[string]interface{} {
	t.Helper()
	entry := contactEntryIfPresent(t, router, path, contactID)
	require.NotNil(t, entry, "contact %s is present in %s", contactID, path)
	return entry
}

func contactEntryIfPresent(t *testing.T, router http.Handler, path, contactID string) map[string]interface{} {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var envelope api.APIResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &envelope))

	switch data := envelope.Data.(type) {
	case map[string]interface{}:
		if data["id"] == contactID {
			return data
		}
	case []interface{}:
		for _, value := range data {
			entry := value.(map[string]interface{})
			if entry["id"] == contactID {
				return entry
			}
		}
	default:
		require.FailNow(t, "unexpected contact API response shape", "%T", envelope.Data)
	}
	return nil
}

func payloadTime(t *testing.T, payload map[string]interface{}, key string) *time.Time {
	t.Helper()
	value, exists := payload[key]
	if !exists || value == nil {
		return nil
	}
	dateString, ok := value.(string)
	require.True(t, ok, "%s is a string timestamp", key)
	parsed, err := time.Parse(time.RFC3339Nano, dateString)
	require.NoError(t, err)
	return &parsed
}
