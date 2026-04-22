// Package consumer holds the event-bus consumer services that subscribe
// to events and perform domain writes. See
// .ai/spec/event-bus-foundation.md §3.4.
//
// PR 6 of #180 cuts the InteractionRecorder over from shadow mode to
// cutover — the consumer is now the sole writer of interaction rows for
// its 6 input kinds (message.received/sent, calendar.attended,
// task.completed, task.outreach_detected, interaction.manual). The
// write is delegated to ContactService.RecordInteractionTx so the
// dedup + cadence-update semantics stay in one place (plan Decision
// 4a). Later PRs add CadenceUpdater (7), FollowUpManager (9a), and
// RematchDispatcher (10).
package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"personal-crm/backend/internal/consumer/consumerjobs"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
)

// InteractionMode mirrors the constants in config.EventBusInteractionMode*.
// Kept duplicated here so non-config callers don't import config just
// to name a mode. "cutover" is the normal operating posture; "off" is
// the emergency-override gated behind EVENT_BUS_INTERACTION_UNSAFE_ALLOW_OFF
// for rollback without a code change. See EventBusConfig in config/ for
// the startup-gate semantics.
const (
	InteractionModeOff     = "off"
	InteractionModeCutover = "cutover"
)

// interactionWriter is the subset of *service.ContactService the consumer
// depends on. Interface defined here (not on the service) so tests can stub
// it without instantiating the full service graph. Production wiring passes
// the concrete *service.ContactService.
//
// `publishesEvent` is true for event-bus consumer callers: the service
// populates the V2 InteractionRecordedPayload snapshot fields
// (PrevCadence + CadenceAtEmit) on the returned result so the recorder
// can emit them on interaction.recorded. False for the non-bus wrapper
// (Todoist completion path), which takes the direct
// CadenceUpdater.ApplyInteraction route instead.
type interactionWriter interface {
	RecordInteractionTx(
		ctx context.Context, tx pgx.Tx, publishesEvent bool, req repository.RecordInteractionRequest,
	) (*repository.RecordInteractionResult, error)
}

// eventBusTx is the subset of *events.Bus the consumer depends on. Allows
// unit tests to stub publish without constructing a real river client.
type eventBusTx interface {
	PublishTx(ctx context.Context, tx pgx.Tx, env *events.Envelope) error
	GetEvent(ctx context.Context, id uuid.UUID) (*events.Envelope, error)
}

// telegramMessageMarker is the subset of *repository.TelegramMessageRepository
// the consumer depends on for the message.* path. Nullable in the constructor —
// non-telegram kinds bypass it.
type telegramMessageMarker interface {
	MarkMessagesProcessedTx(ctx context.Context, tx pgx.Tx, messageIDs []uuid.UUID, interactionID uuid.UUID) error
}

// cadenceDispatcher is the subset of *CadenceUpdater the recorder needs
// for the inline-apply path. Unit tests stub this without building a
// real claim store / contact reader.
type cadenceDispatcher interface {
	HandleEvent(ctx context.Context, tx pgx.Tx, env *events.Envelope) error
}

// followUpDispatcher is the subset of *FollowUpManager the recorder
// needs for the inline-apply path. Returns a post-commit closure (non-
// nil on the refresh branch) that the recorder folds into its own
// post-commit callback so the Todoist item_update runs outside the
// caller's tx (core.md rule 153).
type followUpDispatcher interface {
	HandleEvent(ctx context.Context, tx pgx.Tx, env *events.Envelope) (postCommit func(context.Context), err error)
}

// InteractionRecorder is the event-bus consumer that turns raw provider
// events into interaction rows + emits interaction.recorded. See spec
// §3.4.1 for the atomicity contract.
//
// On fresh writes, InteractionRecorder synchronously invokes
// CadenceUpdater.HandleEvent(ctx, tx, recordedEnv) after bus.PublishTx
// succeeds. That call claims the event on the durable
// event_consumer_claim table and, if it wins the claim, performs the
// cadence write in the SAME tx. A later queued delivery of the same
// event finds the claim row and returns nil without mutating contact
// state — closing the queued-worker replay hole for manual corrections.
type InteractionRecorder struct {
	writer              interactionWriter
	telegramMessageRepo telegramMessageMarker
	bus                 eventBusTx
	cadence             cadenceDispatcher
	followUp            followUpDispatcher
}

// NewInteractionRecorder builds the consumer. telegramMessageRepo may
// be nil for test environments that don't exercise message.* kinds.
// cadence MUST be non-nil in production wiring — the inline cadence
// apply is the seam that closes the queued-worker replay hole for
// manual corrections. followUp is inline-invoked post-publish so the
// manual UI stays synchronous and the queued delivery becomes a
// durable no-op via event_consumer_claim. Tests that don't care
// about cadence / follow-up pass nil stubs.
func NewInteractionRecorder(
	writer interactionWriter,
	telegramMessageRepo telegramMessageMarker,
	bus eventBusTx,
	cadence cadenceDispatcher,
	followUp followUpDispatcher,
) *InteractionRecorder {
	return &InteractionRecorder{
		writer:              writer,
		telegramMessageRepo: telegramMessageRepo,
		bus:                 bus,
		cadence:             cadence,
		followUp:            followUp,
	}
}

// HandleEvent is the per-event entry point. The caller must own tx; this
// method delegates to ContactService.RecordInteractionTx, marks any
// inline telegram messages processed, and emits interaction.recorded —
// all within that tx (spec §3.4.1).
//
// Returns:
//   - interaction: the persisted row (either freshly-inserted or the
//     existing dedup-hit row). Caller may use the ID for HTTP responses.
//   - postCommit:  non-nil when the write warrants a best-effort follow-up-
//     manager call. Nil on replay (dedup-hit) per plan Decision 8.
//     Callers invoke AFTER the outer tx commits.
//   - error:       wrapped. Caller rolls back tx.
func (r *InteractionRecorder) HandleEvent(ctx context.Context, tx pgx.Tx, env *events.Envelope) (*repository.Interaction, func(context.Context), error) {
	if env == nil {
		return nil, nil, errors.New("consumer: nil envelope")
	}
	if tx == nil {
		return nil, nil, errors.New("consumer: nil tx")
	}

	req, direction, err := r.extractRequest(env)
	if err != nil {
		return nil, nil, fmt.Errorf("extract %s: %w", env.Kind, err)
	}

	// Publisher-resolved ContactID (plan Decision 4). Consumer does no
	// peer-ref → contact lookup; a zero ContactID is a publisher bug.
	if req.ContactID == uuid.Nil {
		logger.Error().
			Str("event_id", env.ID.String()).
			Str("kind", string(env.Kind)).
			Msg("consumer: contact_id unresolved for kind; dropping")
		return nil, nil, fmt.Errorf("consumer: contact_id unresolved for kind %s", env.Kind)
	}

	// Delegate to ContactService.RecordInteractionTx — single source of
	// truth for dedup + insert + cadence updates. publishesEvent=true asks
	// the service to populate the V2 payload snapshot (PrevCadence +
	// CadenceAtEmit) on the returned result so we can attach them to
	// the interaction.recorded event we publish below.
	res, err := r.writer.RecordInteractionTx(ctx, tx, true, req)
	if err != nil {
		return nil, nil, fmt.Errorf("record interaction tx: %w", err)
	}

	// Mark telegram messages processed inside the SAME tx as the interaction
	// insert so telegram_message.interaction_id FK write is atomic with the
	// row it references (plan Decision 10). Runs on BOTH fresh-write and
	// replay paths — today's telegram publisher code always called
	// MarkMessagesProcessed after RecordInteraction, regardless of dedup-hit.
	if env.Kind == events.KindMessageReceived || env.Kind == events.KindMessageSent {
		if msgIDs, extractErr := extractTelegramMessageIDs(env); extractErr == nil && len(msgIDs) > 0 && r.telegramMessageRepo != nil && res.Interaction != nil {
			if markErr := r.telegramMessageRepo.MarkMessagesProcessedTx(ctx, tx, msgIDs, res.Interaction.ID); markErr != nil {
				return nil, nil, fmt.Errorf("mark telegram messages processed: %w", markErr)
			}
		}
	}

	// Replay: skip interaction.recorded emit (spec §3.4.1) and return nil
	// postCommit (today's no-re-apply-on-replay semantics; plan Decision 4).
	if res.IsReplay {
		return res.Interaction, nil, nil
	}

	// Fresh-write: emit interaction.recorded atomically in the same tx.
	// SourceID = interaction.ID so the event table's partial unique index
	// (source, source_id) dedupes any retry that reaches this point.
	// PR 7: payload is V2 — includes PrevCadenceSnapshot + PrevCadenceValue
	// so CadenceUpdater can replay the direct-path's pre-cadence state
	// deterministically (plan Decision 2a).
	recordedPayload, err := marshalRecordedPayload(res.Interaction, direction, req, res.PrevCadence, res.CadenceAtEmit)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal interaction.recorded: %w", err)
	}
	recordedEnv := &events.Envelope{
		Source:     env.Source,
		SourceID:   res.Interaction.ID.String(),
		Kind:       events.KindInteractionRecorded,
		Payload:    recordedPayload,
		ObservedAt: req.OccurredAt,
	}
	if err := r.bus.PublishTx(ctx, tx, recordedEnv); err != nil {
		return nil, nil, fmt.Errorf("publish interaction.recorded: %w", err)
	}

	// Inline-apply cadence synchronously in the SAME tx so the manual
	// UI stays synchronous AND the queued worker for this event_id
	// becomes a durable no-op on re-delivery. The claim row + cadence
	// write commit atomically with the interaction insert; a tx
	// rollback rolls back all three together so a queued re-delivery
	// is safe.
	if r.cadence != nil {
		if cadenceErr := r.cadence.HandleEvent(ctx, tx, recordedEnv); cadenceErr != nil {
			return nil, nil, fmt.Errorf("inline apply cadence: %w", cadenceErr)
		}
	}

	// Inline-apply follow-up under the same atomicity + dedupe rules as
	// cadence. The consumer claims the event via event_consumer_claim so
	// the queued worker for the same event becomes a durable no-op on
	// re-delivery. The returned post-commit closure (non-nil on the
	// refresh branch) carries the Todoist item_update call out of the
	// tx per core.md rule 153.
	var followUpPostCommit func(context.Context)
	if r.followUp != nil {
		pc, followUpErr := r.followUp.HandleEvent(ctx, tx, recordedEnv)
		if followUpErr != nil {
			return nil, nil, fmt.Errorf("inline apply follow-up: %w", followUpErr)
		}
		followUpPostCommit = pc
	}

	// Build the post-commit callback. res.FollowUpFn is nil on the bus
	// path (publishesEvent=true); it's set only when the non-bus wrapper
	// path runs (publishesEvent=false — Todoist completion). The follow-up
	// consumer's own post-commit closure fires on the bus path's
	// refresh branch.
	followUpFn := res.FollowUpFn
	var postCommit func(context.Context)
	if followUpFn != nil || followUpPostCommit != nil {
		postCommit = func(pctx context.Context) {
			if followUpFn != nil {
				followUpFn(pctx)
			}
			if followUpPostCommit != nil {
				followUpPostCommit(pctx)
			}
		}
	}

	return res.Interaction, postCommit, nil
}

// extractRequest dispatches on env.Kind to build a RecordInteractionRequest
// + the effective direction. Direction derivation rules follow plan
// Decision 3 (PR 5).
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
			ContactID:   p.ContactID,
			Source:      repository.InteractionSourceGCal,
			SourceRef:   &ref,
			OccurredAt:  p.OccurredAt,
			Description: p.Title,
			Direction:   repository.InteractionDirectionMutual,
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

// extractTelegramMessageIDs pulls the MessageIDs slice from the envelope
// payload for message.* kinds. Returns empty slice on non-message kinds
// (safe for all callers) and an error on malformed payload JSON.
func extractTelegramMessageIDs(env *events.Envelope) ([]uuid.UUID, error) {
	switch env.Kind {
	case events.KindMessageReceived:
		var p events.MessageReceivedPayload
		if err := events.Unmarshal(env, &p); err != nil {
			return nil, err
		}
		return p.MessageIDs, nil
	case events.KindMessageSent:
		var p events.MessageSentPayload
		if err := events.Unmarshal(env, &p); err != nil {
			return nil, err
		}
		return p.MessageIDs, nil
	}
	return nil, nil
}

// makeTelegramRequest builds the RecordInteractionRequest for message.*
// kinds. Shared by message.received and message.sent extract branches.
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
// PR 7 bumps Version to 2 and adds PrevCadenceSnapshot + PrevCadenceValue
// so CadenceUpdater can replay direct-path's pre-cadence state (plan
// Decision 2a). prev may be nil when the service wrapper was called
// without an eventID (non-event-bus callers); in that case the event
// is emitted WITHOUT the snapshot and downstream consumers treat it as
// a V1-shape payload.
func marshalRecordedPayload(
	interaction *repository.Interaction, direction string, req repository.RecordInteractionRequest,
	prev *repository.ContactCadenceFields, cadenceAtEmit *string,
) (json.RawMessage, error) {
	payload := events.InteractionRecordedPayload{
		Version:          2,
		ContactID:        interaction.ContactID,
		InteractionID:    interaction.ID,
		Direction:        direction,
		OccurredAt:       interaction.OccurredAt,
		Source:           interaction.Source,
		SourceRef:        req.SourceRef,
		PrevCadenceValue: cadenceAtEmit,
	}
	if prev != nil {
		payload.PrevCadenceSnapshot = &events.CadenceFieldsSnapshot{
			LastContacted:  prev.LastContacted,
			LastOutreachAt: prev.LastOutreachAt,
			LastResponseAt: prev.LastResponseAt,
			ContactBy:      prev.ContactBy,
		}
	}
	return events.Marshal(events.KindInteractionRecorded, payload)
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
// After the tx commits, invokes the recorder's returned postCommit
// closure (best-effort follow-up manager invocation; plan Decision 8).
func (w *InteractionRecorderWorker) Work(ctx context.Context, j *river.Job[consumerjobs.InteractionRecorderJobArgs]) error {
	env, err := w.bus.GetEvent(ctx, j.Args.EventID)
	if err != nil {
		return fmt.Errorf("fetch event %s: %w", j.Args.EventID, err)
	}
	var postCommit func(context.Context)
	err = pgx.BeginTxFunc(ctx, w.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		_, pc, handleErr := w.recorder.HandleEvent(ctx, tx, env)
		if handleErr != nil {
			return handleErr
		}
		postCommit = pc
		return nil
	})
	if err != nil {
		return err
	}
	if postCommit != nil {
		postCommit(ctx)
	}
	return nil
}

// Timeout bounds how long a single worker invocation can run. A single
// interaction insert should complete in ~10ms on the Pi; 30s is ample
// headroom for pool saturation + retries (plan Decision 8).
func (*InteractionRecorderWorker) Timeout(*river.Job[consumerjobs.InteractionRecorderJobArgs]) time.Duration {
	return 30 * time.Second
}
