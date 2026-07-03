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

// commsLocator is the subset of *repository.CommsMessageRepository the
// email consumer depends on: locate the content row by natural key inside
// the worker tx, and link it to the derived interaction. Interface defined
// here (consumer-side pattern) so unit tests can stub without a DB.
type commsLocator interface {
	GetMessageTx(ctx context.Context, tx pgx.Tx, source, externalID string, contactID uuid.UUID) (*repository.CommsMessage, error)
	MarkProcessedTx(ctx context.Context, tx pgx.Tx, messageIDs []uuid.UUID, interactionID uuid.UUID, sessionRef string) (int64, error)
}

// emailInteractionFinder is the subset of *repository.InteractionRepository
// the email consumer depends on for the aggregation branch: take the
// per-source_ref serialization lock, then find the existing aggregate.
type emailInteractionFinder interface {
	AcquireSourceRefLockTx(ctx context.Context, tx pgx.Tx, sourceRef string) error
	FindBySourceRefTx(ctx context.Context, tx pgx.Tx, contactID uuid.UUID, source, sourceRef string) (*repository.Interaction, error)
}

// emailAggregator is the subset of *service.ContactService the email
// consumer depends on for the found-branch aggregation primitives. Both
// methods write the supplied occurred_at unconditionally + apply cadence
// and follow-up inline in the caller's tx + publish nothing.
type emailAggregator interface {
	PromoteInteractionToMutualTx(ctx context.Context, tx pgx.Tx, interactionID, contactID uuid.UUID, replyAt time.Time) error
	ExtendInteractionTx(ctx context.Context, tx pgx.Tx, interactionID, contactID uuid.UUID, direction string, occurredAt time.Time, description *string) error
}

// emailVenueResolver resolves the email-thread venue for a freshly-created email
// interaction from the thread container key. Best-effort: an empty thread or a
// resolution error leaves venue_id NULL. Satisfied by
// *repository.VenueRepository.ResolveVenueForInteraction. Optional (nil in tests
// that don't assert on venues).
type emailVenueResolver interface {
	ResolveVenueForInteraction(ctx context.Context, tx pgx.Tx, source, kind, containerKey, title string) (uuid.UUID, error)
}

// EmailInteractionConsumer is the event-bus consumer for email.received /
// email.sent. It derives a per-(contact, thread, local-calendar-day)
// aggregated interaction from the lightweight email event + its
// comms_message content row, in one tx (spec §4.2 / §5.2):
//
//  1. Take a per-source_ref advisory lock so same-key jobs serialize (the
//     forward-only occurred_at guard is a read-compute-write and must be
//     atomic per aggregation key).
//  2. Locate the comms_message by natural key inside the tx.
//  3. Branch on FindBySourceRefTx(contact, "email", source_ref):
//     - not found → create the interaction AND publish interaction.recorded
//     (so CadenceUpdater + FollowUpManager run — cadence delivery path #1),
//     reusing the InteractionRecorder's package-private create sequence.
//     - found → extend (same direction) or promote-to-mutual
//     (direction differs), both of which apply cadence inline and publish
//     nothing (cadence delivery path #2).
//  4. Link the content row via MarkProcessedTx on every branch.
//
// The Gmail provider publishes email.received/email.sent in production, so this
// consumer derives the corresponding interaction from each such event.
type EmailInteractionConsumer struct {
	writer       interactionWriter
	comms        commsLocator
	interactions emailInteractionFinder
	aggregator   emailAggregator
	bus          eventBusTx
	cadence      cadenceDispatcher
	followUp     followUpDispatcher
	venue        emailVenueResolver
}

// NewEmailInteractionConsumer builds the consumer with narrow interfaces so
// unit tests can stub them. Production wires the concrete ContactService
// (as both interactionWriter and emailAggregator), CommsMessageRepository,
// InteractionRepository, event Bus, CadenceUpdater, and FollowUpManager.
func NewEmailInteractionConsumer(
	writer interactionWriter,
	comms commsLocator,
	interactions emailInteractionFinder,
	aggregator emailAggregator,
	bus eventBusTx,
	cadence cadenceDispatcher,
	followUp followUpDispatcher,
) *EmailInteractionConsumer {
	return &EmailInteractionConsumer{
		writer:       writer,
		comms:        comms,
		interactions: interactions,
		aggregator:   aggregator,
		bus:          bus,
		cadence:      cadence,
		followUp:     followUp,
	}
}

// SetVenueResolver wires the resolver that populates interaction.venue_id with
// the email-thread venue on the create branch. Optional: when unset the consumer
// records email interactions with a NULL venue_id.
func (c *EmailInteractionConsumer) SetVenueResolver(v emailVenueResolver) {
	c.venue = v
}

// HandleEvent processes an email.received / email.sent envelope inside the
// caller's tx. All effects — cadence, follow-up, and any todoist_task_op
// enqueue — happen in that tx, so there is no post-commit work; returns an
// error (caller rolls back) only. See the EmailInteractionConsumer doc
// comment for the branch contract.
func (c *EmailInteractionConsumer) HandleEvent(ctx context.Context, tx pgx.Tx, env *events.Envelope) error {
	if env == nil {
		return errors.New("email_interaction: nil envelope")
	}
	if tx == nil {
		return errors.New("email_interaction: nil tx")
	}
	if env.Kind != events.KindEmailReceived && env.Kind != events.KindEmailSent {
		return fmt.Errorf("email_interaction: unsupported kind %q", env.Kind)
	}

	var p events.EmailEventPayload
	if err := events.Unmarshal(env, &p); err != nil {
		return fmt.Errorf("unmarshal email payload: %w", err)
	}

	// Publisher-bug guards (mirroring CalendarDeclineHandler). The publisher
	// resolves ContactID before PublishTx; a zero value here is a bug, not a
	// legitimate state. ExternalID is required to locate the content row.
	// Direction is constrained to {inbound, outbound} (the kind already
	// implies it, but defend against a malformed publisher). SentAt anchors
	// occurred_at + the forward-only compare.
	if p.ContactID == uuid.Nil {
		return fmt.Errorf("email_interaction: empty contact_id (event %s)", env.ID)
	}
	if p.ExternalID == "" {
		return fmt.Errorf("email_interaction: empty external_id (event %s)", env.ID)
	}
	if p.Direction != repository.InteractionDirectionInbound && p.Direction != repository.InteractionDirectionOutbound {
		return fmt.Errorf("email_interaction: direction must be %q or %q, got %q (event %s)",
			repository.InteractionDirectionInbound, repository.InteractionDirectionOutbound, p.Direction, env.ID)
	}
	if p.SentAt.IsZero() {
		return fmt.Errorf("email_interaction: zero sent_at (event %s)", env.ID)
	}

	// source_ref = "<contact_uuid>:<thread_id>:<local_day>". Built from the
	// provider-computed LocalDay (already in time.Local at publish time),
	// NOT re-derived from SentAt — the durable event row is a complete
	// hand-off (spec §5.2). An empty ThreadID still forms an
	// internally-consistent "<uuid>::<day>" key, unique per contact/day;
	// Gmail always sets threadId, so this is defensive only.
	sourceRef := p.ContactID.String() + ":" + p.ThreadID + ":" + p.LocalDay

	// Per-source_ref serialization lock, taken BEFORE the read
	// so the whole find → branch → write (occurred_at) → mark is atomic per
	// aggregation key. A second same-key job blocks here until the first
	// commits, then sees the committed aggregate on its FindBySourceRefTx and
	// takes the found branch — so the forward-only occurred_at guard never
	// regresses under concurrency.
	if err := c.interactions.AcquireSourceRefLockTx(ctx, tx, sourceRef); err != nil {
		return fmt.Errorf("acquire source_ref lock: %w", err)
	}

	// Locate the content row inside the tx. A miss is an
	// anomaly — publish-before-mutate commits the content row in the SAME
	// provider tx as the event, so by the time this job runs the row is
	// committed and visible. ErrNotFound therefore means a content-row
	// hard-delete (test cleanup) or DB inconsistency. We return an error so
	// River retries (a transient race self-heals; a permanent miss discards
	// the job after MaxAttempts, the correct loud failure — we do NOT
	// silently drop an interaction the event says should exist). Contrast
	// CalendarDeclineHandler, which treats ErrNotFound as benign because a
	// future-decline legitimately has no interaction yet; email has no such
	// legitimate-miss case.
	commsMsg, err := c.comms.GetMessageTx(ctx, tx, repository.InteractionSourceEmail, p.ExternalID, p.ContactID)
	if err != nil {
		return fmt.Errorf("locate comms_message for email event %s: %w", env.ID, err)
	}

	found, err := c.interactions.FindBySourceRefTx(ctx, tx, p.ContactID, repository.InteractionSourceEmail, sourceRef)
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		return fmt.Errorf("find email interaction by source_ref: %w", err)
	}

	var interactionID uuid.UUID

	if errors.Is(err, db.ErrNotFound) {
		// Create branch: reuse the InteractionRecorder's
		// package-private create sequence so the email create path is
		// indistinguishable from any other recorded interaction.
		ref := sourceRef
		req := repository.RecordInteractionRequest{
			ContactID:   p.ContactID,
			Source:      repository.InteractionSourceEmail,
			SourceRef:   &ref,
			OccurredAt:  p.SentAt,
			Description: p.Subject,
			Direction:   p.Direction,
		}
		// Resolve the email-thread venue from the content row's thread_id, set
		// atomically with the insert. An empty/absent thread leaves venue_id NULL
		// (no shared container); a real DB error during resolution propagates and
		// rolls back the interaction (the recorder retries).
		if c.venue != nil && commsMsg.ThreadID != nil && *commsMsg.ThreadID != "" {
			venueID, venueErr := c.venue.ResolveVenueForInteraction(
				ctx, tx, repository.InteractionSourceEmail, repository.VenueKindEmailThread, *commsMsg.ThreadID, "")
			if venueErr != nil {
				return fmt.Errorf("resolve email venue: %w", venueErr)
			}
			req.VenueID = &venueID
		}
		res, recordErr := c.writer.RecordInteractionTx(ctx, tx, true, req)
		if recordErr != nil {
			return fmt.Errorf("record email interaction: %w", recordErr)
		}

		if res.IsReplay {
			// A concurrent same-key job created the aggregate between our
			// outer FindBySourceRefTx (not-found) and RecordInteractionTx's
			// own internal source_ref dedup. With the advisory lock this
			// cannot actually happen (the second job blocks until the first
			// commits, so its outer find already sees the row). The
			// fall-through is the correctness backstop that keeps the create
			// branch sound even if the lock were removed: treat res.Interaction
			// as the found row and apply this message's extend/promote +
			// forward-only timestamp rather than silently marking it processed
			// (which would be a lost update). No second interaction.recorded
			// is published; the found path publishes nothing.
			if aggErr := c.aggregate(ctx, tx, &p, res.Interaction); aggErr != nil {
				return aggErr
			}
			interactionID = res.Interaction.ID
		} else {
			// Fresh write: emit interaction.recorded atomically + inline-apply
			// cadence + follow-up, exactly as InteractionRecorder does. The
			// fresh branch's remote effects leave via op jobs enqueued in this
			// tx, so it contributes no post-commit closure.
			if emitErr := c.emitRecorded(ctx, tx, env, &p, res, req); emitErr != nil {
				return emitErr
			}
			interactionID = res.Interaction.ID
		}
	} else {
		// Found branch: extend (same direction) or promote (direction
		// differs). Both apply cadence + follow-up inline and publish nothing.
		if aggErr := c.aggregate(ctx, tx, &p, found); aggErr != nil {
			return aggErr
		}
		interactionID = found.ID
	}

	// Link the content row to the interaction it rolled into, on every
	// branch. Zero affected means this message's own row was
	// already processed on a prior run (genuine re-delivery) — benign under
	// the tx-bound read + per-key lock, so we don't error on it.
	if _, err := c.comms.MarkProcessedTx(ctx, tx, []uuid.UUID{commsMsg.ID}, interactionID, sourceRef); err != nil {
		return fmt.Errorf("mark comms_message processed: %w", err)
	}

	// All follow-up effects (and their op enqueues) ran inline in this tx —
	// no post-commit work.
	return nil
}

// aggregate applies the found-branch extend/promote to an existing
// interaction with the forward-only timestamp guard. Used by
// both the genuine found branch and the create branch's res.IsReplay
// fall-through.
func (c *EmailInteractionConsumer) aggregate(
	ctx context.Context, tx pgx.Tx, p *events.EmailEventPayload, found *repository.Interaction,
) error {
	// Forward-only: only advance occurred_at when SentAt is strictly later
	// than the stored value. Equal needs no write; an earlier (out-of-order
	// backfill) SentAt holds the stored value so occurred_at never moves
	// backward. found.OccurredAt is already UTC (normalized by
	// convertDbInteraction); compare in UTC.
	ts := found.OccurredAt
	if p.SentAt.UTC().After(found.OccurredAt) {
		ts = p.SentAt
	}

	if p.Direction != found.Direction {
		// Direction differs → promote to mutual. Promote flips direction
		// even when ts == found.OccurredAt (the held value), so an
		// out-of-order mixed-direction backfill still promotes without
		// moving occurred_at backward.
		if err := c.aggregator.PromoteInteractionToMutualTx(ctx, tx, found.ID, p.ContactID, ts); err != nil {
			return fmt.Errorf("promote email interaction to mutual: %w", err)
		}
		return nil
	}

	if err := c.aggregator.ExtendInteractionTx(ctx, tx, found.ID, p.ContactID, p.Direction, ts, p.Subject); err != nil {
		return fmt.Errorf("extend email interaction: %w", err)
	}
	return nil
}

// emitRecorded runs the InteractionRecorder's fresh-write post-record
// sequence for the email create branch: marshal the V3 interaction.recorded
// payload, publish it in the same tx (so CadenceUpdater + FollowUpManager
// get enqueued — cadence delivery path #1), then inline-apply cadence +
// follow-up (the inline apply wins the durable event claim, so the async
// workers no-op; the recorder pattern reused verbatim). All remote follow-
// up effects leave via todoist_task_op jobs enqueued in this tx, so there
// is no post-commit closure to return.
func (c *EmailInteractionConsumer) emitRecorded(
	ctx context.Context, tx pgx.Tx, env *events.Envelope, p *events.EmailEventPayload,
	res *repository.RecordInteractionResult, req repository.RecordInteractionRequest,
) error {
	// Email never suppresses follow-ups (only kind=send task completions do).
	recordedPayload, err := marshalRecordedPayload(res.Interaction, p.Direction, req, res.PrevCadence, res.CadenceAtEmit, false)
	if err != nil {
		return fmt.Errorf("marshal interaction.recorded: %w", err)
	}
	recordedEnv := &events.Envelope{
		Source:     repository.InteractionSourceEmail,
		SourceID:   res.Interaction.ID.String(),
		Kind:       events.KindInteractionRecorded,
		Payload:    recordedPayload,
		ObservedAt: req.OccurredAt,
	}
	if err := c.bus.PublishTx(ctx, tx, recordedEnv); err != nil {
		return fmt.Errorf("publish interaction.recorded: %w", err)
	}
	if c.cadence != nil {
		if cadenceErr := c.cadence.HandleEvent(ctx, tx, recordedEnv); cadenceErr != nil {
			return fmt.Errorf("inline apply cadence: %w", cadenceErr)
		}
	}
	if c.followUp != nil {
		if followUpErr := c.followUp.HandleEvent(ctx, tx, recordedEnv); followUpErr != nil {
			return fmt.Errorf("inline apply follow-up: %w", followUpErr)
		}
	}
	return nil
}

// --------------------------------------------------------------------------
// River worker wrapper. Mirrors InteractionRecorderWorker: fetch the
// envelope by id, open a fresh tx, call HandleEvent — all effects commit
// in that tx.
// --------------------------------------------------------------------------

// EmailInteractionConsumerWorker is the river worker that dispatches queued
// EmailInteractionConsumerJobArgs to EmailInteractionConsumer.HandleEvent.
type EmailInteractionConsumerWorker struct {
	river.WorkerDefaults[consumerjobs.EmailInteractionConsumerJobArgs]
	bus      eventBusTx
	pool     *pgxpool.Pool
	consumer *EmailInteractionConsumer
}

// NewEmailInteractionConsumerWorker wires the worker to the concrete bus,
// the application pgxpool, and the consumer instance.
func NewEmailInteractionConsumerWorker(bus eventBusTx, pool *pgxpool.Pool, consumer *EmailInteractionConsumer) *EmailInteractionConsumerWorker {
	return &EmailInteractionConsumerWorker{bus: bus, pool: pool, consumer: consumer}
}

// Work implements river.Worker. Fetches the event envelope by id, opens a
// fresh tx, and invokes HandleEvent — all effects commit in that tx, so
// there is no post-commit work. On error River retries per MaxAttempts (5
// from the InsertOpts in events.consumerJobsForKind).
func (w *EmailInteractionConsumerWorker) Work(ctx context.Context, j *river.Job[consumerjobs.EmailInteractionConsumerJobArgs]) error {
	env, err := w.bus.GetEvent(ctx, j.Args.EventID)
	if err != nil {
		return fmt.Errorf("fetch event %s: %w", j.Args.EventID, err)
	}
	return pgx.BeginTxFunc(ctx, w.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		return w.consumer.HandleEvent(ctx, tx, env)
	})
}

// Timeout bounds a single email-consumer run. A lock + read + single
// interaction insert/extend completes in ~10ms on the Pi; 30s is ample
// headroom for pool saturation + retries.
func (*EmailInteractionConsumerWorker) Timeout(*river.Job[consumerjobs.EmailInteractionConsumerJobArgs]) time.Duration {
	return 30 * time.Second
}
