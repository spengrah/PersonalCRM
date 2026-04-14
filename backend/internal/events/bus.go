package events

import (
	"context"
	"errors"
	"fmt"

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
// river jobs to enqueue atomically alongside its event row. In PR 2 the
// registry is a stub: it returns an empty slice for every kind (spec §5
// PR 2 scope: "returns empty slice for all kinds"). PR 5+ extends this
// function as consumer workers come online.
//
// The eventID argument is the event row's primary key; job arg structs
// should embed it so the worker can fetch the full payload by ID (spec
// §3.3 — "keeps job-arg payload small").
func consumerJobsForKind(_ Kind, _ uuid.UUID) []consumerJob {
	return []consumerJob{}
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
	if env == nil {
		return fmt.Errorf("publish: nil envelope")
	}
	if env.Kind == "" {
		return fmt.Errorf("publish: empty kind")
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
