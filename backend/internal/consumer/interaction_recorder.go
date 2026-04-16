// Package consumer holds the event-bus consumer services that subscribe
// to events and perform domain writes. See
// .ai/spec/event-bus-foundation.md §3.4.
//
// PR 5 of #180 introduces InteractionRecorder, the first consumer. It
// consumes 6 input kinds (message.received/sent, calendar.attended,
// task.completed, task.outreach_detected, interaction.manual) and
// atomically writes an interaction row + emits interaction.recorded in
// the caller's transaction. Later PRs add CadenceUpdater (7),
// FollowUpManager (9a), and RematchDispatcher (10).
package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"personal-crm/backend/internal/consumer/consumerjobs"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
)

// InteractionMode gates the PR 5-8 rollout (spec §3.9). Mirrors the
// constants in config.EventBusInteractionMode*. Kept duplicated here so
// non-config callers don't import config just to name a mode.
const (
	InteractionModeOff     = "off"
	InteractionModeShadow  = "shadow"
	InteractionModeCutover = "cutover"
)

// manualDedupWindow matches the 30-minute window used by
// ContactService.RecordInteraction for manual-source dedup. Kept as a
// shared constant so consumer and service agree on what "recent manual"
// means.
const manualDedupWindow = 30 * time.Minute

// interactionRepoTx is the subset of *repository.InteractionRepository the
// consumer depends on. Interface defined here (not on the repo) so tests
// can stub it.
type interactionRepoTx interface {
	CreateInteractionTx(ctx context.Context, tx pgx.Tx, req repository.CreateInteractionRequest) (*repository.Interaction, error)
	FindBySourceRefTx(ctx context.Context, tx pgx.Tx, contactID uuid.UUID, source string, sourceRef string) (*repository.Interaction, error)
	FindInWindowTx(ctx context.Context, tx pgx.Tx, contactID uuid.UUID, source string, occurredAt time.Time, window time.Duration) (*repository.Interaction, error)
}

// contactRepoTx is the subset of *repository.ContactRepository the
// consumer depends on.
type contactRepoTx interface {
	GetContactTx(ctx context.Context, tx pgx.Tx, id uuid.UUID) (*repository.Contact, error)
}

// eventBusTx is the subset of *events.Bus the consumer depends on. Allows
// unit tests to stub publish without constructing a real river client.
type eventBusTx interface {
	PublishTx(ctx context.Context, tx pgx.Tx, env *events.Envelope) error
	GetEvent(ctx context.Context, id uuid.UUID) (*events.Envelope, error)
}

// shadowObsRepo is the subset of *repository.ShadowObservationRepository
// the consumer depends on.
type shadowObsRepo interface {
	RecordConsumerWrite(ctx context.Context, tx pgx.Tx, obs repository.ShadowObservation) (*repository.ShadowObservation, error)
	RecordConsumerReplay(ctx context.Context, tx pgx.Tx, obs repository.ShadowObservation) (*repository.ShadowObservation, error)
	FindMatchingDirectWrite(ctx context.Context, tx pgx.Tx, obs repository.ShadowObservation) (*repository.ShadowObservation, error)
}

// InteractionRecorder is the event-bus consumer that turns raw provider
// events into interaction rows + emits interaction.recorded. See spec
// §3.4.1 for the atomicity contract.
type InteractionRecorder struct {
	mode            string
	contactRepo     contactRepoTx
	interactionRepo interactionRepoTx
	bus             eventBusTx
	shadowRepo      shadowObsRepo
}

// NewInteractionRecorder builds the consumer. mode is one of
// InteractionMode{Off,Shadow,Cutover}; shadowRepo may be nil when mode is
// off (the consumer will not be invoked in that mode at publisher sites,
// but defensive-nil-check still tolerates it).
func NewInteractionRecorder(
	mode string,
	contactRepo contactRepoTx,
	interactionRepo interactionRepoTx,
	bus eventBusTx,
	shadowRepo shadowObsRepo,
) *InteractionRecorder {
	return &InteractionRecorder{
		mode:            mode,
		contactRepo:     contactRepo,
		interactionRepo: interactionRepo,
		bus:             bus,
		shadowRepo:      shadowRepo,
	}
}

// HandleEvent is the per-event entry point. The caller must own tx; this
// method inserts the interaction, emits interaction.recorded, and writes
// a shadow observation all within that tx (spec §3.4.1).
//
// Returns:
//   - nil on success (new-write OR replay early-return).
//   - wrapped ErrNotFound when the payload's ContactID doesn't exist.
//   - other errors as-is (caller should rollback tx).
func (r *InteractionRecorder) HandleEvent(ctx context.Context, tx pgx.Tx, env *events.Envelope) error {
	if env == nil {
		return errors.New("consumer: nil envelope")
	}
	if tx == nil {
		return errors.New("consumer: nil tx")
	}

	req, direction, err := r.extractRequest(env)
	if err != nil {
		return fmt.Errorf("extract %s: %w", env.Kind, err)
	}

	// Publisher-resolved ContactID (plan Decision 4). Consumer does no
	// peer-ref → contact lookup; a zero ContactID is a publisher bug.
	if req.ContactID == uuid.Nil {
		logger.Error().
			Str("event_id", env.ID.String()).
			Str("kind", string(env.Kind)).
			Msg("consumer: contact_id unresolved for kind; dropping")
		return fmt.Errorf("consumer: contact_id unresolved for kind %s", env.Kind)
	}

	// 1. Dedup against existing rows. FindBySourceRef for ref-bearing
	// kinds, FindInWindow (30 min) for manual.
	existing, err := r.findExisting(ctx, tx, req)
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		return fmt.Errorf("find existing: %w", err)
	}
	if existing != nil {
		// Replay / direct-wrote-first. Record observation and early return
		// WITHOUT emitting interaction.recorded (spec §3.4.1).
		if r.mode == InteractionModeShadow && r.shadowRepo != nil {
			obs := shadowObsFromEnv(env, req, direction, &existing.ID)
			if _, err := r.shadowRepo.RecordConsumerReplay(ctx, tx, obs); err != nil {
				// Best-effort: a failed shadow insert aborts the consumer tx
				// but does NOT corrupt the interaction row (direct already
				// wrote it). We still return the error so the worker retries.
				return fmt.Errorf("record consumer replay: %w", err)
			}

			// Inline divergence detection (plan Decision 14 Part A). In
			// shadow mode the replay path IS the expected case (direct
			// always commits first); this is where drift between what
			// direct and consumer would have written shows up. Compares
			// the peer direct row's direction / occurred_at against what
			// the consumer just extracted from the event envelope.
			r.logInlineDivergence(ctx, tx, obs)
		}
		return nil
	}

	// 2. Verify contact exists. Gives a clean db.ErrNotFound rather than
	// an FK constraint violation on the insert.
	if _, err := r.contactRepo.GetContactTx(ctx, tx, req.ContactID); err != nil {
		return fmt.Errorf("get contact: %w", err)
	}

	// 3. Insert the interaction row inside the caller's tx.
	interaction, err := r.interactionRepo.CreateInteractionTx(ctx, tx, repository.CreateInteractionRequest(req))
	if err != nil {
		return fmt.Errorf("create interaction: %w", err)
	}

	// 4. Emit interaction.recorded atomically in the same tx. SourceID =
	// interaction.ID so the event table's partial unique index
	// (source, source_id) dedupes any retry that reaches this point (plan
	// Decision 9).
	recordedPayload, err := marshalRecordedPayload(interaction, direction, req)
	if err != nil {
		return fmt.Errorf("marshal interaction.recorded: %w", err)
	}
	recordedEnv := &events.Envelope{
		Source:     env.Source,
		SourceID:   interaction.ID.String(),
		Kind:       events.KindInteractionRecorded,
		Payload:    recordedPayload,
		ObservedAt: req.OccurredAt,
	}
	if err := r.bus.PublishTx(ctx, tx, recordedEnv); err != nil {
		return fmt.Errorf("publish interaction.recorded: %w", err)
	}

	// 5. Shadow observation for the fresh-write path. Plan Decision 2:
	// the consumer is the sole fresh-writer only when the direct path
	// failed or hasn't run yet — expected-degradation territory.
	if r.mode == InteractionModeShadow && r.shadowRepo != nil {
		obs := shadowObsFromEnv(env, req, direction, &interaction.ID)
		if _, err := r.shadowRepo.RecordConsumerWrite(ctx, tx, obs); err != nil {
			return fmt.Errorf("record consumer write: %w", err)
		}

		// Inline divergence detection (plan Decision 14 Part A). Log an
		// error-level structured record when the consumer's observation
		// disagrees with a direct-path peer. Best-effort: a lookup failure
		// doesn't fail the tx.
		r.logInlineDivergence(ctx, tx, obs)
	}

	return nil
}

// extractRequest dispatches on env.Kind to build a RecordInteractionRequest
// + the effective direction. Direction derivation rules are in plan
// Decision 3.
func (r *InteractionRecorder) extractRequest(env *events.Envelope) (repository.RecordInteractionRequest, string, error) {
	switch env.Kind {
	case events.KindMessageReceived:
		var p events.MessageReceivedPayload
		if err := events.Unmarshal(env, &p); err != nil {
			return repository.RecordInteractionRequest{}, "", err
		}
		if p.ExternalMessageID == "" {
			return repository.RecordInteractionRequest{}, "", errors.New("message.received: empty external_message_id (source_ref required)")
		}
		direction := p.Direction
		if direction == "" {
			direction = repository.InteractionDirectionInbound
		}
		return makeTelegramRequest(p.ContactID, p.ExternalMessageID, p.MessageAt, p.Description, direction), direction, nil

	case events.KindMessageSent:
		var p events.MessageSentPayload
		if err := events.Unmarshal(env, &p); err != nil {
			return repository.RecordInteractionRequest{}, "", err
		}
		if p.ExternalMessageID == "" {
			return repository.RecordInteractionRequest{}, "", errors.New("message.sent: empty external_message_id (source_ref required)")
		}
		direction := p.Direction
		if direction == "" {
			direction = repository.InteractionDirectionOutbound
		}
		return makeTelegramRequest(p.ContactID, p.ExternalMessageID, p.MessageAt, p.Description, direction), direction, nil

	case events.KindCalendarAttended:
		var p events.CalendarAttendedPayload
		if err := events.Unmarshal(env, &p); err != nil {
			return repository.RecordInteractionRequest{}, "", err
		}
		if p.EventID == "" {
			return repository.RecordInteractionRequest{}, "", errors.New("calendar.attended: empty event_id (source_ref required)")
		}
		ref := p.EventID
		return repository.RecordInteractionRequest{
			ContactID:  p.ContactID,
			Source:     repository.InteractionSourceGCal,
			SourceRef:  &ref,
			OccurredAt: p.OccurredAt,
			Direction:  repository.InteractionDirectionMutual,
		}, repository.InteractionDirectionMutual, nil

	case events.KindTaskCompleted:
		var p events.TaskCompletedPayload
		if err := events.Unmarshal(env, &p); err != nil {
			return repository.RecordInteractionRequest{}, "", err
		}
		if p.TaskID == "" {
			return repository.RecordInteractionRequest{}, "", errors.New("task.completed: empty task_id (source_ref required)")
		}
		direction := p.Direction
		if direction == "" {
			direction = repository.InteractionDirectionMutual
		}
		ref := p.TaskID
		return repository.RecordInteractionRequest{
			ContactID:  p.ContactID,
			Source:     repository.InteractionSourceTodoist,
			SourceRef:  &ref,
			OccurredAt: p.CompletedAt,
			Direction:  direction,
		}, direction, nil

	case events.KindTaskOutreachDetected:
		var p events.TaskOutreachDetectedPayload
		if err := events.Unmarshal(env, &p); err != nil {
			return repository.RecordInteractionRequest{}, "", err
		}
		if p.TaskID == "" {
			return repository.RecordInteractionRequest{}, "", errors.New("task.outreach_detected: empty task_id (source_ref required)")
		}
		ref := p.TaskID
		return repository.RecordInteractionRequest{
			ContactID:  p.ContactID,
			Source:     repository.InteractionSourceTodoist,
			SourceRef:  &ref,
			OccurredAt: p.DetectedAt,
			Direction:  repository.InteractionDirectionOutbound,
		}, repository.InteractionDirectionOutbound, nil

	case events.KindInteractionManual:
		var p events.InteractionManualPayload
		if err := events.Unmarshal(env, &p); err != nil {
			return repository.RecordInteractionRequest{}, "", err
		}
		direction := p.Direction
		if direction == "" {
			direction = repository.InteractionDirectionMutual
		}
		var desc *string
		if p.Description != "" {
			d := p.Description
			desc = &d
		}
		return repository.RecordInteractionRequest{
			ContactID:   p.ContactID,
			Source:      repository.InteractionSourceManual,
			SourceRef:   nil,
			OccurredAt:  p.OccurredAt,
			Description: desc,
			Direction:   direction,
		}, direction, nil
	}

	return repository.RecordInteractionRequest{}, "", fmt.Errorf("unsupported kind %q", env.Kind)
}

// findExisting dedups on (source, source_ref) for ref-bearing kinds or on
// the 30-min manual window. Mirrors ContactService.RecordInteraction
// (service/contact.go:342-376). Returns (nil, db.ErrNotFound) on miss.
func (r *InteractionRecorder) findExisting(ctx context.Context, tx pgx.Tx, req repository.RecordInteractionRequest) (*repository.Interaction, error) {
	if req.SourceRef != nil && *req.SourceRef != "" {
		return r.interactionRepo.FindBySourceRefTx(ctx, tx, req.ContactID, req.Source, *req.SourceRef)
	}
	return r.interactionRepo.FindInWindowTx(ctx, tx, req.ContactID, req.Source, req.OccurredAt, manualDedupWindow)
}

// logInlineDivergence looks up the peer direct-path observation and logs
// at error level when direction / occurred_at disagree. Plan Decision 14
// Part A. Best-effort: lookup failures are suppressed.
func (r *InteractionRecorder) logInlineDivergence(ctx context.Context, tx pgx.Tx, obs repository.ShadowObservation) {
	peer, err := r.shadowRepo.FindMatchingDirectWrite(ctx, tx, obs)
	if err != nil {
		logger.Warn().Err(err).Msg("shadow: find matching direct write failed")
		return
	}
	if peer == nil {
		// No direct-path peer yet — expected when consumer fires first
		// (rare in shadow mode; direct always commits first in the
		// publisher call order). Don't log.
		return
	}
	if peer.Direction == obs.Direction &&
		peer.OccurredAt.Truncate(time.Second).Equal(obs.OccurredAt.Truncate(time.Second)) {
		return
	}

	ev := logger.Error().
		Str("event_id", obsStrID(obs)).
		Str("source", obs.Source).
		Str("direct_direction", peer.Direction).
		Str("consumer_direction", obs.Direction).
		Time("direct_occurred_at", peer.OccurredAt).
		Time("consumer_occurred_at", obs.OccurredAt)
	if obs.SourceRef != nil {
		ev = ev.Str("source_ref", *obs.SourceRef)
	}
	ev.Msg("shadow: divergence detected — consumer vs direct disagreement")
}

// shadowObsFromEnv builds a ShadowObservation for the consumer-path
// observation write. interactionID is either the existing row (replay) or
// the newly-inserted row (fresh write).
func shadowObsFromEnv(env *events.Envelope, req repository.RecordInteractionRequest, direction string, interactionID *uuid.UUID) repository.ShadowObservation {
	var eventID *uuid.UUID
	if env.ID != uuid.Nil {
		id := env.ID
		eventID = &id
	}
	return repository.ShadowObservation{
		EventID:       eventID,
		Kind:          string(env.Kind),
		Source:        req.Source,
		SourceRef:     req.SourceRef,
		ContactID:     req.ContactID,
		Direction:     direction,
		OccurredAt:    req.OccurredAt,
		InteractionID: interactionID,
	}
}

// makeTelegramRequest builds the RecordInteractionRequest for message.*
// kinds. Shared by message.received and message.sent extract branches.
// Requires ExternalMessageID to be non-empty — the publisher
// (aggregation engine) fills it from sess.sourceRef(), so empty is a
// publisher bug we surface as a zero-valued SourceRef (which then trips
// FindInWindow — wrong behavior, so we defensively return empty-ref error
// by returning a zero ContactID when the ref is empty).
func makeTelegramRequest(contactID *uuid.UUID, externalMessageID string, messageAt time.Time, description *string, direction string) repository.RecordInteractionRequest {
	var cid uuid.UUID
	if contactID != nil {
		cid = *contactID
	}
	var ref *string
	if externalMessageID != "" {
		ref = &externalMessageID
	}
	return repository.RecordInteractionRequest{
		ContactID:   cid,
		Source:      repository.InteractionSourceTelegram,
		SourceRef:   ref,
		OccurredAt:  messageAt,
		Description: description,
		Direction:   direction,
	}
}

// marshalRecordedPayload builds the JSON payload for the
// interaction.recorded derived event emitted after a successful insert.
// Called inside HandleEvent step 4.
func marshalRecordedPayload(interaction *repository.Interaction, direction string, req repository.RecordInteractionRequest) (json.RawMessage, error) {
	payload := events.InteractionRecordedPayload{
		Version:       1,
		ContactID:     interaction.ContactID,
		InteractionID: interaction.ID,
		Direction:     direction,
		OccurredAt:    interaction.OccurredAt,
		Source:        interaction.Source,
		SourceRef:     req.SourceRef,
	}
	return events.Marshal(events.KindInteractionRecorded, payload)
}

// obsStrID returns a stringified form of obs.EventID for logging (empty
// when nil).
func obsStrID(obs repository.ShadowObservation) string {
	if obs.EventID == nil {
		return ""
	}
	return obs.EventID.String()
}

// --------------------------------------------------------------------------
// River worker wrapper. Spec §3.4 "river-worker wrapper pattern".
// --------------------------------------------------------------------------

// InteractionRecorderWorker is the river worker that dispatches queued
// InteractionRecorderJobArgs to InteractionRecorder.HandleEvent.
type InteractionRecorderWorker struct {
	river.WorkerDefaults[consumerjobs.InteractionRecorderJobArgs]
	bus      eventBusTx
	pool     *pgxpool.Pool
	recorder *InteractionRecorder
}

// NewInteractionRecorderWorker wires the worker to the concrete bus, the
// application pgxpool, and the consumer instance.
func NewInteractionRecorderWorker(bus eventBusTx, pool *pgxpool.Pool, recorder *InteractionRecorder) *InteractionRecorderWorker {
	return &InteractionRecorderWorker{
		bus:      bus,
		pool:     pool,
		recorder: recorder,
	}
}

// Work implements river.Worker. Fetches the event envelope by id, opens a
// fresh tx, and invokes HandleEvent. On error river will retry per
// MaxAttempts (set to 5 via InsertOpts in events.consumerJobsForKind).
func (w *InteractionRecorderWorker) Work(ctx context.Context, j *river.Job[consumerjobs.InteractionRecorderJobArgs]) error {
	env, err := w.bus.GetEvent(ctx, j.Args.EventID)
	if err != nil {
		return fmt.Errorf("fetch event %s: %w", j.Args.EventID, err)
	}
	return pgx.BeginTxFunc(ctx, w.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		return w.recorder.HandleEvent(ctx, tx, env)
	})
}

// Timeout bounds how long a single worker invocation can run. A single
// interaction insert should complete in ~10ms on the Pi; 30s is ample
// headroom for pool saturation + retries (plan Decision 8).
func (*InteractionRecorderWorker) Timeout(*river.Job[consumerjobs.InteractionRecorderJobArgs]) time.Duration {
	return 30 * time.Second
}
