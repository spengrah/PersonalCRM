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
// river jobs to enqueue atomically alongside its event row. PR 5 wires the
// first consumer (InteractionRecorder); later PRs extend the switch as
// CadenceUpdater, FollowUpManager, and RematchDispatcher come online.
//
// The eventID argument is the event row's primary key; job arg structs
// embed it so the worker can fetch the full payload by ID (spec §3.3 —
// "keeps job-arg payload small").
//
// PR 5 routing rules:
//
//   - 5 async-publisher kinds (message.received/sent, calendar.attended,
//     task.completed, task.outreach_detected) → InteractionRecorder worker
//     with MaxAttempts=5 (plan Decision 8).
//   - interaction.manual → returns nil. The manual UI handler inline-
//     invokes HandleEvent in its shadow tx so the consumer isn't double-
//     invoked. Spec §3.4 says PublishTx should enqueue jobs "for other
//     consumers"; InteractionRecorder is not "other" in the manual flow
//     (plan Decision 7).
//   - interaction.recorded → returns nil. No consumer in PR 5 (CadenceUpdater
//     lands in PR 7, FollowUpManager in PR 9a).
//   - calendar.declined, task.skipped, contact_methods.added → returns nil.
//     Consumers land in later PRs.
func consumerJobsForKind(kind Kind, eventID uuid.UUID) []consumerJob {
	switch kind {
	case KindMessageReceived, KindMessageSent, KindCalendarAttended,
		KindTaskCompleted, KindTaskOutreachDetected:
		return []consumerJob{{
			Args: consumerjobs.InteractionRecorderJobArgs{EventID: eventID},
			Opts: &river.InsertOpts{MaxAttempts: 5},
		}}
	}
	return nil
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
// On successful insert, env.ID is populated with the DB-generated (or
// caller-provided) row id.
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

	err := b.eventRepo.InsertEvent(ctx, tx, env)
	if err != nil {
		if errors.Is(err, db.ErrDuplicate) {
			// (source, source_id) collision → idempotent no-op.
			return nil
		}
		return fmt.Errorf("insert event: %w", err)
	}

	for _, job := range consumerJobsForKind(env.Kind, env.ID) {
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
