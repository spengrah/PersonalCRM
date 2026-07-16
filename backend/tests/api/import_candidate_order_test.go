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
//
// The shared test DB accumulates rows across runs, so every read here is
// source-scoped: the ordering rows live under a per-run unique source (the
// list endpoint filters by exact source, so that list contains exactly this
// test's rows), and the telegram/anarlog checks query their own sources.
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
	// Register the pool close FIRST so it runs LAST — t.Cleanup is LIFO, and
	// the fixture deletes registered below need the pool still open.
	t.Cleanup(cleanup)

	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	orderSource := "test-order-" + suffix

	seedExternal := func(t *testing.T, req repository.UpsertExternalContactRequest) *repository.ExternalContact {
		t.Helper()
		req.SourceID = uuid.New().String()
		external, err := externalRepo.Upsert(ctx, req)
		require.NoError(t, err)
		t.Cleanup(func() {
			if err := externalRepo.Delete(ctx, external.ID); err != nil {
				t.Errorf("cleanup: delete external contact %s: %v", external.ID, err)
			}
		})
		return external
	}
	strPtr := func(s string) *string { return &s }

	// Two CRM contacts to match against. The high-confidence match shares
	// name AND email (method overlap); the medium one shares only the name.
	highName := fmt.Sprintf("Ordertest High %s", suffix)
	highEmail := fmt.Sprintf("order-high-%s@example.com", suffix)
	highContact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{FullName: highName})
	require.NoError(t, err)
	t.Cleanup(func() {
		if err := contactRepo.HardDeleteContact(ctx, highContact.ID); err != nil {
			t.Errorf("cleanup: delete contact %s: %v", highContact.ID, err)
		}
	})
	_, err = contactMethodRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
		ContactID: highContact.ID,
		Type:      "email",
		Value:     highEmail,
	})
	require.NoError(t, err)

	medName := fmt.Sprintf("Ordertest Medium %s", suffix)
	medContact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{FullName: medName})
	require.NoError(t, err)
	t.Cleanup(func() {
		if err := contactRepo.HardDeleteContact(ctx, medContact.ID); err != nil {
			t.Errorf("cleanup: delete contact %s: %v", medContact.ID, err)
		}
	})

	// Unmatched externals under the test-owned source: high confidence
	// (name+email), medium confidence (name only), two unsuggested rows
	// whose relative name order is fixed, and one row with no name at all.
	externalHigh := seedExternal(t, repository.UpsertExternalContactRequest{
		Source:      orderSource,
		DisplayName: strPtr(highName),
		Emails:      []repository.EmailEntry{{Value: highEmail, Type: "home"}},
	})
	externalMed := seedExternal(t, repository.UpsertExternalContactRequest{
		Source:      orderSource,
		DisplayName: strPtr(medName),
		Emails:      []repository.EmailEntry{{Value: fmt.Sprintf("order-med-other-%s@example.com", suffix), Type: "home"}},
	})
	externalNameA := seedExternal(t, repository.UpsertExternalContactRequest{
		Source:      orderSource,
		DisplayName: strPtr(fmt.Sprintf("Aaaqwx %s Unsuggested", suffix)),
	})
	externalNameZ := seedExternal(t, repository.UpsertExternalContactRequest{
		Source:      orderSource,
		DisplayName: strPtr(fmt.Sprintf("Zzzqwx %s Unsuggested", suffix)),
	})
	externalNoName := seedExternal(t, repository.UpsertExternalContactRequest{
		Source: orderSource,
	})

	// The source-scoped list contains exactly the five seeded rows, in the
	// documented order: confidence descending among suggested candidates,
	// suggested before unsuggested, unsuggested by name, empty names last.
	ids, _ := listCandidateIDs(t, router, "source="+orderSource+"&limit=100")
	assert.Equal(t, []string{
		externalHigh.ID.String(),
		externalMed.ID.String(),
		externalNameA.ID.String(),
		externalNameZ.ID.String(),
		externalNoName.ID.String(),
	}, ids, "source-scoped candidate list should be confidence-ranked, then name-ordered with empty names last")

	// An unresolved telegram peer (no names, no username, no methods) is
	// hidden by default, with a count surfaced. The telegram-scoped read
	// keeps this independent of the global queue.
	externalUnresolvedTG := seedExternal(t, repository.UpsertExternalContactRequest{
		Source: "telegram",
	})
	tgIDs, tgMeta := listCandidateIDs(t, router, "source=telegram&limit=10000")
	assert.NotContains(t, tgIDs, externalUnresolvedTG.ID.String(),
		"unresolved telegram peer should be hidden by default")
	require.NotNil(t, tgMeta)
	assert.GreaterOrEqual(t, tgMeta.HiddenUnresolvedTelegramCount, int64(1))

	// The opt-in flag surfaces the hidden peer.
	tgIDsShown, _ := listCandidateIDs(t, router, "source=telegram&limit=10000&include_unresolved_telegram=true")
	assert.Contains(t, tgIDsShown, externalUnresolvedTG.ID.String(),
		"include_unresolved_telegram=true should surface the unresolved peer")

	// Weak title-derived discovery rows never appear in this list (they
	// surface only through the grouped names section) — even when the
	// caller asks for the anarlog_title source directly.
	externalTitleToken := seedExternal(t, repository.UpsertExternalContactRequest{
		Source:      "anarlog_title",
		DisplayName: strPtr(fmt.Sprintf("Titletoken %s", suffix)),
		Metadata: map[string]any{
			"token_normalized": fmt.Sprintf("titletoken %s", suffix),
			"token_display":    fmt.Sprintf("Titletoken %s", suffix),
			"session_uuid":     uuid.New().String(),
		},
	})
	titleIDs, _ := listCandidateIDs(t, router, "source=anarlog_title&limit=10000")
	assert.NotContains(t, titleIDs, externalTitleToken.ID.String(),
		"anarlog_title rows should never appear in the candidate list")
}
