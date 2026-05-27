package anarlog

import (
	"context"
	"errors"
	"testing"

	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
)

// mockContactMatchFinder lets each test hand-craft the candidate list
// returned by FindSimilarContacts (and record call sites).
type mockContactMatchFinder struct {
	matches []repository.ContactMatch
	err     error
	calls   int
}

func (m *mockContactMatchFinder) FindSimilarContacts(_ context.Context, _ string, _ float64, _ int32) ([]repository.ContactMatch, error) {
	m.calls++
	if m.err != nil {
		return nil, m.err
	}
	return m.matches, nil
}

func contactMatch(id uuid.UUID, name string, similarity float64) repository.ContactMatch {
	return repository.ContactMatch{
		Contact: repository.Contact{
			ID:       id,
			FullName: name,
		},
		Similarity: similarity,
	}
}

// TC-TM1 — no matches in the DB.
func TestMatchTitleToken_NoMatches(t *testing.T) {
	mock := &mockContactMatchFinder{matches: nil}
	m := NewTitleMatcher(mock)
	got, err := m.MatchTitleToken(context.Background(), "Alice")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil match, got %+v", got)
	}
}

// TC-TM2 — single high-confidence match.
func TestMatchTitleToken_SingleHighConfidence(t *testing.T) {
	id := uuid.New()
	mock := &mockContactMatchFinder{matches: []repository.ContactMatch{
		contactMatch(id, "Alice Smith", 0.8),
	}}
	m := NewTitleMatcher(mock)
	got, err := m.MatchTitleToken(context.Background(), "Alice")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got == nil {
		t.Fatalf("expected match, got nil")
	}
	if got.Contact.ID != id {
		t.Errorf("wrong contact: %v", got.Contact.ID)
	}
}

// TC-TM3 — below confidence floor.
func TestMatchTitleToken_BelowConfidenceFloor(t *testing.T) {
	id := uuid.New()
	mock := &mockContactMatchFinder{matches: []repository.ContactMatch{
		contactMatch(id, "Alice", 0.30),
	}}
	m := NewTitleMatcher(mock)
	got, err := m.MatchTitleToken(context.Background(), "Alice")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for below-floor match, got %+v", got)
	}
}

// TC-TM4 — gap too small (0.10 < 0.20).
func TestMatchTitleToken_CollisionGapTooSmall(t *testing.T) {
	mock := &mockContactMatchFinder{matches: []repository.ContactMatch{
		contactMatch(uuid.New(), "Alice One", 0.7),
		contactMatch(uuid.New(), "Alice Two", 0.6),
	}}
	m := NewTitleMatcher(mock)
	got, err := m.MatchTitleToken(context.Background(), "Alice")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for ambiguous match, got %+v", got)
	}
}

// TC-TM5 — gap exactly meets threshold (0.20). Strict-less-or-equal
// means this should still drop.
func TestMatchTitleToken_CollisionGapJustMeets(t *testing.T) {
	mock := &mockContactMatchFinder{matches: []repository.ContactMatch{
		contactMatch(uuid.New(), "Alice One", 0.7),
		contactMatch(uuid.New(), "Alice Two", 0.5),
	}}
	m := NewTitleMatcher(mock)
	got, err := m.MatchTitleToken(context.Background(), "Alice")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != nil {
		t.Errorf("gap of exactly 0.20 should be ambiguous, got %+v", got)
	}
}

// TC-TM6 — gap exceeds threshold (0.21 > 0.20).
func TestMatchTitleToken_CollisionGapExceeds(t *testing.T) {
	topID := uuid.New()
	mock := &mockContactMatchFinder{matches: []repository.ContactMatch{
		contactMatch(topID, "Alice Top", 0.7),
		contactMatch(uuid.New(), "Alice Runner", 0.49),
	}}
	m := NewTitleMatcher(mock)
	got, err := m.MatchTitleToken(context.Background(), "Alice")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got == nil || got.Contact.ID != topID {
		t.Errorf("expected top match, got %+v", got)
	}
}

// TC-TM7 — tie-breaker stable: two same-similarity matches. Even if
// they're above the floor, the gap is 0 so both should drop.
func TestMatchTitleToken_TieBreakerStable(t *testing.T) {
	// Use IDs where idA < idB lexicographically to verify the
	// tie-breaker sort. Even though sort is stable, we still drop
	// because the gap is 0.
	idA := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	idB := uuid.MustParse("00000000-0000-0000-0000-000000000002")
	mock := &mockContactMatchFinder{matches: []repository.ContactMatch{
		// Feed reversed so the sort has to do work.
		contactMatch(idB, "Alice B", 0.7),
		contactMatch(idA, "Alice A", 0.7),
	}}
	m := NewTitleMatcher(mock)
	got, err := m.MatchTitleToken(context.Background(), "Alice")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != nil {
		t.Errorf("equal-similarity ties should be ambiguous, got %+v", got)
	}
}

// TC-TM8 — empty token short-circuits without a DB call.
func TestMatchTitleToken_EmptyToken(t *testing.T) {
	mock := &mockContactMatchFinder{}
	m := NewTitleMatcher(mock)
	got, err := m.MatchTitleToken(context.Background(), "")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != nil {
		t.Errorf("empty token should return nil, got %+v", got)
	}
	if mock.calls != 0 {
		t.Errorf("empty token should not invoke repo, got %d calls", mock.calls)
	}
}

// TestMatchTitleToken_RepoError surfaces DB errors to the caller.
func TestMatchTitleToken_RepoError(t *testing.T) {
	mock := &mockContactMatchFinder{err: errors.New("boom")}
	m := NewTitleMatcher(mock)
	got, err := m.MatchTitleToken(context.Background(), "Alice")
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if got != nil {
		t.Errorf("expected nil match on error, got %+v", got)
	}
}
