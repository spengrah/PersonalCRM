//go:build integration_testdb

package repository_test

import (
	"context"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/consumer"
	"personal-crm/backend/internal/consumer/consumerjobs"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	"personal-crm/backend/internal/synthetic"
	"personal-crm/backend/internal/synthetic/factory"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/stretchr/testify/require"
)

// correctionEnv seeds contacts through the synthetic harness, records
// observations through the contact service's recording path and writes correction state through AssertService and the
// interaction-node merge write, in the correction representation: a removal is
// an accepted not_participant_in or removed_for_everyone assertion, and a merge
// is an alias plus an accepted same_interaction_as assertion, each with one
// user locator.
type correctionEnv struct {
	ctx          context.Context
	harness      *synthetic.Harness
	database     *db.Database
	contacts     *service.ContactService
	interactions *repository.InteractionRepository
	nodes        *repository.NodeRepository
	predicates   *repository.PredicateRepository
	assertions   *repository.AssertionRepository
	asserts      *service.AssertService
	base         time.Time
}

func newCorrectionEnv(t *testing.T) *correctionEnv {
	t.Helper()
	ctx := t.Context()
	harness := synthetic.NewHarnessForNamespace(t, ctx, "r"+uuid.NewString()[:8], factory.DefaultSeed)
	database := harness.Database()

	contactRepo := repository.NewContactRepository(database.Queries)
	contactRepo.SetPool(database.Pool)
	interactions := repository.NewInteractionRepository(database.Queries)
	nodes := repository.NewNodeRepository(database.Queries)
	predicates := repository.NewPredicateRepository(database.Queries)
	assertions := repository.NewAssertionRepository(database.Queries)
	asserts := service.NewAssertService(database.Pool, nodes, repository.NewEntityRepository(database.Queries),
		predicates, assertions, newInsertOnlyBus(t, database))
	cadence := consumer.NewCadenceUpdater(repository.NewEventConsumerClaimRepository(database.Queries), contactRepo,
		database.Queries, consumer.CadenceModeCutover, false, config.TestConfig().Watchdog)
	contacts := service.NewContactService(database, contactRepo, repository.NewContactMethodRepository(database.Queries),
		interactions, repository.NewContactTaskRepository(database.Queries), nil, nil, cadence, asserts,
		consumer.NewKnowledgeCacheUpdater(assertions, nodes, contactRepo), nil)

	return &correctionEnv{
		ctx:          ctx,
		harness:      harness,
		database:     database,
		contacts:     contacts,
		interactions: interactions,
		nodes:        nodes,
		predicates:   predicates,
		assertions:   assertions,
		asserts:      asserts,
		base:         accelerated.GetCurrentTime().UTC().Add(-48 * time.Hour).Truncate(time.Second),
	}
}

// newInsertOnlyBus is a bus whose River client is never started: a publish
// inserts the event and its consumer jobs, which never run.
func newInsertOnlyBus(t *testing.T, database *db.Database) *events.Bus {
	t.Helper()
	workers := river.NewWorkers()
	river.AddWorker(workers, &knowledgeCacheNoopWorker{})
	client, err := river.NewClient(riverpgxv5.New(database.Pool), &river.Config{
		Queues:   map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 1}},
		Workers:  workers,
		TestOnly: true,
	})
	require.NoError(t, err)
	return events.NewBus(database.Pool, client, repository.NewEventRepository(database.Queries))
}

type knowledgeCacheNoopWorker struct {
	river.WorkerDefaults[consumerjobs.KnowledgeCacheUpdaterJobArgs]
}

func (*knowledgeCacheNoopWorker) Work(context.Context, *river.Job[consumerjobs.KnowledgeCacheUpdaterJobArgs]) error {
	return nil
}

// at returns the base time plus h hours.
func (e *correctionEnv) at(h int) time.Time { return e.base.Add(time.Duration(h) * time.Hour) }

// contact seeds a synthetic contact (and its person node) in the env's
// namespace.
func (e *correctionEnv) contact(t *testing.T) uuid.UUID {
	t.Helper()
	c, err := e.harness.SeedContact(e.ctx, e.harness.Generator().Contact())
	require.NoError(t, err)
	return c.ID
}

// observe records one observation through the recording path and returns it.
func (e *correctionEnv) observe(t *testing.T, contactID uuid.UUID, source, sourceRef, direction string, occurredAt time.Time) *repository.Interaction {
	t.Helper()
	ix, err := e.contacts.RecordInteraction(e.ctx, repository.RecordInteractionRequest{
		ContactID:  contactID,
		Source:     source,
		SourceRef:  &sourceRef,
		OccurredAt: occurredAt,
		Direction:  direction,
	})
	require.NoError(t, err)
	return ix
}

// call records a contact's observation of the phone call ref; every
// observation of one call shares its interaction node.
func (e *correctionEnv) call(t *testing.T, contactID uuid.UUID, ref, direction string, occurredAt time.Time) *repository.Interaction {
	t.Helper()
	ix := e.observe(t, contactID, repository.InteractionSourcePhoneCalls, ref, direction, occurredAt)
	require.NotNil(t, ix.InteractionNodeID)
	return ix
}

// inTx runs fn in one committed transaction.
func (e *correctionEnv) inTx(t *testing.T, fn func(tx pgx.Tx) error) {
	t.Helper()
	require.NoError(t, pgx.BeginTxFunc(e.ctx, e.database.Pool, pgx.TxOptions{}, fn))
}

// userLocator is the one provenance locator of a correction act.
func userLocator(act uuid.UUID, reason *string) service.ProvenanceLocator {
	loc := service.ProvenanceLocator{
		SourceKind:   repository.SourceKindUser,
		SourceID:     act.String(),
		ProducerKind: repository.ProducerKindUser,
	}
	if reason != nil {
		field := "reason"
		loc.Field = &field
		loc.Quote = reason
	}
	return loc
}

func strPtr(s string) *string { return &s }

// removeContact writes the removal of contactID from node: an accepted
// not_participant_in from the contact's person node.
func (e *correctionEnv) removeContact(t *testing.T, contactID, node uuid.UUID) *repository.Assertion {
	t.Helper()
	var a *repository.Assertion
	e.inTx(t, func(tx pgx.Tx) error {
		var err error
		a, err = e.asserts.AssertTx(e.ctx, tx, service.AssertRequest{
			SubjectNodeID: contactID,
			PredicateKey:  "not_participant_in",
			ObjectNodeID:  &node,
			Confidence:    100,
			Locators:      []service.ProvenanceLocator{userLocator(uuid.New(), strPtr("did_not_attend"))},
		})
		return err
	})
	return a
}

// removeEveryone writes the removal of node for everyone: an accepted
// removed_for_everyone (true) on the node.
func (e *correctionEnv) removeEveryone(t *testing.T, node uuid.UUID) {
	t.Helper()
	e.inTx(t, func(tx pgx.Tx) error {
		yes := true
		_, err := e.asserts.AssertTx(e.ctx, tx, service.AssertRequest{
			SubjectNodeID: node,
			PredicateKey:  "removed_for_everyone",
			ValueBool:     &yes,
			Confidence:    100,
			Locators:      []service.ProvenanceLocator{userLocator(uuid.New(), strPtr("did_not_happen"))},
		})
		return err
	})
}

// merge merges root loser into root winner: the registry alias plus an
// accepted same_interaction_as from loser to winner.
func (e *correctionEnv) merge(t *testing.T, loser, winner uuid.UUID) {
	t.Helper()
	e.inTx(t, func(tx pgx.Tx) error {
		if err := e.nodes.MergeInteractionNodeTx(e.ctx, tx, loser, winner); err != nil {
			return err
		}
		_, err := e.asserts.AssertTx(e.ctx, tx, service.AssertRequest{
			SubjectNodeID: loser,
			PredicateKey:  "same_interaction_as",
			ObjectNodeID:  &winner,
			Confidence:    100,
			Locators:      []service.ProvenanceLocator{userLocator(uuid.New(), nil)},
		})
		return err
	})
}

// participation returns the contact's effective participation.
func (e *correctionEnv) participation(t *testing.T, contactID uuid.UUID) []repository.EffectiveParticipation {
	t.Helper()
	eps, err := e.interactions.ListEffectiveParticipation(e.ctx, contactID)
	require.NoError(t, err)
	return eps
}

// entryFor returns the contact's entry keyed by root, failing when absent.
func entryFor(t *testing.T, eps []repository.EffectiveParticipation, root uuid.UUID) repository.EffectiveParticipation {
	t.Helper()
	for _, ep := range eps {
		if ep.InteractionNodeID != nil && *ep.InteractionNodeID == root {
			return ep
		}
	}
	require.Failf(t, "no entry", "no effective participation for root %s in %+v", root, eps)
	return repository.EffectiveParticipation{}
}

// memberIDs returns an entry's member observation ids.
func memberIDs(ep repository.EffectiveParticipation) []uuid.UUID {
	ids := make([]uuid.UUID, len(ep.Members))
	for i, m := range ep.Members {
		ids[i] = m.ObservationID
	}
	return ids
}
