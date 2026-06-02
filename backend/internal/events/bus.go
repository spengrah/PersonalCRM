package events

import (
	"context"
	"errors"
	"fmt"

	"personal-crm/backend/internal/consumer/consumerjobs"
	"personal-crm/backend/internal/db"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
)

// EventRepository is the persistence contract used by Bus. Defined here
// (consumer-side interface pattern) so the events package has no dependency
// on the repository package. The repository.EventRepository concrete type
// satisfies this interface by structural typing.
type EventRepository interface {
	InsertEvent(ctx context.Context, tx pgx.Tx, env *Envelope) error
	GetEvent(ctx context.Context, id uuid.UUID) (*Envelope, error)
	FindEventBySource(ctx context.Context, source, sourceID string) (*Envelope, error)
}

// consumerJob bundles a river JobArgs with its per-insert options. A
// single Kind may map to multiple consumer jobs.
type consumerJob struct {
	Args river.JobArgs
	Opts *river.InsertOpts
}

// consumerJobsForKind is the static registry mapping a Kind to the set of
// river jobs to enqueue atomically alongside its event row.
//
// The envelope is passed in (not just the event id) so kinds whose
// JobArgs carries additional payload-derived fields — currently
// contact_methods.added, whose RematchDispatcherJobArgs includes
// ContactID and RematchJobID for river UniqueOpts{ByArgs} — can decode
// the payload at routing time. Payload decode failures surface as a
// routing error so PublishTx can fail the caller's tx rather than land
// a stranded event row with no consumer (spec §4 atomicity).
//
// Routing rules:
//
//   - 5 async-publisher kinds (message.received/sent, calendar.attended,
//     task.completed, task.outreach_detected) → InteractionRecorder worker
//     with MaxAttempts=5.
//   - interaction.recorded → two jobs, one CadenceUpdater and one
//     FollowUpManager, each with MaxAttempts=5.
//   - contact_methods.added → RematchDispatcher worker. UniqueOpts
//     {ByArgs: ["ContactID", "RematchJobID"], ByState: scheduled|
//     available|running|retryable} dedupes same-mutation publisher
//     retries at enqueue time.
//   - interaction.manual: inline-invoked by the manual UI handler; no
//     async consumer.
//   - calendar.declined → CalendarDeclineHandler worker with
//     MaxAttempts=5 (soft-deletes the derived gcal interaction + recomputes
//     the contact's date columns when a calendar event is declined/
//     cancelled/user-removed upstream).
//   - email.received / email.sent → EmailInteractionConsumer worker with
//     MaxAttempts=5 (derives a per-(contact, thread, local-day) aggregated
//     email interaction). Both kinds share one case (one job each). The
//     consumer is production-inert until the Gmail provider is registered +
//     enabled (phase 5) — no production code publishes email.* events.
//   - task.skipped: no consumer yet wired.
func consumerJobsForKind(env *Envelope) ([]consumerJob, error) {
	switch env.Kind {
	case KindMessageReceived, KindMessageSent, KindCalendarAttended,
		KindTaskCompleted, KindTaskOutreachDetected:
		return []consumerJob{{
			Args: consumerjobs.InteractionRecorderJobArgs{EventID: env.ID},
			Opts: &river.InsertOpts{MaxAttempts: 5},
		}}, nil
	case KindCalendarDeclined:
		return []consumerJob{{
			Args: consumerjobs.CalendarDeclineHandlerJobArgs{EventID: env.ID},
			Opts: &river.InsertOpts{MaxAttempts: 5},
		}}, nil
	case KindEmailReceived, KindEmailSent:
		return []consumerJob{{
			Args: consumerjobs.EmailInteractionConsumerJobArgs{EventID: env.ID},
			Opts: &river.InsertOpts{MaxAttempts: 5},
		}}, nil
	case KindInteractionRecorded:
		return []consumerJob{
			{
				Args: consumerjobs.CadenceUpdaterJobArgs{EventID: env.ID},
				Opts: &river.InsertOpts{MaxAttempts: 5},
			},
			{
				Args: consumerjobs.FollowUpManagerJobArgs{EventID: env.ID},
				Opts: &river.InsertOpts{MaxAttempts: 5},
			},
		}, nil
	case KindContactMethodsAdded:
		var p ContactMethodsAddedPayload
		if err := Unmarshal(env, &p); err != nil {
			return nil, fmt.Errorf("decode contact_methods.added: %w", err)
		}
		if p.ContactID == uuid.Nil {
			return nil, fmt.Errorf("contact_methods.added: empty contact_id")
		}
		if p.RematchJobID == uuid.Nil {
			return nil, fmt.Errorf("contact_methods.added: empty rematch_job_id")
		}
		return []consumerJob{{
			Args: consumerjobs.RematchDispatcherJobArgs{
				EventID:      env.ID,
				ContactID:    p.ContactID,
				RematchJobID: p.RematchJobID,
			},
			Opts: &river.InsertOpts{
				MaxAttempts: 3,
				UniqueOpts: river.UniqueOpts{
					// ByArgs: true combined with the river:"unique" tags on
					// ContactID + RematchJobID in the JobArgs struct tells
					// river to hash only those two fields (EventID is NOT
					// tagged, so retries with different event.id but same
					// (ContactID, RematchJobID) dedupe to one river job).
					ByArgs: true,
					// ByState is nil here to accept river's default set
					// (Pending/Scheduled/Available/Running/Retryable). A
					// partial ByState list must include Pending or river
					// rejects the InsertOpts at InsertTx time. Default
					// covers our "not yet terminal" window as tightly as
					// a custom list would.
				},
			},
		}}, nil
	}
	return nil, nil
}

// Bus publishes typed events to the append-only event log and enqueues
// consumer jobs atomically via river.InsertTx. See spec §3.3.
type Bus struct {
	pool        *pgxpool.Pool
	riverClient *river.Client[pgx.Tx]
	eventRepo   EventRepository
}

// NewBus builds a Bus. pool is used by Publish to open its own tx; caller
// passes the same pgxpool it uses for repositories.
func NewBus(pool *pgxpool.Pool, riverClient *river.Client[pgx.Tx], eventRepo EventRepository) *Bus {
	return &Bus{pool: pool, riverClient: riverClient, eventRepo: eventRepo}
}

// Publish opens a new pgx.Tx on the configured pool, calls PublishTx, and
// commits. Use when the caller is NOT already in a transaction.
func (b *Bus) Publish(ctx context.Context, env *Envelope) (err error) {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(ctx); rollbackErr != nil &&
			!errors.Is(rollbackErr, pgx.ErrTxClosed) && err == nil {
			err = rollbackErr
		}
	}()
	if err = b.PublishTx(ctx, tx, env); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// PublishTx inserts the event row and enqueues all consumer jobs within
// the caller's tx. Caller owns commit/rollback.
//
// Idempotency: a duplicate (source, source_id) insert (when source_id is
// non-empty) is treated as a no-op — returns nil without enqueueing
// consumer jobs. This matches spec §3.3: "External daemons can safely
// retry a batched post; in-process publishers that pass a stable source_id
// are automatically deduped."
//
// Routing-before-insert: consumerJobsForKind runs before InsertEvent so
// a payload decode / validation failure (currently only
// contact_methods.added requires this) rolls back the caller's tx
// without landing a stranded event row with no consumer (spec §4).
//
// Event id preassignment: env.ID is populated before routing so
// consumer job args that reference it (e.g., RematchDispatcherJobArgs.
// EventID) are non-nil. The repository honors the caller-supplied UUID
// when env.ID is set; otherwise the DB's gen_random_uuid() DEFAULT
// fires.
func (b *Bus) PublishTx(ctx context.Context, tx pgx.Tx, env *Envelope) error {
	if tx == nil {
		return fmt.Errorf("publish: nil tx")
	}
	if env == nil {
		return fmt.Errorf("publish: nil envelope")
	}
	if env.Kind == "" {
		return fmt.Errorf("publish: empty kind")
	}
	if _, ok := kindPayloadTypes[env.Kind]; !ok {
		// Spec §6 "events/bus_test.go" list: unknown-kind rejection is
		// required. Prevents publishers from persisting arbitrary strings
		// into the event log (which would later surface as undecodable
		// payloads in consumers).
		return fmt.Errorf("publish: unknown kind %q", env.Kind)
	}
	if env.Source == "" {
		return fmt.Errorf("publish: empty source")
	}
	if len(env.Payload) == 0 {
		return fmt.Errorf("publish: empty payload for kind %s", env.Kind)
	}
	if env.ObservedAt.IsZero() {
		return fmt.Errorf("publish: empty observed_at for kind %s", env.Kind)
	}

	// Preassign env.ID so consumer-job args that reference it populate
	// correctly during routing (which runs BEFORE the actual DB insert).
	// repository/event.go honors a caller-supplied UUID; otherwise the
	// DB DEFAULT gen_random_uuid() fires. Generating client-side is
	// equivalent to a DB-side UUID from a uniqueness standpoint.
	//
	// We track whether the ID was caller-supplied so a duplicate
	// (ON CONFLICT) path can restore env.ID to uuid.Nil for the
	// IngestService duplicate-detection contract (it distinguishes
	// accepted vs duplicate via `env.ID == uuid.Nil` post-return).
	callerSuppliedID := env.ID != uuid.Nil
	if !callerSuppliedID {
		env.ID = uuid.New()
	}

	// Route BEFORE insert so a routing error fails the whole tx and no
	// zombie event row lands with no consumer (spec §4 atomicity).
	jobs, err := consumerJobsForKind(env)
	if err != nil {
		return fmt.Errorf("route consumer jobs: %w", err)
	}

	if err := b.eventRepo.InsertEvent(ctx, tx, env); err != nil {
		if errors.Is(err, db.ErrDuplicate) {
			// (source, source_id) collision → idempotent no-op.
			// Reset env.ID when we pre-assigned it so callers using the
			// `env.ID == uuid.Nil` duplicate sentinel (IngestService)
			// see the dup. Caller-supplied IDs stay as-is.
			if !callerSuppliedID {
				env.ID = uuid.Nil
			}
			return nil
		}
		return fmt.Errorf("insert event: %w", err)
	}

	for _, job := range jobs {
		if _, err := b.riverClient.InsertTx(ctx, tx, job.Args, job.Opts); err != nil {
			return fmt.Errorf("enqueue consumer job %T: %w", job.Args, err)
		}
	}
	return nil
}

// GetEvent returns the envelope for an event id. Used by consumer worker
// wrappers (PR 5+) to fetch the full payload given a job-arg event id.
func (b *Bus) GetEvent(ctx context.Context, id uuid.UUID) (*Envelope, error) {
	return b.eventRepo.GetEvent(ctx, id)
}

// FindEventBySource looks up an event by its (source, source_id) pair.
// Returns db.ErrNotFound when no row matches. Used by the aggregator's
// stale-claim recovery path: when the engine sees an already-published
// event for a session's deterministic sourceRef, it re-enqueues a
// consumer job against the existing event row rather than
// re-publishing (event-log dedup would suppress re-publish).
//
// Thin passthrough to b.eventRepo.FindEventBySource — exposed on the
// Bus so the aggregation package can depend on *events.Bus rather than
// importing the repository package directly.
func (b *Bus) FindEventBySource(ctx context.Context, source, sourceID string) (*Envelope, error) {
	return b.eventRepo.FindEventBySource(ctx, source, sourceID)
}
