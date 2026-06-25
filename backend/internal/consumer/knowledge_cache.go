package consumer

import (
	"context"
	"errors"
	"fmt"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/consumer/consumerjobs"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
)

// currentAcceptedReader reads the current-accepted assertion for a (subject,
// predicate) slot inside the caller's tx. ErrNotFound means "no current value"
// (a gap or closure), which the cache refresh maps to a NULL column.
type currentAcceptedReader interface {
	GetCurrentAcceptedTx(ctx context.Context, tx pgx.Tx, subjectNodeID uuid.UUID, predicateKey string, now time.Time) (*repository.Assertion, error)
}

// nodeLabelReader reads a node's canonical label inside the caller's tx. Used to
// resolve a lives_in edge's place-node label into the location cache column.
type nodeLabelReader interface {
	GetNodeTx(ctx context.Context, tx pgx.Tx, id uuid.UUID) (*repository.Node, error)
}

// contactCacheWriter is the sole-writer surface for the derived contact
// knowledge-cache columns. Each method writes one column (nil clears it to NULL)
// and never touches updated_at — a cache refresh is bookkeeping, not a profile
// edit.
type contactCacheWriter interface {
	UpdateContactLocationCacheTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, location *string) error
	UpdateContactBirthdayCacheTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, birthday *time.Time) error
	UpdateContactHowMetCacheTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, howMet *string) error
}

// KnowledgeCacheUpdater is the sole post-cutover writer of the derived
// contact.location / contact.birthday / contact.how_met cache columns. It
// recomputes a column from the current-accepted lives_in / birthday / how_met
// assertion (recompute-from-scratch, NOT an incremental patch), which makes it
// correct for supersession (NYC→LA), closure (moved away → gap → NULL), and
// retraction (an accepted row retracted → no current value → NULL) alike.
//
// Two entry points funnel into RefreshTx:
//   - RefreshTx:   direct-invoke from ContactService's create/update/merge tx,
//     so a user edit's cache column is current the moment the tx commits (no
//     read-path gap waiting on the async worker).
//   - HandleEvent: envelope-driven (the river worker), for assertion.accepted /
//     assertion.superseded events from any other producer (SP3 extractors, the
//     rollover worker, retractions). It decodes the payload's predicate_key and
//     no-ops unless the predicate is one of the three cutover cache predicates.
//
// The subject node id is the contact id (node.id == contact.id for person
// nodes), so the cache column is keyed directly off the assertion's subject.
type KnowledgeCacheUpdater struct {
	assertions currentAcceptedReader
	nodes      nodeLabelReader
	contacts   contactCacheWriter
}

// NewKnowledgeCacheUpdater wires the consumer with narrow interfaces so unit
// tests can stub them. Production wires the concrete AssertionRepository,
// NodeRepository, and ContactRepository.
func NewKnowledgeCacheUpdater(
	assertions currentAcceptedReader,
	nodes nodeLabelReader,
	contacts contactCacheWriter,
) *KnowledgeCacheUpdater {
	return &KnowledgeCacheUpdater{
		assertions: assertions,
		nodes:      nodes,
		contacts:   contacts,
	}
}

// HandleEvent decodes an assertion.accepted / assertion.superseded envelope and,
// when its predicate is a cutover cache predicate, refreshes the subject
// contact's cache column. A non-cutover predicate is a no-op (the bus routes by
// kind, so every accepted/superseded event reaches this consumer). Returns nil
// on a no-op so the river job completes.
func (u *KnowledgeCacheUpdater) HandleEvent(ctx context.Context, tx pgx.Tx, env *events.Envelope) error {
	var payload events.AssertionEventPayload
	if err := events.Unmarshal(env, &payload); err != nil {
		return fmt.Errorf("decode assertion event: %w", err)
	}
	if !isCacheCutoverPredicate(payload.PredicateKey) {
		return nil
	}
	return u.RefreshTx(ctx, tx, payload.SubjectNodeID, payload.PredicateKey)
}

// RefreshTx recomputes one contact cache column from the current-accepted
// assertion for (contactID, predicateKey). predicateKey MUST be one of the three
// cutover cache predicates; any other key is a programming error and returns an
// error rather than silently no-op'ing. A missing current-accepted assertion
// (ErrNotFound) clears the column to NULL.
func (u *KnowledgeCacheUpdater) RefreshTx(ctx context.Context, tx pgx.Tx, contactID uuid.UUID, predicateKey string) error {
	now := accelerated.GetCurrentTime().UTC()
	current, err := u.assertions.GetCurrentAcceptedTx(ctx, tx, contactID, predicateKey, now)
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		return fmt.Errorf("read current %s for %s: %w", predicateKey, contactID, err)
	}
	// ErrNotFound → current is nil → clear the column to NULL (supersession to a
	// gap, closure, or retraction all land here).

	switch predicateKey {
	case repository.PredicateLivesIn:
		location, lerr := u.resolveLivesInLabel(ctx, tx, current)
		if lerr != nil {
			return lerr
		}
		return u.contacts.UpdateContactLocationCacheTx(ctx, tx, contactID, location)
	case repository.PredicateBirthday:
		var birthday *time.Time
		if current != nil {
			birthday = current.ValueDate
		}
		return u.contacts.UpdateContactBirthdayCacheTx(ctx, tx, contactID, birthday)
	case repository.PredicateHowMet:
		var howMet *string
		if current != nil {
			howMet = current.ValueText
		}
		return u.contacts.UpdateContactHowMetCacheTx(ctx, tx, contactID, howMet)
	default:
		return fmt.Errorf("knowledge-cache refresh: unsupported predicate %q", predicateKey)
	}
}

// resolveLivesInLabel returns the place node's canonical_label for a current
// lives_in edge, or nil when there is no current value. A lives_in assertion
// with no object node id is a data error (the edge payload must be present) and
// returns an error rather than a silently blank cache.
func (u *KnowledgeCacheUpdater) resolveLivesInLabel(ctx context.Context, tx pgx.Tx, current *repository.Assertion) (*string, error) {
	if current == nil {
		return nil, nil
	}
	if current.ObjectNodeID == nil {
		return nil, fmt.Errorf("knowledge-cache: current lives_in assertion %s has no object node", current.ID)
	}
	place, err := u.nodes.GetNodeTx(ctx, tx, *current.ObjectNodeID)
	if err != nil {
		return nil, fmt.Errorf("read place node %s: %w", *current.ObjectNodeID, err)
	}
	label := place.CanonicalLabel
	return &label, nil
}

// isCacheCutoverPredicate reports whether a predicate key drives one of the
// derived contact cache columns.
func isCacheCutoverPredicate(predicateKey string) bool {
	switch predicateKey {
	case repository.PredicateLivesIn, repository.PredicateBirthday, repository.PredicateHowMet:
		return true
	default:
		return false
	}
}

// --------------------------------------------------------------------------
// River worker — fetches the event envelope by id, opens a fresh tx, and
// dispatches to HandleEvent.
// --------------------------------------------------------------------------

// KnowledgeCacheUpdaterWorker is the river worker that dispatches queued
// KnowledgeCacheUpdaterJobArgs to KnowledgeCacheUpdater.HandleEvent.
type KnowledgeCacheUpdaterWorker struct {
	river.WorkerDefaults[consumerjobs.KnowledgeCacheUpdaterJobArgs]
	bus      eventBusTx
	pool     *pgxpool.Pool
	consumer *KnowledgeCacheUpdater
}

// NewKnowledgeCacheUpdaterWorker wires the worker to the concrete bus, the
// application pgxpool, and the consumer instance.
func NewKnowledgeCacheUpdaterWorker(bus eventBusTx, pool *pgxpool.Pool, consumer *KnowledgeCacheUpdater) *KnowledgeCacheUpdaterWorker {
	return &KnowledgeCacheUpdaterWorker{bus: bus, pool: pool, consumer: consumer}
}

// Work implements river.Worker. Fetches the event envelope by id, opens a fresh
// tx, and invokes HandleEvent. On error River retries per MaxAttempts (5 from
// the InsertOpts in events.consumerJobsForKind).
func (w *KnowledgeCacheUpdaterWorker) Work(ctx context.Context, j *river.Job[consumerjobs.KnowledgeCacheUpdaterJobArgs]) error {
	env, err := w.bus.GetEvent(ctx, j.Args.EventID)
	if err != nil {
		return fmt.Errorf("fetch event %s: %w", j.Args.EventID, err)
	}
	return pgx.BeginTxFunc(ctx, w.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		return w.consumer.HandleEvent(ctx, tx, env)
	})
}

// Timeout bounds a single cache-refresh run. A read of the current-accepted
// assertion + one node read + one column UPDATE completes in a few ms; 30s is
// ample headroom for pool saturation.
func (*KnowledgeCacheUpdaterWorker) Timeout(*river.Job[consumerjobs.KnowledgeCacheUpdaterJobArgs]) time.Duration {
	return 30 * time.Second
}
