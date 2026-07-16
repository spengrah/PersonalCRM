package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// listCandidateIDs fetches the candidate list with the given query string and
// returns the response ids in order plus the response meta.
func listCandidateIDs(t *testing.T, router http.Handler, query string) ([]string, *api.Meta) {
	t.Helper()
	req, _ := http.NewRequest("GET", "/api/v1/imports/candidates?"+query, nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	var response struct {
		Success bool `json:"success"`
		Data    []struct {
			ID string `json:"id"`
		} `json:"data"`
		Meta *api.Meta `json:"meta"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	require.True(t, response.Success)

	ids := make([]string, 0, len(response.Data))
	for _, c := range response.Data {
		ids = append(ids, c.ID)
	}
	return ids, response.Meta
}

// The candidate list is globally confidence-ranked: suggested candidates
// sort by confidence descending ahead of unsuggested ones, unsuggested
// candidates order by name with empty names last; unresolved telegram
// peers are hidden by default behind a surfaced count and an opt-in flag;
// weak title-derived discovery rows never appear in this list.
// spec: IMP-010
func TestImportAPI_CandidateListConfidenceOrder(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	t.Parallel()
	router, externalRepo, contactRepo, contactMethodRepo, cleanup := setupImportTestRouter()
	defer cleanup()

	ctx := context.Background()
	suffix := uuid.New().String()[:8]

	seedExternal := func(t *testing.T, req repository.UpsertExternalContactRequest) *repository.ExternalContact {
		t.Helper()
		req.SourceID = uuid.New().String()
		external, err := externalRepo.Upsert(ctx, req)
		require.NoError(t, err)
		t.Cleanup(func() { _ = externalRepo.Delete(ctx, external.ID) })
		return external
	}
	strPtr := func(s string) *string { return &s }

	// Two CRM contacts to match against. The high-confidence match shares
	// name AND email (method overlap); the medium one shares only the name.
	highName := fmt.Sprintf("Ordertest High %s", suffix)
	highEmail := fmt.Sprintf("order-high-%s@example.com", suffix)
	highContact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{FullName: highName})
	require.NoError(t, err)
	t.Cleanup(func() { _ = contactRepo.HardDeleteContact(ctx, highContact.ID) })
	_, err = contactMethodRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
		ContactID: highContact.ID,
		Type:      "email",
		Value:     highEmail,
	})
	require.NoError(t, err)

	medName := fmt.Sprintf("Ordertest Medium %s", suffix)
	medContact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{FullName: medName})
	require.NoError(t, err)
	t.Cleanup(func() { _ = contactRepo.HardDeleteContact(ctx, medContact.ID) })

	// Unmatched externals: high confidence (name+email), medium confidence
	// (name only), two unsuggested rows whose relative name order is fixed,
	// and one unsuggested row with no name at all.
	externalHigh := seedExternal(t, repository.UpsertExternalContactRequest{
		Source:      "test",
		DisplayName: strPtr(highName),
		Emails:      []repository.EmailEntry{{Value: highEmail, Type: "home"}},
	})
	externalMed := seedExternal(t, repository.UpsertExternalContactRequest{
		Source:      "test",
		DisplayName: strPtr(medName),
		Emails:      []repository.EmailEntry{{Value: fmt.Sprintf("order-med-other-%s@example.com", suffix), Type: "home"}},
	})
	externalNameA := seedExternal(t, repository.UpsertExternalContactRequest{
		Source:      "test",
		DisplayName: strPtr(fmt.Sprintf("Aaaqwx %s Unsuggested", suffix)),
	})
	externalNameZ := seedExternal(t, repository.UpsertExternalContactRequest{
		Source:      "test",
		DisplayName: strPtr(fmt.Sprintf("Zzzqwx %s Unsuggested", suffix)),
	})
	externalNoName := seedExternal(t, repository.UpsertExternalContactRequest{
		Source: "test",
	})

	// An unresolved telegram peer (no names, no username, no methods) and a
	// weak title-derived discovery row.
	externalUnresolvedTG := seedExternal(t, repository.UpsertExternalContactRequest{
		Source: "telegram",
	})
	externalTitleToken := seedExternal(t, repository.UpsertExternalContactRequest{
		Source:      "anarlog_title",
		DisplayName: strPtr(fmt.Sprintf("Titletoken %s", suffix)),
		Metadata: map[string]any{
			"token_normalized": fmt.Sprintf("titletoken %s", suffix),
			"token_display":    fmt.Sprintf("Titletoken %s", suffix),
			"session_uuid":     uuid.New().String(),
		},
	})

	ids, meta := listCandidateIDs(t, router, "limit=10000")

	highIdx := indexOf(ids, externalHigh.ID.String())
	medIdx := indexOf(ids, externalMed.ID.String())
	nameAIdx := indexOf(ids, externalNameA.ID.String())
	nameZIdx := indexOf(ids, externalNameZ.ID.String())
	noNameIdx := indexOf(ids, externalNoName.ID.String())

	require.NotEqual(t, -1, highIdx, "high-confidence candidate should be listed")
	require.NotEqual(t, -1, medIdx, "medium-confidence candidate should be listed")
	require.NotEqual(t, -1, nameAIdx, "unsuggested candidate A should be listed")
	require.NotEqual(t, -1, nameZIdx, "unsuggested candidate Z should be listed")
	require.NotEqual(t, -1, noNameIdx, "empty-name candidate should be listed")

	// Confidence descending among suggested candidates.
	assert.Less(t, highIdx, medIdx, "name+email match should rank above name-only match")
	// Suggested candidates precede unsuggested ones.
	assert.Less(t, medIdx, nameAIdx, "suggested candidates should precede unsuggested ones")
	// Unsuggested candidates order by name...
	assert.Less(t, nameAIdx, nameZIdx, "unsuggested candidates should order by name")
	// ...with empty names last.
	assert.Less(t, nameZIdx, noNameIdx, "empty-name candidates should sort after named ones")

	// Unresolved telegram peers are hidden by default, with a count surfaced.
	assert.Equal(t, -1, indexOf(ids, externalUnresolvedTG.ID.String()),
		"unresolved telegram peer should be hidden by default")
	require.NotNil(t, meta)
	assert.GreaterOrEqual(t, meta.HiddenUnresolvedTelegramCount, int64(1))

	// The opt-in flag surfaces the hidden peer.
	idsWithTG, _ := listCandidateIDs(t, router, "limit=10000&include_unresolved_telegram=true")
	assert.NotEqual(t, -1, indexOf(idsWithTG, externalUnresolvedTG.ID.String()),
		"include_unresolved_telegram=true should surface the unresolved peer")

	// Weak title-derived discovery rows never appear in this list (they
	// surface only through the grouped names section).
	assert.Equal(t, -1, indexOf(ids, externalTitleToken.ID.String()),
		"anarlog_title rows should never appear in the candidate list")
	assert.Equal(t, -1, indexOf(idsWithTG, externalTitleToken.ID.String()),
		"anarlog_title rows should stay excluded even with the telegram opt-in")
}
