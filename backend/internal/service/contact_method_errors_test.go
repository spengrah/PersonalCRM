package service

import (
	"errors"
	"fmt"
	"testing"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
)

// The service owns the translation of repository-level errors into
// service-level ones, so nothing above this layer needs to know a repository or
// database error value. These are pure-function tests: no database, no fixture.
//
// The translation matters precisely because the path that produces it is
// unreachable in an integration test. A correct C6 mirror rejects every trigger
// collision during the fold, so the repository's 23505 classification only ever
// fires when the mirror is wrong. Testing the translation directly is what makes
// it verifiable at all.

func TestClassifyApplyError_ValueConflictBecomesInvalidOperations(t *testing.T) {
	t.Parallel()

	got := classifyApplyError(fmt.Errorf("insert: %w", repository.ErrMethodValueConflict))

	if !errors.Is(got, ErrInvalidOperations) {
		t.Fatalf("a repository value conflict must present as ErrInvalidOperations, got %v", got)
	}
	if errors.Is(got, repository.ErrMethodValueConflict) {
		t.Fatal("the repository error identity must not survive translation; callers above the service would be able to branch on it")
	}
}

func TestClassifyApplyError_UnrelatedErrorsPassThrough(t *testing.T) {
	t.Parallel()

	// Anything that is not a value conflict keeps its identity, so a genuine
	// fault still reaches the handler's 500 branch instead of being reported to
	// the user as an invalid request.
	sentinel := errors.New("connection reset")
	if got := classifyApplyError(sentinel); !errors.Is(got, sentinel) {
		t.Fatalf("unrelated error lost its identity: %v", got)
	}
	if got := classifyApplyError(sentinel); errors.Is(got, ErrInvalidOperations) {
		t.Fatal("an unrelated failure was misreported as an invalid payload")
	}
	if got := classifyApplyError(db.ErrNotFound); !errors.Is(got, db.ErrNotFound) {
		t.Fatalf("db.ErrNotFound lost its identity: %v", got)
	}
}

func TestClassifyApplyError_NilStaysNil(t *testing.T) {
	t.Parallel()

	if got := classifyApplyError(nil); got != nil {
		t.Fatalf("expected nil, got %v", got)
	}
}
