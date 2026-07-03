package service

import (
	"context"
	"testing"
	"time"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

// These tests pin the nil semantics preserved by the setter→constructor-arg
// conversion (PR5): the promoted consumer args carry exactly the behavior their
// former setters had. A fully-nil consumer set is the "setter never called"
// posture — the guarded write paths must still return their "not wired" ERRORS
// (not panic), and a HALF-set knowledge pair must fail LOUDLY at construction.

// stubCacheRefresher is a non-nil knowledgeCacheRefresher for the half-set-pair
// construction tests (a real cache would drag in the consumer package).
type stubCacheRefresher struct{}

func (stubCacheRefresher) RefreshTx(context.Context, pgx.Tx, uuid.UUID, string) error { return nil }

// nilContactService builds a ContactService with the fully-nil consumer set.
// The core repos are nil too: every guarded path asserted below returns its
// not-wired error BEFORE touching a repo or the pool.
func nilContactService() *ContactService {
	return NewContactService(&db.Database{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
}

func TestContactService_NilKnowledge_CreateContactErrors(t *testing.T) {
	t.Parallel()
	_, _, err := nilContactService().CreateContact(
		context.Background(), repository.CreateContactRequest{FullName: "guard"}, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "knowledge writer not wired")
}

func TestContactService_NilCadence_PromoteErrors(t *testing.T) {
	t.Parallel()
	_, err := nilContactService().PromoteInteractionToMutualTx(
		context.Background(), nil, uuid.New(), uuid.New(), time.Time{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "cadence updater not wired")
}

func TestContactService_NilCadence_ExtendErrors(t *testing.T) {
	t.Parallel()
	_, err := nilContactService().ExtendInteractionTx(
		context.Background(), nil, uuid.New(), uuid.New(), repository.InteractionDirectionMutual, time.Time{}, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "cadence updater not wired")
}

func TestNewContactService_HalfSetKnowledgePairPanics(t *testing.T) {
	t.Parallel()
	assertSvc := NewAssertService(nil, nil, nil, nil, nil, nil)
	// assertSvc set, cache nil → half-set → panic.
	require.Panics(t, func() {
		NewContactService(&db.Database{}, nil, nil, nil, nil, nil, nil, nil, assertSvc, nil, nil)
	})
	// cache set, assertSvc nil → half-set → panic.
	require.Panics(t, func() {
		NewContactService(&db.Database{}, nil, nil, nil, nil, nil, nil, nil, nil, stubCacheRefresher{}, nil)
	})
}

func TestNewEnrichmentService_HalfSetKnowledgePairPanics(t *testing.T) {
	t.Parallel()
	assertSvc := NewAssertService(nil, nil, nil, nil, nil, nil)
	require.Panics(t, func() {
		NewEnrichmentService(&db.Database{}, nil, nil, nil, nil, nil, nil, assertSvc, nil)
	})
	require.Panics(t, func() {
		NewEnrichmentService(&db.Database{}, nil, nil, nil, nil, nil, nil, nil, stubCacheRefresher{})
	})
}

// TestNewServices_NilPairLeavesKnowledgeNil documents the both-neither branch:
// a fully-nil pair must NOT build a knowledge writer (an unconditional wrap
// would flip the clean "not wired" guard into a nil-pointer panic deep in
// assertCreate). Proven indirectly by the guarded-path error tests above +
// asserted here at construction (no panic, service constructs cleanly).
func TestNewServices_NilPairConstructsCleanly(t *testing.T) {
	t.Parallel()
	require.NotPanics(t, func() {
		nilContactService()
		NewEnrichmentService(&db.Database{}, nil, nil, nil, nil, nil, nil, nil, nil)
	})
}
