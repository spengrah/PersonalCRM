package consumer

import (
	"context"
	"errors"
	"testing"
	"time"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

// stubDeclineInteractionRepo stubs declineInteractionRepo. `found` (when
// non-nil) is returned by FindBySourceRefTx; otherwise findErr is returned
// (default db.ErrNotFound). softDeleteCalls + softDeletedID record the
// soft-delete.
type stubDeclineInteractionRepo struct {
	found   *repository.Interaction
	findErr error

	findCalls     int
	lastFindKey   [3]string // contactID, source, sourceRef (stringified)
	softDelCalls  int
	softDeletedID uuid.UUID
	softDelErr    error
}

func (s *stubDeclineInteractionRepo) FindBySourceRefTx(_ context.Context, _ pgx.Tx, contactID uuid.UUID, source, sourceRef string) (*repository.Interaction, error) {
	s.findCalls++
	s.lastFindKey = [3]string{contactID.String(), source, sourceRef}
	if s.found != nil {
		return s.found, nil
	}
	if s.findErr != nil {
		return nil, s.findErr
	}
	return nil, db.ErrNotFound
}

func (s *stubDeclineInteractionRepo) SoftDeleteInteractionTx(_ context.Context, _ pgx.Tx, id uuid.UUID) error {
	s.softDelCalls++
	s.softDeletedID = id
	return s.softDelErr
}

// stubDeclineContactRepo stubs declineContactRepo. recomputeErr is returned
// from RecomputeContactDatesAfterDeleteTx; recomputeCalls + lastDeletedAt
// record the call.
type stubDeclineContactRepo struct {
	recomputeCalls int
	lastContactID  uuid.UUID
	lastDeletedAt  time.Time
	recomputeErr   error
}

func (s *stubDeclineContactRepo) RecomputeContactDatesAfterDeleteTx(_ context.Context, _ pgx.Tx, contactID uuid.UUID, deletedAt time.Time) error {
	s.recomputeCalls++
	s.lastContactID = contactID
	s.lastDeletedAt = deletedAt
	return s.recomputeErr
}

func mustDeclineEnv(t *testing.T, p events.CalendarDeclinedPayload) *events.Envelope {
	t.Helper()
	raw, err := events.Marshal(events.KindCalendarDeclined, p)
	require.NoError(t, err)
	return &events.Envelope{
		ID:         uuid.New(),
		Source:     "gcal",
		SourceID:   "declined:" + p.EventID + ":" + p.ContactID.String(),
		Kind:       events.KindCalendarDeclined,
		Payload:    raw,
		ObservedAt: p.OccurredAt,
	}
}

// (a) interaction found → SoftDelete + Recompute both called.
func TestCalendarDeclineHandler_InteractionFound_SoftDeletesAndRecomputes(t *testing.T) {
	cid := uuid.New()
	eventUUID := uuid.New().String()
	occurredAt := time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)
	interactionID := uuid.New()

	ir := &stubDeclineInteractionRepo{found: &repository.Interaction{
		ID:         interactionID,
		ContactID:  cid,
		Source:     repository.InteractionSourceGCal,
		OccurredAt: occurredAt,
		Direction:  repository.InteractionDirectionMutual,
	}}
	cr := &stubDeclineContactRepo{}
	h := NewCalendarDeclineHandler(ir, cr)

	env := mustDeclineEnv(t, events.CalendarDeclinedPayload{
		Version: 1, ContactID: cid, EventID: eventUUID, OccurredAt: occurredAt,
	})
	require.NoError(t, h.HandleEvent(context.Background(), nonNilTx(), env))

	require.Equal(t, 1, ir.findCalls)
	require.Equal(t, [3]string{cid.String(), "gcal", eventUUID}, ir.lastFindKey, "looks up by (contact, gcal, internal-UUID source_ref)")
	require.Equal(t, 1, ir.softDelCalls)
	require.Equal(t, interactionID, ir.softDeletedID)
	require.Equal(t, 1, cr.recomputeCalls)
	require.Equal(t, cid, cr.lastContactID)
	require.Equal(t, occurredAt, cr.lastDeletedAt, "recompute keyed on the deleted interaction's occurred_at")
}

// (b) interaction not found → neither SoftDelete nor Recompute called.
func TestCalendarDeclineHandler_InteractionNotFound_NoOp(t *testing.T) {
	cid := uuid.New()
	ir := &stubDeclineInteractionRepo{} // default findErr → db.ErrNotFound
	cr := &stubDeclineContactRepo{}
	h := NewCalendarDeclineHandler(ir, cr)

	env := mustDeclineEnv(t, events.CalendarDeclinedPayload{
		Version: 1, ContactID: cid, EventID: uuid.New().String(),
		OccurredAt: time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC),
	})
	require.NoError(t, h.HandleEvent(context.Background(), nonNilTx(), env))

	require.Equal(t, 1, ir.findCalls)
	require.Zero(t, ir.softDelCalls, "no soft-delete when no live interaction")
	require.Zero(t, cr.recomputeCalls, "no recompute when nothing was removed")
}

// (c) nil ContactID / empty EventID → error.
func TestCalendarDeclineHandler_InvalidPayload_Errors(t *testing.T) {
	ir := &stubDeclineInteractionRepo{}
	cr := &stubDeclineContactRepo{}
	h := NewCalendarDeclineHandler(ir, cr)

	t.Run("nil contact_id", func(t *testing.T) {
		env := mustDeclineEnv(t, events.CalendarDeclinedPayload{
			Version: 1, ContactID: uuid.Nil, EventID: uuid.New().String(),
			OccurredAt: time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC),
		})
		err := h.HandleEvent(context.Background(), nonNilTx(), env)
		require.Error(t, err)
		require.Zero(t, ir.findCalls)
	})

	t.Run("empty event_id", func(t *testing.T) {
		env := mustDeclineEnv(t, events.CalendarDeclinedPayload{
			Version: 1, ContactID: uuid.New(), EventID: "",
			OccurredAt: time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC),
		})
		err := h.HandleEvent(context.Background(), nonNilTx(), env)
		require.Error(t, err)
	})
}

// (d) Recompute returns db.ErrNotFound (contact soft-deleted) → HandleEvent
// returns nil (benign no-op, NOT an error — no retry poison). The interaction
// was still soft-deleted.
func TestCalendarDeclineHandler_RecomputeContactNotFound_BenignNoOp(t *testing.T) {
	cid := uuid.New()
	occurredAt := time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)
	ir := &stubDeclineInteractionRepo{found: &repository.Interaction{
		ID:         uuid.New(),
		ContactID:  cid,
		Source:     repository.InteractionSourceGCal,
		OccurredAt: occurredAt,
		Direction:  repository.InteractionDirectionMutual,
	}}
	cr := &stubDeclineContactRepo{recomputeErr: db.ErrNotFound}
	h := NewCalendarDeclineHandler(ir, cr)

	env := mustDeclineEnv(t, events.CalendarDeclinedPayload{
		Version: 1, ContactID: cid, EventID: uuid.New().String(), OccurredAt: occurredAt,
	})
	require.NoError(t, h.HandleEvent(context.Background(), nonNilTx(), env), "contact-not-found recompute is a benign no-op")
	require.Equal(t, 1, ir.softDelCalls, "interaction still soft-deleted before recompute")
	require.Equal(t, 1, cr.recomputeCalls)
}

// A non-ErrNotFound recompute error propagates (River retries).
func TestCalendarDeclineHandler_RecomputeOtherError_Propagates(t *testing.T) {
	cid := uuid.New()
	occurredAt := time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)
	ir := &stubDeclineInteractionRepo{found: &repository.Interaction{
		ID: uuid.New(), ContactID: cid, Source: repository.InteractionSourceGCal,
		OccurredAt: occurredAt, Direction: repository.InteractionDirectionMutual,
	}}
	cr := &stubDeclineContactRepo{recomputeErr: errors.New("db down")}
	h := NewCalendarDeclineHandler(ir, cr)

	env := mustDeclineEnv(t, events.CalendarDeclinedPayload{
		Version: 1, ContactID: cid, EventID: uuid.New().String(), OccurredAt: occurredAt,
	})
	require.Error(t, h.HandleEvent(context.Background(), nonNilTx(), env))
}
