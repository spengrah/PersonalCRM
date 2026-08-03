// Package consumer holds the event-bus consumer services that subscribe
// to events and perform domain writes. See
// .ai/spec/event-bus-foundation.md §3.4.
//
// InteractionRecorder is the sole writer of interaction rows for its
// six input kinds (message.received/sent, calendar.attended,
// task.completed, task.outreach_detected, interaction.manual). The
// write is delegated to ContactService.RecordInteractionTx so dedup
// + cadence-update semantics stay in one place. Sibling consumers in
// this package: CadenceUpdater, FollowUpManager, RematchDispatcher.
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
// to name a mode. "cutover" is the normal operating posture; "off"
// silences publisher-driven paths for emergency rollback. Unlike
// cadence / follow-up modes there is no paired UnsafeAllowOff gate —
// "off" is safe to flip directly (publishers are not sole-writer-gated).
// See EventBusConfig in config/ for the full mode semantics.
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

// stagingProcessor is the source-neutral seam between the consumer and
// per-source staging tables. Satisfied by
// *repository.StagingProcessorRegistry which dispatches by source name
// to the per-source repository's MarkMessagesProcessedTx. Nullable in
// the constructor — non-message kinds bypass it.
//
// sessionRef is the event's source_ref. The underlying SQL scopes the
// update to rows still claimed for that exact session, defending
// against the stale boundary-shift race (spec §3 Race Mechanics).
// Returns rows actually updated so the consumer can log a warning
// when zero rows matched (race detected).
type stagingProcessor interface {
	MarkProcessedTx(ctx context.Context, tx pgx.Tx, source string, messageIDs []uuid.UUID, interactionID uuid.UUID, sessionRef string) (int64, error)
}

// aggregatorReenqueuer dispatches a post-commit aggregator pass for the
// just-processed (source, contactID). Used to drain rows that arrived
// in the Stage 2 → Stage 3 window — those rows are unclaimed by the
// time the consumer commits, and the post-commit hook is what picks
// them up before the next external aggregation trigger.
//
// Implementations live in the consumer package
// (consumer.AggregatorReenqueuerRegistry).
type aggregatorReenqueuer interface {
	Reenqueue(ctx context.Context, env *events.Envelope, contactID uuid.UUID) error
}

// cadenceDispatcher is the subset of *CadenceUpdater the recorder needs
// for the inline-apply path. Unit tests stub this without building a
// real claim store / contact reader.
type cadenceDispatcher interface {
	HandleEvent(ctx context.Context, tx pgx.Tx, env *events.Envelope) error
}

// calendarEventLocker is the narrow dependency that serializes a
// calendar.attended insert against a concurrent decline DELETE on the
// backing calendar_event row. LockExistsByIDTx takes a FOR SHARE lock on
// the row (held until the caller's tx commits) and reports whether it
// exists: false means the row was already deleted (a decline committed
// first), so the recorder skips the attended insert to avoid stranding a
// false interaction. Satisfied by *repository.CalendarEventRepository.
// Unit tests that don't exercise calendar.attended pass a stub returning
// (true, nil).
type calendarEventLocker interface {
	LockExistsByIDTx(ctx context.Context, tx pgx.Tx, id uuid.UUID) (bool, error)
}

// venueResolver is the narrow dependency the recorder uses to resolve the
// shared-container venue node an interaction happened in, so it can set
// interaction.venue_id atomically with the insert. Both methods are
// best-effort: a nil return (no resolvable container) records the interaction
// with a NULL venue_id rather than failing. Satisfied by
// *repository.VenueResolverRegistry (message.*) + the calendar-event 3-tuple
// read (gcal). Nil in tests that don't assert on venues — the recorder skips
// resolution when it's nil.
type venueResolver interface {
	// ResolveMessageVenueTx resolves the venue for a telegram/messages/gchat
	// interaction from its staging-row ids.
	ResolveMessageVenueTx(ctx context.Context, tx pgx.Tx, source string, messageIDs []uuid.UUID) (*uuid.UUID, error)
	// ResolveGCalVenueTx resolves the meeting venue for a gcal interaction from
	// the internal calendar_event id carried in the interaction's source_ref.
	ResolveGCalVenueTx(ctx context.Context, tx pgx.Tx, calendarEventID uuid.UUID) (*uuid.UUID, error)
}

// followUpDispatcher is the subset of *FollowUpManager the recorder
// needs for the inline-apply path. All remote effects leave via
// todoist_task_op jobs enqueued in the caller's tx, so no post-commit
// closure is returned.
type followUpDispatcher interface {
	HandleEvent(ctx context.Context, tx pgx.Tx, env *events.Envelope) error
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
	writer       interactionWriter
	staging      stagingProcessor
	bus          eventBusTx
	cadence      cadenceDispatcher
	followUp     followUpDispatcher
	calendarLock calendarEventLocker
	venue        venueResolver
}

// NewInteractionRecorder builds the consumer. staging may be nil for
// test environments that don't exercise message.* kinds.
// cadence MUST be non-nil in production wiring — the inline cadence
// apply is the seam that closes the queued-worker replay hole for
// manual corrections. followUp is inline-invoked post-publish so the
// manual UI stays synchronous and the queued delivery becomes a
// durable no-op via event_consumer_claim. calendarLock serializes the
// calendar.attended insert against a concurrent decline DELETE on the
// backing calendar_event row; when nil the recorder skips the lock check
// (no calendar race protection — acceptable for tests that don't exercise
// calendar.attended). Tests that don't care about cadence / follow-up pass
// nil stubs.
func NewInteractionRecorder(
	writer interactionWriter,
	staging stagingProcessor,
	bus eventBusTx,
	cadence cadenceDispatcher,
	followUp followUpDispatcher,
	calendarLock calendarEventLocker,
) *InteractionRecorder {
	return &InteractionRecorder{
		writer:       writer,
		staging:      staging,
		bus:          bus,
		cadence:      cadence,
		followUp:     followUp,
		calendarLock: calendarLock,
	}
}

// SetVenueResolver wires the venue resolver used to populate interaction.venue_id
// for message.* and gcal interactions. Optional: when unset the recorder records
// interactions with a NULL venue_id (tests that don't assert on venues leave it
// nil). Production wiring sets it after construction, mirroring the other
// optional-dependency setters on this package's consumers.
func (r *InteractionRecorder) SetVenueResolver(v venueResolver) {
	r.venue = v
}

// HandleEvent is the per-event entry point. The caller must own tx; this
// method delegates to ContactService.RecordInteractionTx, marks any
// inline telegram messages processed, and emits interaction.recorded —
// all within that tx (spec §3.4.1).
//
// Returns:
//   - interaction: the persisted row (either freshly-inserted or the
//     existing dedup-hit row). Caller may use the ID for HTTP responses.
//   - error:       wrapped. Caller rolls back tx.
//
// All effects — cadence, follow-up, and any todoist_task_op enqueue —
// commit in the caller's tx, so there is no post-commit closure.
func (r *InteractionRecorder) HandleEvent(ctx context.Context, tx pgx.Tx, env *events.Envelope) (*repository.Interaction, error) {
	if env == nil {
		return nil, errors.New("consumer: nil envelope")
	}
	if tx == nil {
		return nil, errors.New("consumer: nil tx")
	}

	req, direction, suppressFollowUp, err := r.extractRequest(env)
	if err != nil {
		return nil, fmt.Errorf("extract %s: %w", env.Kind, err)
	}

	// ContactID is resolved by the publisher before PublishTx; the
	// consumer does no peer-ref → contact lookup, so a zero ContactID
	// at this point is a publisher bug.
	if req.ContactID == uuid.Nil {
		logger.Error().
			Str("event_id", env.ID.String()).
			Str("kind", string(env.Kind)).
			Msg("consumer: contact_id unresolved for kind; dropping")
		return nil, fmt.Errorf("consumer: contact_id unresolved for kind %s", env.Kind)
	}

	// calendar.attended: serialize against a concurrent decline DELETE on
	// the backing calendar_event row. A locking FOR SHARE read of the event
	// (by the payload's internal-UUID source_ref) is held until this tx
	// commits, so an interleaving decline DELETE either blocks (we insert,
	// the decline then soft-deletes) or has already committed (the row is
	// gone → we skip the insert, leaving no false interaction). Skipping a
	// past-attended insert when the event was declined is exactly the
	// desired outcome.
	if env.Kind == events.KindCalendarAttended && r.calendarLock != nil && req.SourceRef != nil {
		eventID, parseErr := uuid.Parse(*req.SourceRef)
		if parseErr != nil {
			return nil, fmt.Errorf("calendar.attended: source_ref %q not a calendar_event UUID: %w", *req.SourceRef, parseErr)
		}
		exists, lockErr := r.calendarLock.LockExistsByIDTx(ctx, tx, eventID)
		if lockErr != nil {
			return nil, fmt.Errorf("lock backing calendar_event: %w", lockErr)
		}
		if !exists {
			// Backing calendar_event already deleted (decline won the race).
			// Skip the insert; no interaction.
			return nil, nil
		}
	}

	// Resolve the shared-container venue so it's set atomically with the insert.
	// Best-effort: a resolution failure that isn't a real DB error leaves
	// venue_id NULL (manual/todoist and unresolvable containers) — never fail
	// the interaction over a missing venue. Runs before the dedup-or-insert so a
	// fresh insert carries venue_id; a dedup hit ignores it (the row already has
	// its venue).
	if r.venue != nil {
		venueID, venueErr := r.resolveVenue(ctx, tx, env, &req)
		if venueErr != nil {
			return nil, fmt.Errorf("resolve venue: %w", venueErr)
		}
		req.VenueID = venueID
	}

	// Delegate to ContactService.RecordInteractionTx — single source of
	// truth for dedup + insert + cadence updates. publishesEvent=true asks
	// the service to populate the V2 payload snapshot (PrevCadence +
	// CadenceAtEmit) on the returned result so we can attach them to
	// the interaction.recorded event we publish below.
	res, err := r.writer.RecordInteractionTx(ctx, tx, true, req)
	if err != nil {
		return nil, fmt.Errorf("record interaction tx: %w", err)
	}

	// Mark staging rows processed inside the SAME tx as the
	// interaction insert so the staging.interaction_id FK write is
	// atomic with the row it references. Runs on BOTH fresh-write and
	// replay paths — matches the telegram publisher's pre-cutover
	// behavior of always calling MarkMessagesProcessed after
	// RecordInteraction, regardless of dedup-hit.
	//
	// Dispatches on env.Source so message.* events from any source
	// (telegram today, messages once the Mac daemon ingest writer is
	// live) route to the right registry entry. env.SourceID is the
	// deterministic per-session source_ref;
	// threading it into MarkProcessedTx scopes the SQL update to rows
	// still claimed for THIS session — defends against the stale
	// boundary-shift race (spec §3 Race Mechanics).
	if env.Kind == events.KindMessageReceived || env.Kind == events.KindMessageSent {
		if msgIDs, extractErr := extractMessageIDs(env); extractErr == nil && len(msgIDs) > 0 && r.staging != nil && res.Interaction != nil {
			affected, markErr := r.staging.MarkProcessedTx(ctx, tx, env.Source, msgIDs, res.Interaction.ID, env.SourceID)
			if markErr != nil {
				return nil, fmt.Errorf("mark staging messages processed: %w", markErr)
			}
			// Zero-rows-affected with a non-empty msgIDs list on a
			// FRESH write means the predicate filtered everything out
			// (rows were claimed for a different session under a new
			// computed sourceRef, or already processed by another
			// consumer running in parallel). Without rollback we'd
			// commit a phantom interaction with no staging rows
			// backing it — a duplicate of whatever the other session
			// already produced. Returning an error rolls back the
			// whole tx; River will retry, and by then either the
			// other session has won (we'll dedup on retry) or its
			// claim has aged out (we'll win and the rows will be
			// available again).
			//
			// Replay (res.IsReplay) is a different shape: the rows
			// were already linked to res.Interaction.ID on the
			// original attempt, so `processed_at IS NOT NULL` filters
			// them out on retry — zero affected is expected. Replay
			// short-circuits below before this check is reached.
			if affected == 0 && !res.IsReplay {
				return nil, fmt.Errorf("recorder: staging mark-processed matched zero rows for source=%s source_id=%s (cross-session race; tx rolled back to let other writer win)",
					env.Source, env.SourceID)
			}
		}
	}

	// Replay: skip interaction.recorded emit (spec §3.4.1) so re-delivery
	// doesn't re-fire side effects.
	if res.IsReplay {
		return res.Interaction, nil
	}

	// Fresh-write: emit interaction.recorded atomically in the same tx.
	// SourceID = interaction.ID so the event table's partial unique
	// index (source, source_id) dedupes any retry that reaches this
	// point. Payload is V3 — includes PrevCadenceSnapshot +
	// PrevCadenceValue so CadenceUpdater can replay the pre-cadence
	// state deterministically, plus SuppressFollowUp propagated from
	// task.completed V2 for FollowUpManager.
	recordedPayload, err := marshalRecordedPayload(res.Interaction, direction, req, res.PrevCadence, res.CadenceAtEmit, suppressFollowUp)
	if err != nil {
		return nil, fmt.Errorf("marshal interaction.recorded: %w", err)
	}
	recordedEnv := &events.Envelope{
		Source:     env.Source,
		SourceID:   res.Interaction.ID.String(),
		Kind:       events.KindInteractionRecorded,
		Payload:    recordedPayload,
		ObservedAt: req.OccurredAt,
	}
	if err := r.bus.PublishTx(ctx, tx, recordedEnv); err != nil {
		return nil, fmt.Errorf("publish interaction.recorded: %w", err)
	}

	// Inline-apply cadence synchronously in the SAME tx so the manual
	// UI stays synchronous AND the queued worker for this event_id
	// becomes a durable no-op on re-delivery. The claim row + cadence
	// write commit atomically with the interaction insert; a tx
	// rollback rolls back all three together so a queued re-delivery
	// is safe.
	if r.cadence != nil {
		if cadenceErr := r.cadence.HandleEvent(ctx, tx, recordedEnv); cadenceErr != nil {
			return nil, fmt.Errorf("inline apply cadence: %w", cadenceErr)
		}
	}

	// Inline-apply follow-up under the same atomicity + dedupe rules as
	// cadence. The consumer claims the event via event_consumer_claim so
	// the queued worker for the same event becomes a durable no-op on
	// re-delivery. All remote effects leave via todoist_task_op jobs
	// enqueued in this tx — no post-commit closure.
	if r.followUp != nil {
		if followUpErr := r.followUp.HandleEvent(ctx, tx, recordedEnv); followUpErr != nil {
			return nil, fmt.Errorf("inline apply follow-up: %w", followUpErr)
		}
	}

	// Follow-up already ran inline above (r.followUp.HandleEvent in this
	// tx); no post-commit work remains.
	return res.Interaction, nil
}

// resolveVenue resolves the shared-container venue node id for the interaction
// being recorded. message.received/sent (telegram/messages/gchat) resolve from
// the staging-row ids in the payload; calendar.attended (gcal) resolves from the
// internal calendar_event id carried in source_ref. Other kinds (task.*,
// interaction.manual) have no shared container → nil. Returns nil (not an error)
// whenever the container can't be resolved so the recorder records with a NULL
// venue_id.
func (r *InteractionRecorder) resolveVenue(ctx context.Context, tx pgx.Tx, env *events.Envelope, req *repository.RecordInteractionRequest) (*uuid.UUID, error) {
	switch env.Kind {
	case events.KindMessageReceived, events.KindMessageSent:
		msgIDs, err := extractMessageIDs(env)
		if err != nil {
			return nil, err
		}
		return r.venue.ResolveMessageVenueTx(ctx, tx, env.Source, msgIDs)
	case events.KindCalendarAttended:
		if req.SourceRef == nil {
			return nil, nil
		}
		eventID, parseErr := uuid.Parse(*req.SourceRef)
		if parseErr != nil {
			// source_ref isn't a calendar_event UUID — already errored above
			// for the lock path when calendarLock is set; here just skip venue.
			return nil, nil
		}
		return r.venue.ResolveGCalVenueTx(ctx, tx, eventID)
	default:
		return nil, nil
	}
}

// extractRequest dispatches on env.Kind to build a RecordInteractionRequest
// + the effective direction + the SuppressFollowUp flag. Per-kind direction
// derivation: message.* defaults to the kind's natural direction
// (inbound / outbound); calendar is mutual; task.completed defaults to
// mutual; task.outreach_detected is always outbound; interaction.manual
// defaults to mutual. Callers may override direction via the payload's
// Direction field where applicable. SuppressFollowUp is non-zero only for
// task.completed (V2+) where the publisher set it to true (kind=send).
func (r *InteractionRecorder) extractRequest(env *events.Envelope) (repository.RecordInteractionRequest, string, bool, error) {
	switch env.Kind {
	case events.KindMessageReceived:
		var p events.MessageReceivedPayload
		if err := events.Unmarshal(env, &p); err != nil {
			return repository.RecordInteractionRequest{}, "", false, err
		}
		if p.ExternalMessageID == "" {
			return repository.RecordInteractionRequest{}, "", false, errors.New("message.received: empty external_message_id (source_ref required)")
		}
		direction := p.Direction
		if direction == "" {
			direction = repository.InteractionDirectionInbound
		}
		req, err := makeMessageRequest(env.Source, p.ContactID, p.ExternalMessageID, p.MessageAt, p.Description, direction)
		if err != nil {
			return repository.RecordInteractionRequest{}, "", false, err
		}
		return req, direction, false, nil

	case events.KindMessageSent:
		var p events.MessageSentPayload
		if err := events.Unmarshal(env, &p); err != nil {
			return repository.RecordInteractionRequest{}, "", false, err
		}
		if p.ExternalMessageID == "" {
			return repository.RecordInteractionRequest{}, "", false, errors.New("message.sent: empty external_message_id (source_ref required)")
		}
		direction := p.Direction
		if direction == "" {
			direction = repository.InteractionDirectionOutbound
		}
		req, err := makeMessageRequest(env.Source, p.ContactID, p.ExternalMessageID, p.MessageAt, p.Description, direction)
		if err != nil {
			return repository.RecordInteractionRequest{}, "", false, err
		}
		return req, direction, false, nil

	case events.KindCalendarAttended:
		var p events.CalendarAttendedPayload
		if err := events.Unmarshal(env, &p); err != nil {
			return repository.RecordInteractionRequest{}, "", false, err
		}
		if p.EventID == "" {
			return repository.RecordInteractionRequest{}, "", false, errors.New("calendar.attended: empty event_id (source_ref required)")
		}
		ref := p.EventID
		return repository.RecordInteractionRequest{
			ContactID:   p.ContactID,
			Source:      repository.InteractionSourceGCal,
			SourceRef:   &ref,
			OccurredAt:  p.OccurredAt,
			Description: p.Title,
			Direction:   repository.InteractionDirectionMutual,
		}, repository.InteractionDirectionMutual, false, nil

	case events.KindTaskCompleted:
		var p events.TaskCompletedPayload
		if err := events.Unmarshal(env, &p); err != nil {
			return repository.RecordInteractionRequest{}, "", false, err
		}
		if p.TaskID == "" {
			return repository.RecordInteractionRequest{}, "", false, errors.New("task.completed: empty task_id (source_ref required)")
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
		}, direction, p.SuppressFollowUp, nil

	case events.KindTaskOutreachDetected:
		var p events.TaskOutreachDetectedPayload
		if err := events.Unmarshal(env, &p); err != nil {
			return repository.RecordInteractionRequest{}, "", false, err
		}
		if p.TaskID == "" {
			return repository.RecordInteractionRequest{}, "", false, errors.New("task.outreach_detected: empty task_id (source_ref required)")
		}
		ref := p.TaskID
		return repository.RecordInteractionRequest{
			ContactID:  p.ContactID,
			Source:     repository.InteractionSourceTodoist,
			SourceRef:  &ref,
			OccurredAt: p.DetectedAt,
			Direction:  repository.InteractionDirectionOutbound,
		}, repository.InteractionDirectionOutbound, false, nil

	case events.KindInteractionManual:
		var p events.InteractionManualPayload
		if err := events.Unmarshal(env, &p); err != nil {
			return repository.RecordInteractionRequest{}, "", false, err
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
		}, direction, false, nil
	}

	return repository.RecordInteractionRequest{}, "", false, fmt.Errorf("unsupported kind %q", env.Kind)
}

// extractMessageIDs pulls the MessageIDs slice from the envelope
// payload for message.* kinds. Returns empty slice on non-message kinds
// (safe for all callers) and an error on malformed payload JSON.
// Source-neutral: the payload's MessageIDs field is source-agnostic.
func extractMessageIDs(env *events.Envelope) ([]uuid.UUID, error) {
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

// messageInteractionSources is the allowlist of sources permitted to flow
// through the message.* create path. It is defense-in-depth — the CHECK
// constraint on interaction.source is the durable contract, but this catches a
// bad publisher before the DB write attempts. Each chat/messaging source whose
// aggregation engine publishes KindMessageReceived/KindMessageSent must appear
// here (telegram, messages, gchat, whatsapp).
var messageInteractionSources = map[string]struct{}{
	repository.InteractionSourceTelegram: {},
	repository.InteractionSourceMessages: {},
	repository.InteractionSourceGChat:    {},
	repository.InteractionSourceWhatsApp: {},
}

// makeMessageRequest builds the RecordInteractionRequest for message.*
// kinds. Shared by message.received and message.sent extract branches.
// `source` is propagated from env.Source so a `source="messages"` event
// produces a `source="messages"` interaction row (P0 invariant: the
// message event's source name flows end-to-end).
func makeMessageRequest(source string, contactID *uuid.UUID, externalMessageID string, messageAt time.Time, description *string, direction string) (repository.RecordInteractionRequest, error) {
	if _, ok := messageInteractionSources[source]; !ok {
		return repository.RecordInteractionRequest{}, fmt.Errorf("unsupported message source %q (allowed: telegram, messages, gchat, whatsapp)", source)
	}
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
		Source:      source,
		SourceRef:   ref,
		OccurredAt:  messageAt,
		Description: description,
		Direction:   direction,
	}, nil
}

// marshalRecordedPayload builds the JSON payload for the
// interaction.recorded derived event emitted after a successful insert.
// Payload is V3 — it includes PrevCadenceSnapshot + PrevCadenceValue so
// CadenceUpdater can deterministically replay the pre-cadence state, plus
// SuppressFollowUp propagated from upstream task.completed (V2+) so
// FollowUpManager can short-circuit on kind=send completions.
// prev may be nil when the service wrapper was called without an
// eventID (non-event-bus callers); in that case the snapshot fields stay
// nil and downstream consumers fall back to a live re-read.
func marshalRecordedPayload(
	interaction *repository.Interaction, direction string, req repository.RecordInteractionRequest,
	prev *repository.ContactCadenceFields, cadenceAtEmit *string, suppressFollowUp bool,
) (json.RawMessage, error) {
	payload := events.InteractionRecordedPayload{
		Version:          3,
		ContactID:        interaction.ContactID,
		InteractionID:    interaction.ID,
		Direction:        direction,
		OccurredAt:       interaction.OccurredAt,
		Source:           interaction.Source,
		SourceRef:        req.SourceRef,
		PrevCadenceValue: cadenceAtEmit,
		SuppressFollowUp: suppressFollowUp,
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
//
// reenqueuer is an optional post-commit hook for the per-source
// aggregator. After a fresh interaction commits, the worker calls
// reenqueuer.Reenqueue(ctx, env, contactID) so rows that arrived in
// the Stage 2 → Stage 3 window are picked up before the next external
// aggregation trigger. nil disables the hook (test mode).
type InteractionRecorderWorker struct {
	river.WorkerDefaults[consumerjobs.InteractionRecorderJobArgs]
	bus        eventBusTx
	pool       *pgxpool.Pool
	recorder   *InteractionRecorder
	reenqueuer aggregatorReenqueuer
}

// NewInteractionRecorderWorker wires the worker to the concrete bus, the
// application pgxpool, the consumer instance, and (optionally) the
// post-commit aggregator reenqueuer. reenqueuer may be nil — tests and
// modes that don't run an aggregator pass nil safely.
func NewInteractionRecorderWorker(bus eventBusTx, pool *pgxpool.Pool, recorder *InteractionRecorder, reenqueuer aggregatorReenqueuer) *InteractionRecorderWorker {
	return &InteractionRecorderWorker{
		bus:        bus,
		pool:       pool,
		recorder:   recorder,
		reenqueuer: reenqueuer,
	}
}

// Work implements river.Worker. Fetches the event envelope by id, opens a
// fresh tx, and invokes HandleEvent. On error river will retry per
// MaxAttempts (set to 5 via InsertOpts in events.consumerJobsForKind).
// After the tx commits, runs the per-source aggregator reenqueue
// (best-effort; logged warn on failure, does NOT roll back the
// interaction). HandleEvent itself has no post-commit closure — all its
// effects commit in the tx.
func (w *InteractionRecorderWorker) Work(ctx context.Context, j *river.Job[consumerjobs.InteractionRecorderJobArgs]) error {
	env, err := w.bus.GetEvent(ctx, j.Args.EventID)
	if err != nil {
		return fmt.Errorf("fetch event %s: %w", j.Args.EventID, err)
	}
	var interactionRow *repository.Interaction
	err = pgx.BeginTxFunc(ctx, w.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		interaction, handleErr := w.recorder.HandleEvent(ctx, tx, env)
		if handleErr != nil {
			return handleErr
		}
		interactionRow = interaction
		return nil
	})
	if err != nil {
		return err
	}
	// Post-commit aggregator reenqueue. Best-effort: a failure here
	// does NOT roll back the interaction (already committed); the
	// stale-claim recovery path is the durable backstop.
	if w.reenqueuer != nil && interactionRow != nil &&
		(env.Kind == events.KindMessageReceived || env.Kind == events.KindMessageSent) {
		if rqErr := w.reenqueuer.Reenqueue(ctx, env, interactionRow.ContactID); rqErr != nil {
			logger.Warn().Err(rqErr).
				Str("source", env.Source).
				Str("contact_id", interactionRow.ContactID.String()).
				Msg("recorder: aggregator re-enqueue failed; relying on stale-claim recovery")
		}
	}
	return nil
}

// Timeout bounds how long a single worker invocation can run. A single
// interaction insert should complete in ~10ms on the Pi; 30s is ample
// headroom for pool saturation + retries.
func (*InteractionRecorderWorker) Timeout(*river.Job[consumerjobs.InteractionRecorderJobArgs]) time.Duration {
	return 30 * time.Second
}
