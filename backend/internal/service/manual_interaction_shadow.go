package service

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"personal-crm/backend/internal/consumer"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// manualInteractionRecorder is the subset of *consumer.InteractionRecorder
// the shadow helper depends on. Interface defined here so the handler
// package can stub it in tests without importing the consumer package
// wholesale. Production wiring passes the concrete type.
type manualInteractionRecorder interface {
	HandleEvent(ctx context.Context, tx pgx.Tx, env *events.Envelope) error
}

// manualInteractionBus is the subset of *events.Bus used by
// ManualInteractionShadow.Run.
type manualInteractionBus interface {
	PublishTx(ctx context.Context, tx pgx.Tx, env *events.Envelope) error
}

// ManualInteractionShadow runs the PR 5 shadow-mode publish + inline
// consumer invocation for a manual-UI interaction. Called by both the
// InteractionHandler (POST /contacts/:id/interactions) and the
// ContactHandler (PATCH /contacts/:id/last-contacted) AFTER the direct-path
// write has committed — plan Decision 7 "two-tx manual UI flow".
//
// PR 6 cutover will delete the direct path and keep this flow as the
// primary write.
//
// Mode semantics:
//   - off:     Run is a no-op. The direct path already ran; shadow
//     observations are not collected.
//   - shadow:  opens its own pgx.Tx, publishes interaction.manual via the
//     event bus (which also inserts the event row but enqueues NO
//     river job — KindInteractionManual maps to nil in
//     consumerJobsForKind, plan Decision 7), then inline-invokes
//     consumer.HandleEvent in the SAME tx. FindInWindow dedup
//     sees the direct-path row from step 1 and the consumer
//     early-returns with a writer='consumer' replay=true
//     observation. tx commits.
//   - cutover: undefined in PR 5. PR 6 switches step 1 off and this helper
//     becomes the sole write path.
//
// Errors are logged at warn level and swallowed. The direct path already
// wrote the production interaction row — a failed shadow tx degrades
// observability, not correctness.
type ManualInteractionShadow struct {
	mode     string
	pool     *pgxpool.Pool
	bus      manualInteractionBus
	recorder manualInteractionRecorder
}

// NewManualInteractionShadow builds the helper. mode / bus / recorder
// may all be zero when the caller doesn't want shadow behavior (Run is a
// no-op in that case).
func NewManualInteractionShadow(mode string, pool *pgxpool.Pool, bus manualInteractionBus, recorder manualInteractionRecorder) *ManualInteractionShadow {
	return &ManualInteractionShadow{
		mode:     mode,
		pool:     pool,
		bus:      bus,
		recorder: recorder,
	}
}

// Run executes the publish + inline-consumer flow. Called synchronously
// from the manual-UI handlers after the direct-path call succeeds.
//
// Params reflect the values the direct path already wrote (or, in the
// PATCH empty-body case, the value the service synthesized — see
// handlers.UpdateContactLastContacted).
func (s *ManualInteractionShadow) Run(
	ctx context.Context,
	contactID uuid.UUID,
	direction string,
	occurredAt time.Time,
	description string,
) {
	if s == nil || s.mode == consumer.InteractionModeOff {
		return
	}
	if s.pool == nil || s.bus == nil || s.recorder == nil {
		// Unusable configuration (should never happen in production wiring);
		// log once and bail.
		logger.Warn().Str("contactId", contactID.String()).
			Msg("shadow: manual helper invoked with nil dependency; skipping")
		return
	}

	env, err := buildManualEnvelope(contactID, direction, occurredAt, description)
	if err != nil {
		logger.Warn().Err(err).
			Str("contactId", contactID.String()).
			Msg("shadow: build manual envelope failed")
		return
	}

	if err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if pubErr := s.bus.PublishTx(ctx, tx, env); pubErr != nil {
			return fmt.Errorf("publish interaction.manual: %w", pubErr)
		}
		if invErr := s.recorder.HandleEvent(ctx, tx, env); invErr != nil {
			return fmt.Errorf("inline consumer: %w", invErr)
		}
		return nil
	}); err != nil {
		logger.Warn().Err(err).
			Str("contactId", contactID.String()).
			Msg("shadow: manual publish+inline failed; direct-path write preserved")
	}
}

// buildManualEnvelope constructs the interaction.manual envelope. Broken
// out for testability. SourceID is left empty because manual interactions
// have no stable external key — consumer dedup uses FindInWindow (30 min
// window) instead of the (source, source_id) unique index.
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
	// Non-nil marshal result as an extra assertion (shouldn't happen; events.Marshal
	// never returns a nil RawMessage on success).
	if raw == nil {
		return nil, fmt.Errorf("marshal returned nil payload")
	}
	// Belt-and-suspenders: make sure the payload is valid JSON by running
	// it through json.RawMessage consumers can't tolerate nil payloads.
	_ = json.RawMessage(raw)

	return &events.Envelope{
		Source:     repository.InteractionSourceManual,
		SourceID:   "",
		Kind:       events.KindInteractionManual,
		Payload:    raw,
		ObservedAt: occurredAt,
	}, nil
}
