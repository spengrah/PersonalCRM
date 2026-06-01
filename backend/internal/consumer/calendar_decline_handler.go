package consumer

import (
	"context"
	"errors"
	"fmt"
	"time"

	"personal-crm/backend/internal/consumer/consumerjobs"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
)

// declineInteractionRepo is the subset of *repository.InteractionRepository
// the decline handler depends on: find the derived gcal interaction by its
// source_ref (the internal calendar_event.ID UUID string) and soft-delete
// it. Interface defined here (consumer-side pattern) so unit tests can stub
// without a DB.
type declineInteractionRepo interface {
	FindBySourceRefTx(ctx context.Context, tx pgx.Tx, contactID uuid.UUID, source, sourceRef string) (*repository.Interaction, error)
	SoftDeleteInteractionTx(ctx context.Context, tx pgx.Tx, id uuid.UUID) error
}

// declineContactRepo is the subset of *repository.ContactRepository the
// decline handler depends on: surgically recompute the contact's date
// columns after the derived interaction is soft-deleted.
type declineContactRepo interface {
	RecomputeContactDatesAfterDeleteTx(ctx context.Context, tx pgx.Tx, contactID uuid.UUID, deletedAt time.Time) error
}

// CalendarDeclineHandler is the event-bus consumer for calendar.declined.
// When a stored calendar_event is declined / cancelled / user-removed
// upstream, the publisher removes the calendar_event row and emits one
// calendar.declined per matched contact; this consumer soft-deletes the
// derived gcal interaction for that contact and recomputes the contact's
// date columns.
//
// The payload's EventID is the INTERNAL calendar_event.ID UUID string —
// the same value the forward path wrote into interaction.source_ref for
// source='gcal' — so FindBySourceRefTx(contactID, "gcal", EventID) matches
// the attended interaction exactly.
type CalendarDeclineHandler struct {
	interactions declineInteractionRepo
	contacts     declineContactRepo
}

// NewCalendarDeclineHandler builds the consumer with narrow repository
// interfaces so unit tests can stub them. Production wires the concrete
// interaction + contact repositories.
func NewCalendarDeclineHandler(interactions declineInteractionRepo, contacts declineContactRepo) *CalendarDeclineHandler {
	return &CalendarDeclineHandler{interactions: interactions, contacts: contacts}
}

// HandleEvent processes a calendar.declined envelope inside the caller's tx.
//
//  1. Decode + validate the payload (ContactID non-nil, EventID non-empty —
//     a publisher-bug guard).
//  2. Find the live derived interaction by (ContactID, "gcal", EventID).
//     ErrNotFound → no live interaction to remove (the common future-decline
//     case + an already-cleaned replay). Return nil; no recompute.
//  3. Capture its occurred_at, soft-delete it.
//  4. Recompute the contact's date columns keyed on the deleted occurred_at.
//     A db.ErrNotFound here (contact soft-deleted between publish and
//     consume) is a benign no-op — the interaction is already gone and a
//     deleted contact needs no recompute; do NOT propagate (would poison
//     River retries).
func (h *CalendarDeclineHandler) HandleEvent(ctx context.Context, tx pgx.Tx, env *events.Envelope) error {
	if env == nil {
		return errors.New("calendar_decline_handler: nil envelope")
	}
	if tx == nil {
		return errors.New("calendar_decline_handler: nil tx")
	}

	var p events.CalendarDeclinedPayload
	if err := events.Unmarshal(env, &p); err != nil {
		return fmt.Errorf("unmarshal calendar.declined payload: %w", err)
	}
	if p.ContactID == uuid.Nil {
		return fmt.Errorf("calendar.declined: empty contact_id (event %s)", env.ID)
	}
	if p.EventID == "" {
		return fmt.Errorf("calendar.declined: empty event_id (event %s)", env.ID)
	}

	found, err := h.interactions.FindBySourceRefTx(ctx, tx, p.ContactID, repository.InteractionSourceGCal, p.EventID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			// No live interaction to remove (future-decline that was never
			// recorded, or an already-cleaned replay). Skip recompute so the
			// dominant case stays write-free and unaffected contacts don't
			// get a spurious updated_at bump.
			return nil
		}
		return fmt.Errorf("find gcal interaction for decline: %w", err)
	}

	deletedOccurredAt := found.OccurredAt
	if err := h.interactions.SoftDeleteInteractionTx(ctx, tx, found.ID); err != nil {
		return fmt.Errorf("soft-delete gcal interaction: %w", err)
	}

	if err := h.contacts.RecomputeContactDatesAfterDeleteTx(ctx, tx, p.ContactID, deletedOccurredAt); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			// Contact soft-deleted between publish and consume. The
			// interaction is already soft-deleted; a deleted contact needs
			// no recompute. Benign no-op — return nil so River does not
			// retry-poison.
			return nil
		}
		return fmt.Errorf("recompute contact dates after decline: %w", err)
	}
	return nil
}

// --------------------------------------------------------------------------
// River worker wrapper. Mirrors InteractionRecorderWorker: fetch the
// envelope by id, open a fresh tx, call HandleEvent. No post-commit closure
// (the decline path performs no external I/O).
// --------------------------------------------------------------------------

// CalendarDeclineHandlerWorker is the river worker that dispatches queued
// CalendarDeclineHandlerJobArgs to CalendarDeclineHandler.HandleEvent.
type CalendarDeclineHandlerWorker struct {
	river.WorkerDefaults[consumerjobs.CalendarDeclineHandlerJobArgs]
	bus     eventBusTx
	pool    *pgxpool.Pool
	handler *CalendarDeclineHandler
}

// NewCalendarDeclineHandlerWorker wires the worker to the concrete bus, the
// application pgxpool, and the consumer instance.
func NewCalendarDeclineHandlerWorker(bus eventBusTx, pool *pgxpool.Pool, handler *CalendarDeclineHandler) *CalendarDeclineHandlerWorker {
	return &CalendarDeclineHandlerWorker{bus: bus, pool: pool, handler: handler}
}

// Work implements river.Worker. Fetches the event envelope by id, opens a
// fresh tx, and invokes HandleEvent. On error River retries per MaxAttempts
// (5 from the InsertOpts in events.consumerJobsForKind).
func (w *CalendarDeclineHandlerWorker) Work(ctx context.Context, j *river.Job[consumerjobs.CalendarDeclineHandlerJobArgs]) error {
	env, err := w.bus.GetEvent(ctx, j.Args.EventID)
	if err != nil {
		return fmt.Errorf("fetch event %s: %w", j.Args.EventID, err)
	}
	return pgx.BeginTxFunc(ctx, w.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		return w.handler.HandleEvent(ctx, tx, env)
	})
}

// Timeout bounds a single decline-handler run. A soft-delete + single-contact
// recompute completes in ~10ms on the Pi; 30s is ample headroom.
func (*CalendarDeclineHandlerWorker) Timeout(*river.Job[consumerjobs.CalendarDeclineHandlerJobArgs]) time.Duration {
	return 30 * time.Second
}
