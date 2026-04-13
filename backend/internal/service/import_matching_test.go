package service

import (
	"context"
	"testing"

	"personal-crm/backend/internal/matching"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

type fakeContactRepo struct {
	matches         []repository.ContactMatch
	batchMatches    []repository.BatchContactMatch
	err             error
	batchErr        error
	lastName        string
	lastThreshold   float64
	lastLimit       int32
	lastBatchInputs []repository.BatchContactInput
}

func (f *fakeContactRepo) FindSimilarContacts(ctx context.Context, name string, threshold float64, limit int32) ([]repository.ContactMatch, error) {
	f.lastName = name
	f.lastThreshold = threshold
	f.lastLimit = limit
	if f.err != nil {
		return nil, f.err
	}
	return f.matches, nil
}

func (f *fakeContactRepo) FindSimilarContactsBatch(ctx context.Context, inputs []repository.BatchContactInput, threshold float64, limitPerCandidate int32) ([]repository.BatchContactMatch, error) {
	f.lastBatchInputs = inputs
	f.lastThreshold = threshold
	f.lastLimit = limitPerCandidate
	if f.batchErr != nil {
		return nil, f.batchErr
	}
	return f.batchMatches, nil
}

func TestImportMatchServiceFindBestMatch_NoName(t *testing.T) {
	svc := NewImportMatchService(&fakeContactRepo{})
	external := &repository.ExternalContact{}

	match, err := svc.FindBestMatch(context.Background(), external)
	assert.NoError(t, err)
	assert.Nil(t, match)
}

func TestImportMatchServiceFindBestMatch_BelowThreshold(t *testing.T) {
	contactID := uuid.New()
	repo := &fakeContactRepo{
		matches: []repository.ContactMatch{
			{
				Contact: repository.Contact{
					ID:       contactID,
					FullName: "Low Score",
					Methods: []repository.ContactMethod{
						{Type: "email", Value: "low@example.com"},
					},
				},
				Similarity: 0.4,
			},
		},
	}
	svc := NewImportMatchService(repo)
	external := &repository.ExternalContact{
		DisplayName: stringPtr("Low Score"),
		Emails:      []repository.EmailEntry{{Value: "nope@example.com"}},
	}

	match, err := svc.FindBestMatch(context.Background(), external)
	assert.NoError(t, err)
	assert.Nil(t, match)
}

func TestImportMatchServiceFindBestMatch_SingleMatch(t *testing.T) {
	contactID := uuid.New()
	repo := &fakeContactRepo{
		matches: []repository.ContactMatch{
			{
				Contact: repository.Contact{
					ID:       contactID,
					FullName: "Jane Doe",
					Methods: []repository.ContactMethod{
						{Type: "email", Value: "jane@example.com"},
					},
				},
				Similarity: 0.9,
			},
		},
	}
	svc := NewImportMatchService(repo)
	external := &repository.ExternalContact{
		DisplayName: stringPtr("Jane Doe"),
		Emails:      []repository.EmailEntry{{Value: "jane@example.com"}},
	}

	match, err := svc.FindBestMatch(context.Background(), external)
	assert.NoError(t, err)
	if assert.NotNil(t, match) {
		assert.Equal(t, contactID.String(), match.ContactID)
		assert.Equal(t, "Jane Doe", match.ContactName)
		assert.True(t, match.Confidence >= matching.ImportConfig.ConfidenceThreshold)
	}
	assert.Equal(t, matching.ImportConfig.MinSimilarityThreshold, repo.lastThreshold)
}

func TestImportMatchServiceFindBestMatch_PrefersBestScore(t *testing.T) {
	bestID := uuid.New()
	repo := &fakeContactRepo{
		matches: []repository.ContactMatch{
			{
				Contact: repository.Contact{
					ID:       uuid.New(),
					FullName: "Match A",
					Methods: []repository.ContactMethod{
						{Type: "email", Value: "a@example.com"},
					},
				},
				Similarity: 0.7,
			},
			{
				Contact: repository.Contact{
					ID:       bestID,
					FullName: "Match B",
					Methods: []repository.ContactMethod{
						{Type: "email", Value: "b@example.com"},
					},
				},
				Similarity: 0.9,
			},
		},
	}
	svc := NewImportMatchService(repo)
	external := &repository.ExternalContact{
		DisplayName: stringPtr("Match B"),
		Emails:      []repository.EmailEntry{{Value: "b@example.com"}},
	}

	match, err := svc.FindBestMatch(context.Background(), external)
	assert.NoError(t, err)
	if assert.NotNil(t, match) {
		assert.Equal(t, bestID.String(), match.ContactID)
	}
}

func TestImportMatchServiceFindBestMatch_Error(t *testing.T) {
	repo := &fakeContactRepo{
		err: assert.AnError,
	}
	svc := NewImportMatchService(repo)
	external := &repository.ExternalContact{
		DisplayName: stringPtr("Jane Doe"),
	}

	match, err := svc.FindBestMatch(context.Background(), external)
	assert.Error(t, err)
	assert.Nil(t, match)
}

func stringPtr(s string) *string {
	return &s
}

// --- FindBestMatch (singular) tests for Telegram handle behavior ---

// TestFindBestMatch_ExactHandleBonusBaseline: handle-only Telegram candidate
// whose normalized form equals the contact's strict-equality form. Bonus
// pushes a 0.82-similarity match above the 0.5 threshold.
func TestFindBestMatch_ExactHandleBonusBaseline(t *testing.T) {
	contactID := uuid.New()
	repo := &fakeContactRepo{
		matches: []repository.ContactMatch{
			{
				Contact: repository.Contact{
					ID:       contactID,
					FullName: "Alice Smith",
				},
				Similarity: 0.82,
			},
		},
	}
	svc := NewImportMatchService(repo)
	external := &repository.ExternalContact{
		ID:       uuid.New(),
		Source:   "telegram",
		Metadata: map[string]any{"username": "@alicesmith"},
	}

	match, err := svc.FindBestMatch(context.Background(), external)
	assert.NoError(t, err)
	if assert.NotNil(t, match) {
		assert.Equal(t, contactID.String(), match.ContactID)
		// 0.82 * 0.6 + 0.4 bonus = 0.892
		assert.InDelta(t, 0.892, match.Confidence, 0.001)
	}
	// The search term passed to the repo should be the normalized handle.
	assert.Equal(t, "alicesmith", repo.lastName)
}

// TestFindBestMatch_BelowMinLengthHandle: handle is 3 chars → no search
// term generated, no suggestion.
func TestFindBestMatch_BelowMinLengthHandle(t *testing.T) {
	repo := &fakeContactRepo{}
	svc := NewImportMatchService(repo)
	external := &repository.ExternalContact{
		ID:       uuid.New(),
		Source:   "telegram",
		Metadata: map[string]any{"username": "@bob"},
	}

	match, err := svc.FindBestMatch(context.Background(), external)
	assert.NoError(t, err)
	assert.Nil(t, match)
	assert.Equal(t, "", repo.lastName) // no repo call
}

// TestFindBestMatch_CollisionSuppressionUsername: ambiguous username-derived
// top match where top-1 − runner-up < 0.15 → suggestion dropped.
func TestFindBestMatch_CollisionSuppressionUsername(t *testing.T) {
	repo := &fakeContactRepo{
		matches: []repository.ContactMatch{
			{
				Contact:    repository.Contact{ID: uuid.New(), FullName: "Alex Johnson"},
				Similarity: 0.90,
			},
			{
				Contact:    repository.Contact{ID: uuid.New(), FullName: "Alexander Chen"},
				Similarity: 0.88,
			},
		},
	}
	svc := NewImportMatchService(repo)
	external := &repository.ExternalContact{
		ID:       uuid.New(),
		Source:   "telegram",
		Metadata: map[string]any{"username": "@alexey"},
	}

	match, err := svc.FindBestMatch(context.Background(), external)
	assert.NoError(t, err)
	assert.Nil(t, match, "collision gap should drop ambiguous username-derived top match")
}

// TestFindBestMatch_NonTelegramSourceNoUsername: non-Telegram source never
// produces a username term, even with metadata.username present.
func TestFindBestMatch_NonTelegramSourceNoUsername(t *testing.T) {
	repo := &fakeContactRepo{}
	svc := NewImportMatchService(repo)
	external := &repository.ExternalContact{
		ID:       uuid.New(),
		Source:   "google",
		Metadata: map[string]any{"username": "alicesmith"},
	}

	match, err := svc.FindBestMatch(context.Background(), external)
	assert.NoError(t, err)
	assert.Nil(t, match)
	assert.Equal(t, "", repo.lastName)
}

// TestFindBestMatch_UsernameBonusBeatsDisplay: display + username terms
// resolve to different contacts; username's exact-handle bonus wins.
func TestFindBestMatch_UsernameBonusBeatsDisplay(t *testing.T) {
	aliceID := uuid.New()
	bobID := uuid.New()
	callCount := 0
	repo := &fakeContactRepoMultiCall{
		responses: [][]repository.ContactMatch{
			// Call 1: display term "Bob J" → Bob Johnson sim 1.0
			{{Contact: repository.Contact{ID: bobID, FullName: "Bob Johnson"}, Similarity: 1.0}},
			// Call 2: username term "alicesmith" → Alice Smith sim 0.82
			{{Contact: repository.Contact{ID: aliceID, FullName: "Alice Smith"}, Similarity: 0.82}},
		},
		callCount: &callCount,
	}
	svc := NewImportMatchService(repo)
	external := &repository.ExternalContact{
		ID:          uuid.New(),
		Source:      "telegram",
		DisplayName: stringPtr("Bob J"),
		Metadata:    map[string]any{"username": "@alicesmith"},
	}

	match, err := svc.FindBestMatch(context.Background(), external)
	assert.NoError(t, err)
	if assert.NotNil(t, match) {
		// Bob: 1.0 * 0.6 = 0.6 (display, no bonus)
		// Alice: 0.82 * 0.6 + 0.4 bonus = 0.892 (username, bonus fires)
		assert.Equal(t, aliceID.String(), match.ContactID)
		assert.InDelta(t, 0.892, match.Confidence, 0.001)
	}
	assert.Equal(t, 2, callCount, "both display and username terms should hit the repo")
}

// fakeContactRepoMultiCall returns a different matches slice per call to
// FindSimilarContacts, matching the order of search terms issued.
type fakeContactRepoMultiCall struct {
	responses [][]repository.ContactMatch
	callCount *int
}

func (f *fakeContactRepoMultiCall) FindSimilarContacts(_ context.Context, _ string, _ float64, _ int32) ([]repository.ContactMatch, error) {
	idx := *f.callCount
	*f.callCount++
	if idx >= len(f.responses) {
		return nil, nil
	}
	return f.responses[idx], nil
}

func (f *fakeContactRepoMultiCall) FindSimilarContactsBatch(_ context.Context, _ []repository.BatchContactInput, _ float64, _ int32) ([]repository.BatchContactMatch, error) {
	return nil, nil
}

// Tests for FindBestMatchesBatch

func TestFindBestMatchesBatch_EmptyInput(t *testing.T) {
	repo := &fakeContactRepo{}
	svc := NewImportMatchService(repo)

	results, err := svc.FindBestMatchesBatch(context.Background(), []*repository.ExternalContact{})
	assert.NoError(t, err)
	assert.Empty(t, results)
}

func TestFindBestMatchesBatch_AllEmptyNames(t *testing.T) {
	repo := &fakeContactRepo{}
	svc := NewImportMatchService(repo)

	externals := []*repository.ExternalContact{
		{ID: uuid.New()}, // No name
		{ID: uuid.New()}, // No name
	}

	results, err := svc.FindBestMatchesBatch(context.Background(), externals)
	assert.NoError(t, err)
	assert.Len(t, results, 2)
	assert.Nil(t, results[0])
	assert.Nil(t, results[1])
}

func TestFindBestMatchesBatch_SingleCandidate(t *testing.T) {
	contactID := uuid.New()
	externalID := uuid.New()

	repo := &fakeContactRepo{
		batchMatches: []repository.BatchContactMatch{
			{
				CandidateID: externalID.String() + "|0",
				Matches: []repository.ContactMatch{
					{
						Contact: repository.Contact{
							ID:       contactID,
							FullName: "Jane Doe",
							Methods: []repository.ContactMethod{
								{Type: "email", Value: "jane@example.com"},
							},
						},
						Similarity: 0.9,
					},
				},
			},
		},
	}
	svc := NewImportMatchService(repo)

	externals := []*repository.ExternalContact{
		{
			ID:          externalID,
			DisplayName: stringPtr("Jane Doe"),
			Emails:      []repository.EmailEntry{{Value: "jane@example.com"}},
		},
	}

	results, err := svc.FindBestMatchesBatch(context.Background(), externals)
	assert.NoError(t, err)
	assert.Len(t, results, 1)
	if assert.NotNil(t, results[0]) {
		assert.Equal(t, contactID.String(), results[0].ContactID)
		assert.Equal(t, "Jane Doe", results[0].ContactName)
		assert.True(t, results[0].Confidence >= matching.ImportConfig.ConfidenceThreshold)
	}
	assert.Equal(t, matching.ImportConfig.MinSimilarityThreshold, repo.lastThreshold)
}

func TestFindBestMatchesBatch_MultipleCandidates(t *testing.T) {
	contact1ID := uuid.New()
	contact2ID := uuid.New()
	external1ID := uuid.New()
	external2ID := uuid.New()
	external3ID := uuid.New()

	repo := &fakeContactRepo{
		batchMatches: []repository.BatchContactMatch{
			{
				CandidateID: external1ID.String() + "|0",
				Matches: []repository.ContactMatch{
					{
						Contact: repository.Contact{
							ID:       contact1ID,
							FullName: "Jane Doe",
							Methods: []repository.ContactMethod{
								{Type: "email", Value: "jane@example.com"},
							},
						},
						Similarity: 0.9,
					},
				},
			},
			{
				CandidateID: external2ID.String() + "|0",
				Matches:     []repository.ContactMatch{}, // No matches
			},
			{
				CandidateID: external3ID.String() + "|0",
				Matches: []repository.ContactMatch{
					{
						Contact: repository.Contact{
							ID:       contact2ID,
							FullName: "John Smith",
							Methods: []repository.ContactMethod{
								{Type: "phone", Value: "+15551234567"},
							},
						},
						Similarity: 0.85,
					},
				},
			},
		},
	}
	svc := NewImportMatchService(repo)

	externals := []*repository.ExternalContact{
		{
			ID:          external1ID,
			DisplayName: stringPtr("Jane Doe"),
			Emails:      []repository.EmailEntry{{Value: "jane@example.com"}},
		},
		{
			ID:          external2ID,
			DisplayName: stringPtr("Unknown Person"),
		},
		{
			ID:          external3ID,
			DisplayName: stringPtr("John Smith"),
			Phones:      []repository.PhoneEntry{{Value: "+15551234567"}},
		},
	}

	results, err := svc.FindBestMatchesBatch(context.Background(), externals)
	assert.NoError(t, err)
	assert.Len(t, results, 3)

	// First candidate should have a match
	if assert.NotNil(t, results[0]) {
		assert.Equal(t, contact1ID.String(), results[0].ContactID)
	}

	// Second candidate has no matches
	assert.Nil(t, results[1])

	// Third candidate should have a match
	if assert.NotNil(t, results[2]) {
		assert.Equal(t, contact2ID.String(), results[2].ContactID)
	}
}

func TestFindBestMatchesBatch_SomeEmptyNames(t *testing.T) {
	contactID := uuid.New()
	external1ID := uuid.New()
	external2ID := uuid.New()
	external3ID := uuid.New()

	repo := &fakeContactRepo{
		batchMatches: []repository.BatchContactMatch{
			{
				CandidateID: external2ID.String() + "|0",
				Matches: []repository.ContactMatch{
					{
						Contact: repository.Contact{
							ID:       contactID,
							FullName: "Jane Doe",
							Methods:  []repository.ContactMethod{},
						},
						Similarity: 0.9,
					},
				},
			},
		},
	}
	svc := NewImportMatchService(repo)

	externals := []*repository.ExternalContact{
		{ID: external1ID}, // No name - should be skipped
		{
			ID:          external2ID,
			DisplayName: stringPtr("Jane Doe"),
		},
		{ID: external3ID}, // No name - should be skipped
	}

	results, err := svc.FindBestMatchesBatch(context.Background(), externals)
	assert.NoError(t, err)
	assert.Len(t, results, 3)

	// First candidate has no name, so no match
	assert.Nil(t, results[0])

	// Second candidate should have a match
	if assert.NotNil(t, results[1]) {
		assert.Equal(t, contactID.String(), results[1].ContactID)
	}

	// Third candidate has no name, so no match
	assert.Nil(t, results[2])

	// Verify only candidate with name was sent to batch (composite ID = uuid|0)
	assert.Len(t, repo.lastBatchInputs, 1)
	assert.Equal(t, external2ID.String()+"|0", repo.lastBatchInputs[0].CandidateID)
}

func TestFindBestMatchesBatch_BelowThreshold(t *testing.T) {
	externalID := uuid.New()

	repo := &fakeContactRepo{
		batchMatches: []repository.BatchContactMatch{
			{
				CandidateID: externalID.String() + "|0",
				Matches: []repository.ContactMatch{
					{
						Contact: repository.Contact{
							ID:       uuid.New(),
							FullName: "Low Score",
							Methods: []repository.ContactMethod{
								{Type: "email", Value: "low@example.com"},
							},
						},
						Similarity: 0.4, // Low similarity, no method match → below 0.5 threshold
					},
				},
			},
		},
	}
	svc := NewImportMatchService(repo)

	externals := []*repository.ExternalContact{
		{
			ID:          externalID,
			DisplayName: stringPtr("Low Score"),
			Emails:      []repository.EmailEntry{{Value: "different@example.com"}},
		},
	}

	results, err := svc.FindBestMatchesBatch(context.Background(), externals)
	assert.NoError(t, err)
	assert.Len(t, results, 1)
	assert.Nil(t, results[0]) // Below confidence threshold
}

func TestFindBestMatchesBatch_PrefersBestScore(t *testing.T) {
	bestID := uuid.New()
	externalID := uuid.New()

	repo := &fakeContactRepo{
		batchMatches: []repository.BatchContactMatch{
			{
				CandidateID: externalID.String() + "|0",
				Matches: []repository.ContactMatch{
					{
						Contact: repository.Contact{
							ID:       uuid.New(),
							FullName: "Match A",
							Methods: []repository.ContactMethod{
								{Type: "email", Value: "a@example.com"},
							},
						},
						Similarity: 0.7,
					},
					{
						Contact: repository.Contact{
							ID:       bestID,
							FullName: "Match B",
							Methods: []repository.ContactMethod{
								{Type: "email", Value: "b@example.com"},
							},
						},
						Similarity: 0.9,
					},
				},
			},
		},
	}
	svc := NewImportMatchService(repo)

	externals := []*repository.ExternalContact{
		{
			ID:          externalID,
			DisplayName: stringPtr("Match B"),
			Emails:      []repository.EmailEntry{{Value: "b@example.com"}},
		},
	}

	results, err := svc.FindBestMatchesBatch(context.Background(), externals)
	assert.NoError(t, err)
	assert.Len(t, results, 1)
	if assert.NotNil(t, results[0]) {
		assert.Equal(t, bestID.String(), results[0].ContactID)
	}
}

func TestFindBestMatchesBatch_Error(t *testing.T) {
	repo := &fakeContactRepo{
		batchErr: assert.AnError,
	}
	svc := NewImportMatchService(repo)

	externals := []*repository.ExternalContact{
		{
			ID:          uuid.New(),
			DisplayName: stringPtr("Jane Doe"),
		},
	}

	results, err := svc.FindBestMatchesBatch(context.Background(), externals)
	assert.Error(t, err)
	assert.Nil(t, results)
}

func TestFindBestMatchesBatch_UnexpectedCandidateID(t *testing.T) {
	// This test verifies that the service gracefully handles the case where
	// the repository returns a batch match with a candidate_id that was not
	// in the original input. This is a safety check that should be handled
	// without panicking.
	externalID := uuid.New()
	contactID := uuid.New()

	repo := &fakeContactRepo{
		batchMatches: []repository.BatchContactMatch{
			{
				// Return a match for an unexpected candidate ID
				CandidateID: "unexpected-candidate-id-not-in-input",
				Matches: []repository.ContactMatch{
					{
						Contact: repository.Contact{
							ID:       contactID,
							FullName: "Jane Doe",
							Methods:  []repository.ContactMethod{},
						},
						Similarity: 0.9,
					},
				},
			},
		},
	}
	svc := NewImportMatchService(repo)

	externals := []*repository.ExternalContact{
		{
			ID:          externalID,
			DisplayName: stringPtr("Jane Doe"),
		},
	}

	// Should not panic and should return nil for the external contact
	// since the batch result didn't have a matching candidate ID
	results, err := svc.FindBestMatchesBatch(context.Background(), externals)
	assert.NoError(t, err)
	assert.Len(t, results, 1)
	assert.Nil(t, results[0]) // No match because candidate ID didn't match
}

// --- Telegram username-vs-name tests (issue #272) ---

// telegramExternalWithUsername builds a Telegram external contact with only
// a username (no display/first/last name) — the peer-with-only-a-handle case.
func telegramExternalWithUsername(id uuid.UUID, username string) *repository.ExternalContact {
	return &repository.ExternalContact{
		ID:       id,
		Source:   "telegram",
		Metadata: map[string]any{"username": username},
	}
}

// TestFindBestMatchesBatch_ExactHandleBonusBaseline (1a): bonus pushes a
// 0.82-similarity match from below the 0.5 threshold to above it.
func TestFindBestMatchesBatch_ExactHandleBonusBaseline(t *testing.T) {
	contactID := uuid.New()
	externalID := uuid.New()

	repo := &fakeContactRepo{
		batchMatches: []repository.BatchContactMatch{
			{
				CandidateID: externalID.String() + "|0",
				Matches: []repository.ContactMatch{
					{
						Contact: repository.Contact{
							ID:       contactID,
							FullName: "Alice Smith",
						},
						Similarity: 0.82,
					},
				},
			},
		},
	}
	svc := NewImportMatchService(repo)

	externals := []*repository.ExternalContact{
		telegramExternalWithUsername(externalID, "@alicesmith"),
	}

	results, err := svc.FindBestMatchesBatch(context.Background(), externals)
	assert.NoError(t, err)
	if assert.NotNil(t, results[0]) {
		assert.Equal(t, contactID.String(), results[0].ContactID)
		assert.Equal(t, "Alice Smith", results[0].ContactName)
		// 0.82*0.6 + 0.4 bonus = 0.892
		assert.InDelta(t, 0.892, results[0].Confidence, 0.001)
	}

	// Verify search term was the normalized handle
	assert.Len(t, repo.lastBatchInputs, 1)
	assert.Equal(t, "alicesmith", repo.lastBatchInputs[0].CandidateName)
}

// TestFindBestMatchesBatch_ExactHandleBonusCap (1b): bonus is capped at 1.0.
func TestFindBestMatchesBatch_ExactHandleBonusCap(t *testing.T) {
	contactID := uuid.New()
	externalID := uuid.New()

	repo := &fakeContactRepo{
		batchMatches: []repository.BatchContactMatch{
			{
				CandidateID: externalID.String() + "|0",
				Matches: []repository.ContactMatch{
					{
						Contact: repository.Contact{
							ID:       contactID,
							FullName: "Alice Smith",
							Methods: []repository.ContactMethod{
								{Type: "email", Value: "alice@example.com"},
							},
						},
						Similarity: 1.0,
					},
				},
			},
		},
	}
	svc := NewImportMatchService(repo)

	ext := telegramExternalWithUsername(externalID, "@alicesmith")
	ext.Emails = []repository.EmailEntry{{Value: "alice@example.com"}}

	results, err := svc.FindBestMatchesBatch(context.Background(), []*repository.ExternalContact{ext})
	assert.NoError(t, err)
	if assert.NotNil(t, results[0]) {
		// Pre-bonus = 1.0*0.6 + 1.0*0.4 = 1.0; +0.4 bonus → 1.4 → cap 1.0
		assert.Equal(t, 1.0, results[0].Confidence)
	}
}

// TestFindBestMatchesBatch_ExactHandleDiacritic (1c): strict-equality
// normalization folds diacritics and punctuation on both sides.
func TestFindBestMatchesBatch_ExactHandleDiacritic(t *testing.T) {
	contactID := uuid.New()
	externalID := uuid.New()

	repo := &fakeContactRepo{
		batchMatches: []repository.BatchContactMatch{
			{
				CandidateID: externalID.String() + "|0",
				Matches: []repository.ContactMatch{
					{
						Contact: repository.Contact{
							ID:       contactID,
							FullName: "José Smith",
						},
						Similarity: 0.78,
					},
				},
			},
		},
	}
	svc := NewImportMatchService(repo)

	externals := []*repository.ExternalContact{
		telegramExternalWithUsername(externalID, "@jose_smith"),
	}

	results, err := svc.FindBestMatchesBatch(context.Background(), externals)
	assert.NoError(t, err)
	if assert.NotNil(t, results[0]) {
		assert.Equal(t, contactID.String(), results[0].ContactID)
		// 0.78*0.6 + 0.4 = 0.868
		assert.InDelta(t, 0.868, results[0].Confidence, 0.001)
	}
	assert.Equal(t, "jose smith", repo.lastBatchInputs[0].CandidateName)
}

// TestFindBestMatchesBatch_PartialHandleNoBonus (1d): @alice vs "Alice Smith"
// strict-eq forms differ (alice vs. alicesmith), so no bonus — score stays
// below threshold and is filtered.
func TestFindBestMatchesBatch_PartialHandleNoBonus(t *testing.T) {
	externalID := uuid.New()

	repo := &fakeContactRepo{
		batchMatches: []repository.BatchContactMatch{
			{
				CandidateID: externalID.String() + "|0",
				Matches: []repository.ContactMatch{
					{
						Contact: repository.Contact{
							ID:       uuid.New(),
							FullName: "Alice Smith",
						},
						Similarity: 0.60,
					},
				},
			},
		},
	}
	svc := NewImportMatchService(repo)

	externals := []*repository.ExternalContact{
		telegramExternalWithUsername(externalID, "@alice"),
	}

	results, err := svc.FindBestMatchesBatch(context.Background(), externals)
	assert.NoError(t, err)
	// 0.60 * 0.6 = 0.36, no bonus, below 0.5 threshold
	assert.Nil(t, results[0])
}

// TestFindBestMatchesBatch_DisplayNameNoBonus (5): display-name matches keep
// the current behavior (no bonus, standard score).
func TestFindBestMatchesBatch_DisplayNameNoBonus(t *testing.T) {
	contactID := uuid.New()
	externalID := uuid.New()

	repo := &fakeContactRepo{
		batchMatches: []repository.BatchContactMatch{
			{
				CandidateID: externalID.String() + "|0",
				Matches: []repository.ContactMatch{
					{
						Contact: repository.Contact{
							ID:       contactID,
							FullName: "Alice Smith",
						},
						Similarity: 0.95,
					},
				},
			},
		},
	}
	svc := NewImportMatchService(repo)

	externals := []*repository.ExternalContact{
		{
			ID:          externalID,
			Source:      "telegram",
			DisplayName: stringPtr("Alice Smith"),
		},
	}

	results, err := svc.FindBestMatchesBatch(context.Background(), externals)
	assert.NoError(t, err)
	if assert.NotNil(t, results[0]) {
		// 0.95 * 0.6 = 0.57 (no bonus because display-name term, not username)
		assert.InDelta(t, 0.57, results[0].Confidence, 0.001)
	}
}

// TestFindBestMatchesBatch_TwoTermsUsernameWins (6): display and username
// terms are both searched; username's exact-handle bonus wins.
func TestFindBestMatchesBatch_TwoTermsUsernameWins(t *testing.T) {
	aliceSmithID := uuid.New()
	aliceJohnsonID := uuid.New()
	externalID := uuid.New()

	repo := &fakeContactRepo{
		batchMatches: []repository.BatchContactMatch{
			{
				CandidateID: externalID.String() + "|0", // display term "Alice J"
				Matches: []repository.ContactMatch{
					{
						Contact:    repository.Contact{ID: aliceJohnsonID, FullName: "Alice Johnson"},
						Similarity: 0.92,
					},
				},
			},
			{
				CandidateID: externalID.String() + "|1", // username term "alicesmith"
				Matches: []repository.ContactMatch{
					{
						Contact:    repository.Contact{ID: aliceSmithID, FullName: "Alice Smith"},
						Similarity: 0.82,
					},
				},
			},
		},
	}
	svc := NewImportMatchService(repo)

	externals := []*repository.ExternalContact{
		{
			ID:          externalID,
			Source:      "telegram",
			DisplayName: stringPtr("Alice J"),
			Metadata:    map[string]any{"username": "@alicesmith"},
		},
	}

	results, err := svc.FindBestMatchesBatch(context.Background(), externals)
	assert.NoError(t, err)
	if assert.NotNil(t, results[0]) {
		// Alice Smith: 0.82*0.6 + 0.4 = 0.892 (username + bonus)
		// Alice Johnson: 0.92*0.6 = 0.552 (display, no bonus)
		// Gap 0.34 > 0.15 → Alice Smith wins
		assert.Equal(t, aliceSmithID.String(), results[0].ContactID)
		assert.InDelta(t, 0.892, results[0].Confidence, 0.001)
	}
}

// TestFindBestMatchesBatch_PerContactDedupe (7): when the same contact scores
// for both terms, only the best per-contact score counts — no spurious
// runner-up that triggers the collision-gap rule.
func TestFindBestMatchesBatch_PerContactDedupe(t *testing.T) {
	contactID := uuid.New()
	externalID := uuid.New()

	repo := &fakeContactRepo{
		batchMatches: []repository.BatchContactMatch{
			{
				CandidateID: externalID.String() + "|0", // display term
				Matches: []repository.ContactMatch{
					{
						Contact:    repository.Contact{ID: contactID, FullName: "Alice Smith"},
						Similarity: 0.95,
					},
				},
			},
			{
				CandidateID: externalID.String() + "|1", // username term
				Matches: []repository.ContactMatch{
					{
						Contact:    repository.Contact{ID: contactID, FullName: "Alice Smith"},
						Similarity: 0.82,
					},
				},
			},
		},
	}
	svc := NewImportMatchService(repo)

	externals := []*repository.ExternalContact{
		{
			ID:          externalID,
			Source:      "telegram",
			DisplayName: stringPtr("Alice Smith"),
			Metadata:    map[string]any{"username": "@alicesmith"},
		},
	}

	results, err := svc.FindBestMatchesBatch(context.Background(), externals)
	assert.NoError(t, err)
	if assert.NotNil(t, results[0]) {
		assert.Equal(t, contactID.String(), results[0].ContactID)
		// Best score for this contact = 0.892 (username+bonus beats display 0.57)
		assert.InDelta(t, 0.892, results[0].Confidence, 0.001)
	}
}

// TestFindBestMatchesBatch_BelowMinLengthUsername (8): @bob normalizes to 3
// chars → dropped. No display name either → no terms → no suggestion.
func TestFindBestMatchesBatch_BelowMinLengthUsername(t *testing.T) {
	externalID := uuid.New()
	repo := &fakeContactRepo{}
	svc := NewImportMatchService(repo)

	externals := []*repository.ExternalContact{
		telegramExternalWithUsername(externalID, "@bob"),
	}

	results, err := svc.FindBestMatchesBatch(context.Background(), externals)
	assert.NoError(t, err)
	assert.Nil(t, results[0])
	assert.Empty(t, repo.lastBatchInputs) // no inputs sent to repo
}

// TestFindBestMatchesBatch_NumericOnlyHandle (9): @12345 strips trailing
// digits → empty → dropped.
func TestFindBestMatchesBatch_NumericOnlyHandle(t *testing.T) {
	externalID := uuid.New()
	repo := &fakeContactRepo{}
	svc := NewImportMatchService(repo)

	externals := []*repository.ExternalContact{
		telegramExternalWithUsername(externalID, "@12345"),
	}

	results, err := svc.FindBestMatchesBatch(context.Background(), externals)
	assert.NoError(t, err)
	assert.Nil(t, results[0])
	assert.Empty(t, repo.lastBatchInputs)
}

// TestFindBestMatchesBatch_CollisionSuppressionUsername (10): ambiguous
// username-derived top match — gap < 0.15 → suggestion dropped.
func TestFindBestMatchesBatch_CollisionSuppressionUsername(t *testing.T) {
	externalID := uuid.New()

	repo := &fakeContactRepo{
		batchMatches: []repository.BatchContactMatch{
			{
				CandidateID: externalID.String() + "|0",
				Matches: []repository.ContactMatch{
					{
						Contact:    repository.Contact{ID: uuid.New(), FullName: "Alex Johnson"},
						Similarity: 0.90,
					},
					{
						Contact:    repository.Contact{ID: uuid.New(), FullName: "Alexander Chen"},
						Similarity: 0.88,
					},
				},
			},
		},
	}
	svc := NewImportMatchService(repo)

	externals := []*repository.ExternalContact{
		telegramExternalWithUsername(externalID, "@alexey"),
	}

	results, err := svc.FindBestMatchesBatch(context.Background(), externals)
	assert.NoError(t, err)
	// Alex Johnson: 0.90*0.6 = 0.54 (no bonus, strict "alexey" vs "alexjohnson")
	// Alexander Chen: 0.88*0.6 = 0.528 (no bonus)
	// Gap 0.012 < 0.15, top-1 from username → dropped
	assert.Nil(t, results[0])
}

// TestFindBestMatchesBatch_NoCollisionSuppressionOnDisplayName (10 sub-case):
// same close scores via display-name term → NOT dropped, top-1 wins.
func TestFindBestMatchesBatch_NoCollisionSuppressionOnDisplayName(t *testing.T) {
	winnerID := uuid.New()
	externalID := uuid.New()

	repo := &fakeContactRepo{
		batchMatches: []repository.BatchContactMatch{
			{
				CandidateID: externalID.String() + "|0",
				Matches: []repository.ContactMatch{
					{
						Contact:    repository.Contact{ID: winnerID, FullName: "Alex Johnson"},
						Similarity: 0.90,
					},
					{
						Contact:    repository.Contact{ID: uuid.New(), FullName: "Alexander Chen"},
						Similarity: 0.88,
					},
				},
			},
		},
	}
	svc := NewImportMatchService(repo)

	externals := []*repository.ExternalContact{
		{
			ID:          externalID,
			Source:      "telegram",
			DisplayName: stringPtr("Alex"),
		},
	}

	results, err := svc.FindBestMatchesBatch(context.Background(), externals)
	assert.NoError(t, err)
	if assert.NotNil(t, results[0]) {
		// Display-term path: top-1 wins regardless of gap
		assert.Equal(t, winnerID.String(), results[0].ContactID)
	}
}

// TestFindBestMatchesBatch_NonTelegramSourceNoUsername (11): non-Telegram
// sources never produce a username term, even with metadata.username.
func TestFindBestMatchesBatch_NonTelegramSourceNoUsername(t *testing.T) {
	externalID := uuid.New()
	repo := &fakeContactRepo{}
	svc := NewImportMatchService(repo)

	externals := []*repository.ExternalContact{
		{
			ID:       externalID,
			Source:   "google",
			Metadata: map[string]any{"username": "alicesmith"},
		},
	}

	results, err := svc.FindBestMatchesBatch(context.Background(), externals)
	assert.NoError(t, err)
	assert.Nil(t, results[0])
	assert.Empty(t, repo.lastBatchInputs)
}

// TestFindBestMatchesBatch_TelegramMetadataWithoutUsername (12): Telegram
// candidate whose metadata lacks the "username" key produces only the
// primary display term.
func TestFindBestMatchesBatch_TelegramMetadataWithoutUsername(t *testing.T) {
	contactID := uuid.New()
	externalID := uuid.New()

	repo := &fakeContactRepo{
		batchMatches: []repository.BatchContactMatch{
			{
				CandidateID: externalID.String() + "|0",
				Matches: []repository.ContactMatch{
					{
						Contact:    repository.Contact{ID: contactID, FullName: "Alice Smith"},
						Similarity: 0.95,
					},
				},
			},
		},
	}
	svc := NewImportMatchService(repo)

	externals := []*repository.ExternalContact{
		{
			ID:          externalID,
			Source:      "telegram",
			DisplayName: stringPtr("Alice Smith"),
			Metadata:    map[string]any{"foo": "bar"},
		},
	}

	results, err := svc.FindBestMatchesBatch(context.Background(), externals)
	assert.NoError(t, err)
	if assert.NotNil(t, results[0]) {
		assert.Equal(t, contactID.String(), results[0].ContactID)
	}
	// Only one term sent to repo (the display term)
	assert.Len(t, repo.lastBatchInputs, 1)
	assert.Equal(t, "Alice Smith", repo.lastBatchInputs[0].CandidateName)
}
