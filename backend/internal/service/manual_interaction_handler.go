package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// manualInteractionRecorder is the subset of *consumer.InteractionRecorder
// the manual handler depends on. Interface defined here so the api/handlers
// package can stub it in tests without importing the consumer package
// wholesale. Production wiring passes the concrete type.
type manualInteractionRecorder interface {
	HandleEvent(ctx context.Context, tx pgx.Tx, env *events.Envelope) (*repository.Interaction, func(context.Context), error)
}

// manualInteractionBus is the subset of *events.Bus used by
// ManualInteractionHandler.Run.
type manualInteractionBus interface {
	PublishTx(ctx context.Context, tx pgx.Tx, env *events.Envelope) error
}

// ManualInteractionHandler orchestrates the cutover-mode publish + inline
// consumer invocation for a manual-UI interaction. Called by
// InteractionHandler (POST /contacts/:id/interactions).
//
// Flow:
//  1. Build interaction.manual envelope from the handler args.
//  2. Open a pgx.Tx on the application pool.
//  3. bus.PublishTx — inserts the event row (no river job enqueued; the
//     consumer runs inline via HandleEvent).
//  4. recorder.HandleEvent — delegates to ContactService.RecordInteractionTx
//     which does dedup + contact check + insert + cadence updates. The
//     consumer also emits interaction.recorded in the same tx on fresh
//     writes (spec §3.4.1 atomicity contract).
//  5. Tx commit.
//  6. Invoke the returned postCommit closure (best-effort follow-up).
//
// Returns the persisted interaction row so the handler can render a 201
// response. Errors are propagated — db.ErrNotFound surfaces for a missing
// contact; all other errors fail the request and the tx rolls back (no
// partial write).
//
// Rollback-era note: pre-PR-6 this helper was named ManualInteractionShadow
// and ran a two-tx flow alongside a direct ContactService.RecordInteraction
// path. PR 6 collapsed both into this single-tx, single-writer flow.
type ManualInteractionHandler struct {
	pool     *pgxpool.Pool
	bus      manualInteractionBus
	recorder manualInteractionRecorder
}

// NewManualInteractionHandler builds the helper. All three parameters are
// required — construct this helper only when cutover wiring is active
// (main.go gates construction on mode==cutover). Pass nil to handlers to
// signal interactions are disabled (handlers return 503).
func NewManualInteractionHandler(pool *pgxpool.Pool, bus manualInteractionBus, recorder manualInteractionRecorder) *ManualInteractionHandler {
	return &ManualInteractionHandler{
		pool:     pool,
		bus:      bus,
		recorder: recorder,
	}
}

// Run executes the publish + inline-consumer flow. Returns the persisted
// interaction row for the HTTP response.
//
// Error mapping:
//   - db.ErrNotFound → contact doesn't exist (propagated for 404 mapping).
//   - publish / consumer errors → wrapped; caller maps to 500.
func (s *ManualInteractionHandler) Run(
	ctx context.Context,
	contactID uuid.UUID,
	direction string,
	occurredAt time.Time,
	description string,
) (*repository.Interaction, error) {
	if s == nil {
		return nil, errors.New("manual interaction handler not configured")
	}
	if s.pool == nil || s.bus == nil || s.recorder == nil {
		return nil, errors.New("manual interaction handler missing dependencies")
	}

	env, err := buildManualEnvelope(contactID, direction, occurredAt, description)
	if err != nil {
		return nil, fmt.Errorf("build manual envelope: %w", err)
	}

	var (
		interaction *repository.Interaction
		postCommit  func(context.Context)
	)
	err = pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if pubErr := s.bus.PublishTx(ctx, tx, env); pubErr != nil {
			return fmt.Errorf("publish interaction.manual: %w", pubErr)
		}
		row, pc, invErr := s.recorder.HandleEvent(ctx, tx, env)
		if invErr != nil {
			return fmt.Errorf("inline consumer: %w", invErr)
		}
		interaction = row
		postCommit = pc
		return nil
	})
	if err != nil {
		return nil, err
	}
	if postCommit != nil {
		postCommit(ctx)
	}
	return interaction, nil
}

// buildManualEnvelope constructs the interaction.manual envelope. SourceID
// is left empty because manual interactions have no stable external key —
// consumer dedup uses FindInWindow (30 min window) instead of the
// (source, source_id) unique index.
func buildManualEnvelope(
	contactID uuid.UUID,
	direction string,
	occurredAt time.Time,
	description string,
) (*events.Envelope, error) {
	if direction == "" {
		direction = repository.InteractionDirectionMutual
	}
	payload := events.InteractionManualPayload{
		Version:     1,
		ContactID:   contactID,
		Direction:   direction,
		OccurredAt:  occurredAt,
		Description: description,
	}
	raw, err := events.Marshal(events.KindInteractionManual, payload)
	if err != nil {
		return nil, err
	}

	return &events.Envelope{
		Source:     repository.InteractionSourceManual,
		SourceID:   "",
		Kind:       events.KindInteractionManual,
		Payload:    raw,
		ObservedAt: occurredAt,
	}, nil
}
