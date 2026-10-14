package consumer

import (
	"context"
	"errors"
	"fmt"
	"time"

	"personal-crm/backend/internal/config"
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
// source_ref (the internal calendar_event.ID UUID string), read the alias tree
// of its interaction node and the contact's recorded participation, and
// soft-delete it. Interface
// defined here (consumer-side pattern) so unit tests can stub without a DB.
type declineInteractionRepo interface {
	FindBySourceRefTx(ctx context.Context, tx pgx.Tx, contactID uuid.UUID, source, sourceRef string) (*repository.Interaction, error)
	GetInteractionTreeTx(ctx context.Context, tx pgx.Tx, nodeID uuid.UUID) (*repository.InteractionTree, error)
	ListRecordedParticipationTx(ctx context.Context, tx pgx.Tx, contactID uuid.UUID) ([]repository.RecordedEntry, error)
	SoftDeleteInteractionTx(ctx context.Context, tx pgx.Tx, id uuid.UUID) error
}

// declineContactRepo is the subset of *repository.ContactRepository the
// decline handler depends on: take the contact's cadence row lock, and
// surgically recompute the contact's date columns after the derived
// interaction is soft-deleted.
type declineContactRepo interface {
	LockContactForDateRecomputeTx(ctx context.Context, tx pgx.Tx, contactID uuid.UUID) error
	RecomputeContactDatesAfterDeleteTx(ctx context.Context, tx pgx.Tx, contactID uuid.UUID, deletedAt time.Time, watchdog config.WatchdogConfig) error
}

// declineNodeRepo is the subset of *repository.NodeRepository the decline
// handler depends on: the correction lock on the observation's interaction
// node.
type declineNodeRepo interface {
	AcquireCorrectionLocksTx(ctx context.Context, tx pgx.Tx, nodeIDs ...uuid.UUID) error
}

// declineCadence is the subset of *CadenceUpdater the decline handler
// recomputes a merged participation's cadence fields through.
type declineCadence interface {
	RecomputeAfterCorrection(ctx context.Context, tx pgx.Tx, contactID uuid.UUID, participation []repository.RecordedParticipation, judgedRecordedAt time.Time) error
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
	nodes        declineNodeRepo
	cadence      declineCadence
	watchdog     config.WatchdogConfig
}

// NewCalendarDeclineHandler builds the consumer with narrow repository
// interfaces so unit tests can stub them. Production wires the concrete
// interaction, contact and node repositories and the CadenceUpdater.
func NewCalendarDeclineHandler(interactions declineInteractionRepo, contacts declineContactRepo, nodes declineNodeRepo, cadence declineCadence, watchdog config.WatchdogConfig) *CalendarDeclineHandler {
	return &CalendarDeclineHandler{interactions: interactions, contacts: contacts, nodes: nodes, cadence: cadence, watchdog: watchdog}
}

// HandleEvent processes a calendar.declined envelope inside the caller's tx.
//
//  1. Decode + validate the payload (ContactID non-nil, EventID non-empty —
//     a publisher-bug guard).
//  2. Find the live derived interaction by (ContactID, "gcal", EventID).
//     ErrNotFound → no live interaction to remove (the common future-decline
//     case + an already-cleaned replay). Return nil; no recompute.
//  3. Take the correction lock on its interaction node, before reading its
//     alias tree, so no correction changes the tree until commit. Then take
//     the contact's cadence row lock, before reading the contact's
//     participation, so no recording of the contact commits between that
//     read and the recompute decided from it.
//  4. Soft-delete it and recompute the contact. When the observation's alias
//     tree holds more than one interaction node (a merged interaction), the
//     correction recompute replays the contact's remaining effective
//     participation (CAD-047), however many of the merged members the
//     contact still has. Otherwise the contact's date columns are recomputed
//     keyed on the deleted occurred_at (CAL-008).
//     A db.ErrNotFound from the contact lock or the recompute (contact
//     soft-deleted between publish and consume) is a benign no-op — the interaction is already gone and a
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

	// A live gcal observation always has an interaction node
	// (interaction_node_representable_check).
	if err := h.nodes.AcquireCorrectionLocksTx(ctx, tx, *found.InteractionNodeID); err != nil {
		return fmt.Errorf("lock declined interaction node: %w", err)
	}
	if err := h.contacts.LockContactForDateRecomputeTx(ctx, tx, p.ContactID); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			// Contact soft-deleted between publish and consume: remove the
			// interaction; a deleted contact needs no recompute.
			if err := h.interactions.SoftDeleteInteractionTx(ctx, tx, found.ID); err != nil {
				return fmt.Errorf("soft-delete gcal interaction: %w", err)
			}
			return nil
		}
		return fmt.Errorf("lock contact for decline: %w", err)
	}
	tree, err := h.interactions.GetInteractionTreeTx(ctx, tx, *found.InteractionNodeID)
	if err != nil {
		return fmt.Errorf("read alias tree of declined interaction: %w", err)
	}
	merged := len(tree.Nodes) > 1
	// The merged participation the decline judges was recorded no earlier
	// than the declined observation, and as late as its latest member.
	judgedRecordedAt := found.RecordedAt
	if merged {
		before, err := h.interactions.ListRecordedParticipationTx(ctx, tx, p.ContactID)
		if err != nil {
			return fmt.Errorf("read participation before decline: %w", err)
		}
		for _, entry := range before {
			if entry.InteractionNodeID != nil && *entry.InteractionNodeID == tree.Root && entry.RecordedAt.After(judgedRecordedAt) {
				judgedRecordedAt = entry.RecordedAt
			}
		}
	}

	if err := h.interactions.SoftDeleteInteractionTx(ctx, tx, found.ID); err != nil {
		return fmt.Errorf("soft-delete gcal interaction: %w", err)
	}

	if merged {
		err = h.recomputeMerged(ctx, tx, p.ContactID, judgedRecordedAt)
	} else {
		err = h.contacts.RecomputeContactDatesAfterDeleteTx(ctx, tx, p.ContactID, found.OccurredAt, h.watchdog)
	}
	if err != nil {
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

// recomputeMerged recomputes the contact's cadence fields from its effective
// participation after the decline, through the correction recompute.
// judgedRecordedAt is when the merged participation that held the declined
// observation was last recorded.
func (h *CalendarDeclineHandler) recomputeMerged(ctx context.Context, tx pgx.Tx, contactID uuid.UUID, judgedRecordedAt time.Time) error {
	entries, err := h.interactions.ListRecordedParticipationTx(ctx, tx, contactID)
	if err != nil {
		return err
	}
	participation := make([]repository.RecordedParticipation, len(entries))
	for i, entry := range entries {
		participation[i] = entry.RecordedParticipation
	}
	return h.cadence.RecomputeAfterCorrection(ctx, tx, contactID, participation, judgedRecordedAt)
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
