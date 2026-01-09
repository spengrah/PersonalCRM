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
	matches       []repository.ContactMatch
	err           error
	lastName      string
	lastThreshold float64
	lastLimit     int32
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
						{Type: "email_personal", Value: "low@example.com"},
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
						{Type: "email_personal", Value: "jane@example.com"},
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
						{Type: "email_personal", Value: "a@example.com"},
					},
				},
				Similarity: 0.7,
			},
			{
				Contact: repository.Contact{
					ID:       bestID,
					FullName: "Match B",
					Methods: []repository.ContactMethod{
						{Type: "email_personal", Value: "b@example.com"},
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
