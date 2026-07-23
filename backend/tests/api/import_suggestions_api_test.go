package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// suggestionItem is the literal-wire-key decode of one SuggestionItemResponse
// entry: a generic map so assertions bind to the actual JSON keys the
// handler emits, never a round trip through the production DTO.
type suggestionItem = map[string]interface{}

// getSuggestions fetches the suggestions list with the given query string and
// decodes the raw response body, returning the item list (as generic maps)
// plus the response meta (also generic, so pagination keys are asserted
// literally).
func getSuggestions(t *testing.T, router http.Handler, query string) ([]suggestionItem, map[string]interface{}) {
	t.Helper()
	req, _ := http.NewRequest("GET", "/api/v1/imports/suggestions?"+query, nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var response struct {
		Success bool                   `json:"success"`
		Data    []suggestionItem       `json:"data"`
		Meta    map[string]interface{} `json:"meta"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	require.True(t, response.Success)
	return response.Data, response.Meta
}

// seedLinkedRowWithPendingMethod seeds a gcontacts external row linked
// (imported) to a fresh contact, carrying exactly one pending method
// suggestion for an email the external row itself carries (so it survives
// the "still on the external row" appliability check) but the contact does
// NOT yet have (so it survives the "not already on contact" drift check).
// Returns the contact and the refreshed external row (post SetMethodSuggestions).
func seedLinkedRowWithPendingMethod(
	t *testing.T,
	ctx context.Context,
	contactRepo *repository.ContactRepository,
	externalRepo *repository.ExternalContactRepository,
	suffix string,
) (*repository.Contact, *repository.ExternalContact) {
	t.Helper()

	name := fmt.Sprintf("Suggestion Method %s", suffix)
	contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{FullName: name})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = contactRepo.HardDeleteContact(ctx, contact.ID)
	})

	pendingEmail := fmt.Sprintf("pending-method-%s@example.com", suffix)
	displayName := name
	external, err := externalRepo.Upsert(ctx, repository.UpsertExternalContactRequest{
		Source:      "gcontacts",
		SourceID:    "sugg-method-" + suffix,
		DisplayName: &displayName,
		Emails:      []repository.EmailEntry{{Value: pendingEmail, Type: "home"}},
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = externalRepo.Delete(ctx, external.ID)
	})

	_, err = externalRepo.UpdateMatch(ctx, external.ID, &contact.ID, repository.MatchStatusImported)
	require.NoError(t, err)
	_, err = externalRepo.SetMethodSuggestions(ctx, external.ID, []repository.PendingMethodSuggestion{
		{Type: "email", Value: pendingEmail},
	})
	require.NoError(t, err)

	updated, err := externalRepo.GetByID(ctx, external.ID)
	require.NoError(t, err)
	return contact, updated
}

// findSuggestionItemIndex returns the index of the item whose kind/id match,
// or -1. kind is "method" (id = suggestion.external_contact_id) or "contact"
// (id = candidate.id).
func findSuggestionItemIndex(items []suggestionItem, kind, id string) int {
	for i, item := range items {
		if item["kind"] != kind {
			continue
		}
		switch kind {
		case "method":
			sugg, ok := item["suggestion"].(map[string]interface{})
			if ok && sugg["external_contact_id"] == id {
				return i
			}
		case "contact":
			cand, ok := item["candidate"].(map[string]interface{})
			if ok && cand["id"] == id {
				return i
			}
		}
	}
	return -1
}

// The method-suggestion group rides on top of the confidence-ranked
// candidates, but ONLY on page 1 — the group is small and always returned
// in full above the fold; paginating to page 2 must never repeat it.
// spec: IMP-022[0]
func TestImportSuggestionsAPI_MethodGroupOnlyOnFirstPage(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	t.Parallel()
	router, externalRepo, contactRepo, _, cleanup := setupImportTestRouter()
	t.Cleanup(cleanup)

	ctx := context.Background()
	suffix := uuid.New().String()[:8]

	_, methodExternal := seedLinkedRowWithPendingMethod(t, ctx, contactRepo, externalRepo, suffix)

	// A plain unmatched candidate under the same source, so page 1 has both
	// a method item and a contact item to order against each other.
	candidateName := fmt.Sprintf("Suggestion Candidate %s", suffix)
	candidateExternal, err := externalRepo.Upsert(ctx, repository.UpsertExternalContactRequest{
		Source:      "gcontacts",
		SourceID:    "sugg-candidate-" + suffix,
		DisplayName: &candidateName,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = externalRepo.Delete(ctx, candidateExternal.ID)
	})

	// Page 1: the method group is present and rides above the candidate
	// group. A large limit is used so both seeded items are guaranteed to
	// land on page 1 regardless of how many other gcontacts rows the shared
	// DB accumulates from concurrent tests — the handler always emits ALL
	// method items before ANY candidate item, so comparing OUR two indices
	// still proves the ordering even with unrelated neighbors in the list.
	page1Items, _ := getSuggestions(t, router, "source=gcontacts&limit=10000&page=1")
	methodIdx := findSuggestionItemIndex(page1Items, "method", methodExternal.ID.String())
	require.GreaterOrEqual(t, methodIdx, 0, "seeded method suggestion must surface on page 1")
	candidateIdx := findSuggestionItemIndex(page1Items, "contact", candidateExternal.ID.String())
	require.GreaterOrEqual(t, candidateIdx, 0, "seeded candidate must surface on page 1")
	assert.Less(t, methodIdx, candidateIdx, "method group must ride on top of the candidate group")

	// Page 2: the method group must NOT repeat, even though the same source
	// filter is in effect.
	page2Items, _ := getSuggestions(t, router, "source=gcontacts&limit=10000&page=2")
	assert.Equal(t, -1, findSuggestionItemIndex(page2Items, "method", methodExternal.ID.String()),
		"method group must not repeat past page 1")
}

// Contact candidates are confidence-ranked (shared sort with
// /imports/candidates) and paginate over the candidate group only, with no
// overlap between pages. Asserted through the /imports/suggestions endpoint
// itself, not the shared sort helper.
// spec: IMP-022[1]
func TestImportSuggestionsAPI_CandidatesRankedAndPaginated(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	t.Parallel()
	router, externalRepo, contactRepo, contactMethodRepo, cleanup := setupImportTestRouter()
	t.Cleanup(cleanup)

	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	source := "test-rank-" + suffix
	strPtr := func(s string) *string { return &s }

	// High confidence: shared name AND email (method overlap).
	highName := fmt.Sprintf("Rankcand High %s", suffix)
	highEmail := fmt.Sprintf("rank-high-%s@example.com", suffix)
	highContact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{FullName: highName})
	require.NoError(t, err)
	t.Cleanup(func() { _ = contactRepo.HardDeleteContact(ctx, highContact.ID) })
	_, err = contactMethodRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
		ContactID: highContact.ID, Type: "email", Value: highEmail,
	})
	require.NoError(t, err)
	externalHigh, err := externalRepo.Upsert(ctx, repository.UpsertExternalContactRequest{
		Source: source, SourceID: "rank-high-" + suffix, DisplayName: strPtr(highName),
		Emails: []repository.EmailEntry{{Value: highEmail, Type: "home"}},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = externalRepo.Delete(ctx, externalHigh.ID) })

	// Medium confidence: shared name only.
	medName := fmt.Sprintf("Rankcand Medium %s", suffix)
	medContact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{FullName: medName})
	require.NoError(t, err)
	t.Cleanup(func() { _ = contactRepo.HardDeleteContact(ctx, medContact.ID) })
	externalMed, err := externalRepo.Upsert(ctx, repository.UpsertExternalContactRequest{
		Source: source, SourceID: "rank-med-" + suffix, DisplayName: strPtr(medName),
		Emails: []repository.EmailEntry{{Value: fmt.Sprintf("rank-med-other-%s@example.com", suffix), Type: "home"}},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = externalRepo.Delete(ctx, externalMed.ID) })

	// Low/no confidence: no plausible match at all, sorts after all suggested
	// candidates.
	lowName := fmt.Sprintf("Rankcand Zzzlow %s", suffix)
	externalLow, err := externalRepo.Upsert(ctx, repository.UpsertExternalContactRequest{
		Source: source, SourceID: "rank-low-" + suffix, DisplayName: strPtr(lowName),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = externalRepo.Delete(ctx, externalLow.ID) })

	// limit=1 forces exactly one candidate per page across 3 pages, proving
	// both rank order AND page-boundary correctness (no overlap).
	page1, meta1 := getSuggestions(t, router, "source="+source+"&limit=1&page=1")
	page2, meta2 := getSuggestions(t, router, "source="+source+"&limit=1&page=2")
	page3, meta3 := getSuggestions(t, router, "source="+source+"&limit=1&page=3")

	requireOneCandidate := func(items []suggestionItem) map[string]interface{} {
		require.Len(t, items, 1)
		require.Equal(t, "contact", items[0]["kind"])
		cand, ok := items[0]["candidate"].(map[string]interface{})
		require.True(t, ok)
		return cand
	}
	cand1 := requireOneCandidate(page1)
	cand2 := requireOneCandidate(page2)
	cand3 := requireOneCandidate(page3)

	assert.Equal(t, externalHigh.ID.String(), cand1["id"], "page 1 must be the highest-confidence candidate")
	assert.Equal(t, externalMed.ID.String(), cand2["id"], "page 2 must be the medium-confidence candidate")
	assert.Equal(t, externalLow.ID.String(), cand3["id"], "page 3 must be the unsuggested candidate")

	assert.NotEqual(t, cand1["id"], cand2["id"])
	assert.NotEqual(t, cand2["id"], cand3["id"])
	assert.NotEqual(t, cand1["id"], cand3["id"])

	for pageNum, meta := range map[int]map[string]interface{}{1: meta1, 2: meta2, 3: meta3} {
		require.NotNil(t, meta, "page %d meta", pageNum)
		pagination, ok := meta["pagination"].(map[string]interface{})
		require.True(t, ok, "page %d pagination meta missing", pageNum)
		assert.Equal(t, float64(pageNum), pagination["page"], "page %d", pageNum)
		assert.Equal(t, float64(1), pagination["limit"], "page %d", pageNum)
		assert.Equal(t, float64(3), pagination["total"], "page %d", pageNum)
		assert.Equal(t, float64(3), pagination["pages"], "page %d", pageNum)
	}
}

// Each candidate's allowed_actions is the server-side derivation for its
// source: a link-only source (gmail_correspondence) never offers "import";
// an ordinary source offers all three. Asserted on the literal wire key.
// spec: IMP-022[2]
func TestImportSuggestionsAPI_CandidateAllowedActionsPerSource(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	t.Parallel()
	router, externalRepo, _, _, cleanup := setupImportTestRouter()
	t.Cleanup(cleanup)

	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	strPtr := func(s string) *string { return &s }

	ordinaryName := fmt.Sprintf("Allowed Ordinary %s", suffix)
	ordinary, err := externalRepo.Upsert(ctx, repository.UpsertExternalContactRequest{
		Source: "gcontacts", SourceID: "allowed-ordinary-" + suffix, DisplayName: strPtr(ordinaryName),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = externalRepo.Delete(ctx, ordinary.ID) })

	linkOnlyName := fmt.Sprintf("Allowed LinkOnly %s", suffix)
	linkOnly, err := externalRepo.Upsert(ctx, repository.UpsertExternalContactRequest{
		Source: "gmail_correspondence", SourceID: "allowed-linkonly-" + suffix, DisplayName: strPtr(linkOnlyName),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = externalRepo.Delete(ctx, linkOnly.ID) })

	ordinaryItems, _ := getSuggestions(t, router, "source=gcontacts&limit=10000")
	idx := findSuggestionItemIndex(ordinaryItems, "contact", ordinary.ID.String())
	require.GreaterOrEqual(t, idx, 0, "seeded ordinary-source candidate must surface")
	ordinaryCand := ordinaryItems[idx]["candidate"].(map[string]interface{})
	ordinaryActions, ok := ordinaryCand["allowed_actions"].([]interface{})
	require.True(t, ok, "allowed_actions missing from ordinary candidate wire response")
	assert.ElementsMatch(t, []interface{}{"import", "link", "ignore"}, ordinaryActions,
		"ordinary source must allow import, link, and ignore")

	linkOnlyItems, _ := getSuggestions(t, router, "source=gmail_correspondence&limit=10000")
	idx = findSuggestionItemIndex(linkOnlyItems, "contact", linkOnly.ID.String())
	require.GreaterOrEqual(t, idx, 0, "seeded link-only-source candidate must surface")
	linkOnlyCand := linkOnlyItems[idx]["candidate"].(map[string]interface{})
	linkOnlyActions, ok := linkOnlyCand["allowed_actions"].([]interface{})
	require.True(t, ok, "allowed_actions missing from link-only candidate wire response")
	assert.ElementsMatch(t, []interface{}{"link", "ignore"}, linkOnlyActions,
		"link-only source must allow only link and ignore")

	assert.NotEqual(t, ordinaryActions, linkOnlyActions, "the two sources must declare pairwise-distinct action sets")
}
