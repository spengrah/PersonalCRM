package events

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"personal-crm/backend/internal/db"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

// TestConsumerJobsForKind_EmptyForAllKinds is the guardrail: PR 2 registers
// no consumer jobs for any kind. A future PR that wires a consumer into
// the registry must update this test, preventing silent partial wiring.
func TestConsumerJobsForKind_EmptyForAllKinds(t *testing.T) {
	for _, k := range AllKinds {
		jobs := consumerJobsForKind(k, uuid.New())
		require.Empty(t, jobs, "kind %s: expected empty consumer-job slice", k)
	}
}

// stubEventRepo is a test-only EventRepository that lets Bus_test files
// exercise envelope validation without touching the database or river.
type stubEventRepo struct {
	insertCalls int
	insertErr   error
}

func (s *stubEventRepo) InsertEvent(_ context.Context, _ pgx.Tx, _ *Envelope) error {
	s.insertCalls++
	return s.insertErr
}

func (s *stubEventRepo) GetEvent(_ context.Context, _ uuid.UUID) (*Envelope, error) {
	return nil, db.ErrNotFound
}

func (s *stubEventRepo) FindEventBySource(_ context.Context, _, _ string) (*Envelope, error) {
	return nil, db.ErrNotFound
}

// nonNilStubTx returns a non-nil pgx.Tx for tests that only exercise
// validation paths (the stub repo never touches the tx). Implementing the
// full pgx.Tx interface for a mock is overkill — a typed-nil interface
// value is non-nil to the Bus's nil check while never being dereferenced
// because validation errors fire before InsertEvent runs.
func nonNilStubTx() pgx.Tx {
	var t *stubTxImpl
	return t
}

// stubTxImpl is a compile-time marker: it satisfies pgx.Tx by embedding a
// valid interface satisfier. The typed-nil receiver never has a method
// called on it in these tests (validation short-circuits before
// InsertEvent runs).
type stubTxImpl struct{ pgx.Tx }

// TestPublishTx_ValidatesEnvelope asserts that PublishTx rejects malformed
// envelopes BEFORE calling InsertEvent. Because validation fails first, the
// stub's InsertEvent is never called and riverClient stays nil-safe.
func TestPublishTx_ValidatesEnvelope(t *testing.T) {
	ctx := context.Background()
	validPayload := json.RawMessage(`{"version":1,"peer_ref":"tg:1:2","message_at":"2026-04-10T12:00:00Z"}`)
	validObservedAt := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name     string
		tx       pgx.Tx
		env      *Envelope
		wantSubs string
	}{
		{
			name:     "nil tx",
			tx:       nil,
			env:      nil, // irrelevant; tx check fires first
			wantSubs: "nil tx",
		},
		{
			name:     "nil envelope",
			tx:       nonNilStubTx(),
			env:      nil,
			wantSubs: "nil envelope",
		},
		{
			name: "empty kind",
			tx:   nonNilStubTx(),
			env: &Envelope{
				Source:     "telegram",
				Payload:    validPayload,
				ObservedAt: validObservedAt,
			},
			wantSubs: "empty kind",
		},
		{
			name: "unknown kind",
			tx:   nonNilStubTx(),
			env: &Envelope{
				Kind:       Kind("made.up"),
				Source:     "telegram",
				Payload:    validPayload,
				ObservedAt: validObservedAt,
			},
			wantSubs: "unknown kind",
		},
		{
			name: "empty source",
			tx:   nonNilStubTx(),
			env: &Envelope{
				Kind:       KindMessageReceived,
				Payload:    validPayload,
				ObservedAt: validObservedAt,
			},
			wantSubs: "empty source",
		},
		{
			name: "empty payload",
			tx:   nonNilStubTx(),
			env: &Envelope{
				Kind:       KindMessageReceived,
				Source:     "telegram",
				ObservedAt: validObservedAt,
			},
			wantSubs: "empty payload",
		},
		{
			name: "zero observed_at",
			tx:   nonNilStubTx(),
			env: &Envelope{
				Kind:    KindMessageReceived,
				Source:  "telegram",
				Payload: validPayload,
			},
			wantSubs: "empty observed_at",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := &stubEventRepo{}
			// Bus with nil pool + nil riverClient is safe as long as
			// validation fails before we reach InsertEvent.
			bus := &Bus{eventRepo: stub}
			err := bus.PublishTx(ctx, tc.tx, tc.env)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantSubs)
			require.Zero(t, stub.insertCalls, "InsertEvent must not be called on validation failure")
		})
	}
}

// TestPublishTx_DuplicateIsNoOp asserts that a repo returning db.ErrDuplicate
// surfaces as a nil error from PublishTx (idempotent no-op). Since the stub's
// InsertEvent returns ErrDuplicate BEFORE the consumer-job loop runs, nil
// riverClient is still safe.
func TestPublishTx_DuplicateIsNoOp(t *testing.T) {
	ctx := context.Background()
	stub := &stubEventRepo{insertErr: db.ErrDuplicate}
	bus := &Bus{eventRepo: stub}

	env := &Envelope{
		Kind:       KindMessageReceived,
		Source:     "telegram",
		SourceID:   "tg:1:2:42",
		Payload:    json.RawMessage(`{"version":1}`),
		ObservedAt: time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
	}
	err := bus.PublishTx(ctx, nonNilStubTx(), env)
	require.NoError(t, err)
	require.Equal(t, 1, stub.insertCalls)
}

// TestPublishTx_RepoErrorPropagates asserts that a non-ErrDuplicate repo
// error propagates wrapped to the caller.
func TestPublishTx_RepoErrorPropagates(t *testing.T) {
	ctx := context.Background()
	sentinel := errors.New("db down")
	stub := &stubEventRepo{insertErr: sentinel}
	bus := &Bus{eventRepo: stub}

	env := &Envelope{
		Kind:       KindMessageReceived,
		Source:     "telegram",
		SourceID:   "tg:1:2:42",
		Payload:    json.RawMessage(`{"version":1}`),
		ObservedAt: time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
	}
	err := bus.PublishTx(ctx, nonNilStubTx(), env)
	require.Error(t, err)
	require.ErrorIs(t, err, sentinel)
	require.Contains(t, err.Error(), "insert event")
}
