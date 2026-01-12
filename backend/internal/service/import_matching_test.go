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
				CandidateID: externalID.String(),
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
				CandidateID: external1ID.String(),
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
				CandidateID: external2ID.String(),
				Matches:     []repository.ContactMatch{}, // No matches
			},
			{
				CandidateID: external3ID.String(),
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
				CandidateID: external2ID.String(),
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

	// Verify only candidate with name was sent to batch
	assert.Len(t, repo.lastBatchInputs, 1)
	assert.Equal(t, external2ID.String(), repo.lastBatchInputs[0].CandidateID)
}

func TestFindBestMatchesBatch_BelowThreshold(t *testing.T) {
	externalID := uuid.New()

	repo := &fakeContactRepo{
		batchMatches: []repository.BatchContactMatch{
			{
				CandidateID: externalID.String(),
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
				CandidateID: externalID.String(),
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
