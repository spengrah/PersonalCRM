//go:build integration_testdb

package judgment_test

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
	"personal-crm/backend/internal/judgment"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	"personal-crm/backend/internal/synthetic"
	"personal-crm/backend/internal/synthetic/factory"
	"personal-crm/backend/internal/testdb"
	"personal-crm/backend/internal/threadctx"
	"personal-crm/backend/internal/todoist"
	"personal-crm/backend/tests/testsupport"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/stretchr/testify/require"
)

// production is the method key the store's designation names in these tests.
const production = "prod-method"

var (
	prodMethod  = judgment.MethodRef{Key: production, Version: "v1", WordingVersion: "w1"}
	otherMethod = judgment.MethodRef{Key: "other-method", Version: "v1", WordingVersion: "w1"}
)

// env wires the judgment store over a real database: the shared assertion
// write path, the thread-context builder, the cadence owner (with the store
// as its obligation closer) and the contact service's recording path. River
// jobs are inserted by a client that never runs them.
type env struct {
	ctx      context.Context
	database *db.Database
	harness  *synthetic.Harness
	store    *judgment.Store
	cadence  *consumer.CadenceUpdater
	contacts *service.ContactService
	comms    *repository.CommsMessageRepository
	venues   *repository.VenueRepository
	tasks    *repository.ContactTaskRepository
	people   *repository.ContactRepository
	jobs     *river.Client[pgx.Tx]
	base     time.Time
}

// envOption adjusts an env's wiring.
type envOption func(*envWiring)

type envWiring struct {
	followUp    bool
	ownDatabase bool
}

// withFollowUp wires the follow-up manager into the recording path, so an
// outbound recording opens the contact's follow-up reminder.
func withFollowUp() envOption { return func(w *envWiring) { w.followUp = true } }

// withOwnDatabase runs the env on a database clone of its own, so reads
// across every contact see only what the test seeds.
func withOwnDatabase() envOption { return func(w *envWiring) { w.ownDatabase = true } }

// newEnv builds an env whose store designates productionMethod ("" for none).
func newEnv(t *testing.T, productionMethod string, opts ...envOption) *env {
	t.Helper()
	var wiring envWiring
	for _, opt := range opts {
		opt(&wiring)
	}
	var database *db.Database
	if wiring.ownDatabase {
		url, drop := testdb.NewEphemeralClone(t)
		t.Cleanup(drop)
		database = testsupport.OpenDatabaseAt(t, url)
	} else {
		database = testsupport.OpenDatabase(t)
	}
	ctx := t.Context()
	jobs := newInsertOnlyClient(t, database)
	bus := events.NewBus(database.Pool, jobs, repository.NewEventRepository(database.Queries))

	contactRepo := repository.NewContactRepository(database.Queries)
	contactRepo.SetPool(database.Pool)
	nodes := repository.NewNodeRepository(database.Queries)
	assertions := repository.NewAssertionRepository(database.Queries)
	asserts := service.NewAssertService(database.Pool, nodes, repository.NewEntityRepository(database.Queries),
		repository.NewPredicateRepository(database.Queries), assertions, bus)
	cadence := consumer.NewCadenceUpdater(repository.NewEventConsumerClaimRepository(database.Queries), contactRepo,
		database.Queries, consumer.CadenceModeCutover, false, config.TestConfig().Watchdog)
	venues := repository.NewVenueRepository(database.Queries)
	store := judgment.NewStore(judgment.StoreDeps{
		Pool:             database.Pool,
		Queries:          database.Queries,
		Assert:           asserts,
		Contexts:         threadctx.NewBuilder(repository.NewThreadContentRepository(database.Queries), venues),
		Cadence:          cadence,
		Jobs:             jobs,
		Watchdog:         config.TestConfig().Watchdog,
		ProductionMethod: productionMethod,
	})
	cadence.SetObligationCloser(store)
	tasks := repository.NewContactTaskRepository(database.Queries)
	interactions := repository.NewInteractionRepository(database.Queries)
	var followUp service.FollowUpConsumer
	if wiring.followUp {
		followUp = consumer.NewFollowUpManager(consumer.FollowUpModeCutover,
			repository.NewEventConsumerClaimRepository(database.Queries), contactRepo, tasks, tasks, interactions, jobs,
			func(context.Context) (*todoist.Settings, string, error) {
				return &todoist.Settings{ProjectID: "project", LabelID: "label", LabelName: "crm"}, "token", nil
			}, "http://frontend.test", config.TestConfig().Watchdog)
	}
	contacts := service.NewContactService(database, contactRepo, repository.NewContactMethodRepository(database.Queries),
		interactions, tasks, nil, nil, cadence, asserts,
		consumer.NewKnowledgeCacheUpdater(assertions, nodes, contactRepo), followUp)

	return &env{
		ctx:      ctx,
		database: database,
		harness:  synthetic.NewHarnessForNamespace(t, ctx, database, "j"+uuid.NewString()[:8], factory.DefaultSeed),
		store:    store,
		cadence:  cadence,
		contacts: contacts,
		comms:    repository.NewCommsMessageRepository(database.Queries),
		venues:   venues,
		tasks:    tasks,
		people:   contactRepo,
		jobs:     jobs,
		base:     accelerated.GetCurrentTime().UTC().Add(-90 * 24 * time.Hour).Truncate(time.Second),
	}
}

// newInsertOnlyClient is a River client that is never started: an insert
// lands its job, which never runs.
func newInsertOnlyClient(t *testing.T, database *db.Database) *river.Client[pgx.Tx] {
	t.Helper()
	workers := river.NewWorkers()
	river.AddWorker(workers, &noopWorker[consumerjobs.KnowledgeCacheUpdaterJobArgs]{})
	river.AddWorker(workers, &noopWorker[consumerjobs.TodoistTaskOpArgs]{})
	client, err := river.NewClient(riverpgxv5.New(database.Pool), &river.Config{
		Queues:   map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 1}},
		Workers:  workers,
		TestOnly: true,
	})
	require.NoError(t, err)
	return client
}

type noopWorker[T river.JobArgs] struct {
	river.WorkerDefaults[T]
}

func (*noopWorker[T]) Work(context.Context, *river.Job[T]) error { return nil }

// day returns the base time plus d days.
func (e *env) day(d int) time.Time { return e.base.Add(time.Duration(d) * 24 * time.Hour) }

// contact seeds a synthetic contact, with the cadence when one is given.
func (e *env) contact(t *testing.T, cadence ...string) uuid.UUID {
	t.Helper()
	var opts []factory.ContactOption
	if len(cadence) > 0 {
		opts = append(opts, factory.WithCadence(cadence[0]))
	}
	c, err := e.harness.SeedContact(e.ctx, e.harness.Generator().Contact(opts...))
	require.NoError(t, err)
	return c.ID
}

// thread is an email thread: its venue and its container key.
type thread struct {
	id        threadctx.VenueID
	container string
}

func (e *env) thread(t *testing.T) thread {
	t.Helper()
	container := "thread-" + uuid.NewString()
	tx, err := e.database.Pool.Begin(e.ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(e.ctx) }()
	id, err := e.venues.ResolveVenueForInteraction(e.ctx, tx, repository.InteractionSourceEmail, repository.VenueKindEmailThread, container, "")
	require.NoError(t, err)
	require.NoError(t, tx.Commit(e.ctx))
	return thread{id: id, container: container}
}

// message stores one message on the thread: from the contact when fromUser is
// false, else the user's own message to the contact.
func (e *env) message(t *testing.T, th thread, contact uuid.UUID, fromUser bool, at time.Time) {
	t.Helper()
	direction := repository.InteractionDirectionInbound
	if fromUser {
		direction = repository.InteractionDirectionOutbound
	}
	container, body, peer := th.container, "synthetic message body", "peer-"+contact.String()+"@example.test"
	_, err := e.comms.UpsertMessage(e.ctx, repository.UpsertCommsMessageParams{
		Source: repository.InteractionSourceEmail, ExternalID: uuid.NewString(), ThreadID: &container,
		Body: &body, PeerHandle: &peer, PeerNormalized: &peer, Direction: direction, SentAt: at,
		SourceMetadata: []byte("{}"), MatchedContactID: contact,
	})
	require.NoError(t, err)
}

// record records one interaction of the contact on the thread through the
// contact service's recording path, returning it.
func (e *env) record(t *testing.T, th thread, contact uuid.UUID, direction string, at time.Time) *repository.Interaction {
	t.Helper()
	ref := contact.String() + ":" + th.container + ":" + at.Format(time.RFC3339Nano)
	ix, err := e.contacts.RecordInteraction(e.ctx, repository.RecordInteractionRequest{
		ContactID: contact, Source: repository.InteractionSourceEmail, SourceRef: &ref,
		OccurredAt: at, Direction: direction, VenueID: &th.id,
	})
	require.NoError(t, err)
	return ix
}

// judgmentOf builds a judgment of the thread at the cut-off.
func judgmentOf(m judgment.MethodRef, th thread, cutOff time.Time, inputHash string, outcomes judgment.Outcomes) judgment.Judgment {
	return judgment.Judgment{
		Method:    m,
		Thread:    th.id,
		Kind:      threadctx.KindConversation,
		CutOff:    cutOff,
		JudgedAt:  cutOff.Add(time.Hour),
		InputHash: inputHash,
		Questions: []judgment.AnsweredQuestion{{ID: "ball_in_court:THEM", Probabilities: map[string]float64{"i_owe_reply": 0.7, "nobody_owes": 0.3}, OriginalAnswer: "i_owe_reply"}},
		Outcomes:  outcomes,
		Usage:     judgment.Usage{ContextTokens: 420, Questions: 4, Latency: 250 * time.Millisecond},
	}
}

// run starts a run of kind for the judgment's method and records the judgment
// in it.
func (e *env) run(t *testing.T, kind judgment.RunKind, j judgment.Judgment) (judgment.RunID, judgment.Effects) {
	t.Helper()
	run, err := e.store.StartRun(e.ctx, judgment.RunSpec{Kind: kind, Method: j.Method})
	require.NoError(t, err)
	effects, err := e.store.RecordJudgment(e.ctx, run, j)
	require.NoError(t, err)
	return run, effects
}

func bic(contact uuid.UUID, value judgment.BallInCourt, confidence float64) judgment.ParticipantOutcome[judgment.BallInCourt] {
	return judgment.ParticipantOutcome[judgment.BallInCourt]{QuestionID: "ball_in_court:" + uuid.NewString()[:6], ContactID: contact, Outcome: value, Confidence: confidence}
}

func commitment(contact uuid.UUID, direction judgment.CommitmentDirection, state judgment.CommitmentState, confidence float64, due *time.Time) judgment.CommitmentOutcome {
	return judgment.CommitmentOutcome{
		QuestionID: "commitment_" + string(direction) + ":" + uuid.NewString()[:6],
		ContactID:  contact, Direction: direction, Outcome: state, Confidence: confidence, DueDate: due,
	}
}

func date(t time.Time) *time.Time {
	d := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	return &d
}

// openObligations returns the thread's open obligations.
func (e *env) openObligations(t *testing.T, th thread) []judgment.Obligation {
	t.Helper()
	all, err := e.store.Obligations(e.ctx, th.id)
	require.NoError(t, err)
	var open []judgment.Obligation
	for _, o := range all {
		if o.Open {
			open = append(open, o)
		}
	}
	return open
}

// outcomes returns the outcomes of the thread's judgments in order.
func (e *env) outcomes(t *testing.T, th thread) []judgment.OutcomeView {
	t.Helper()
	views, err := e.store.ListJudgments(e.ctx, judgment.JudgmentQuery{Thread: &th.id})
	require.NoError(t, err)
	var out []judgment.OutcomeView
	for _, v := range views {
		out = append(out, v.Outcomes...)
	}
	return out
}

// outcome returns the one outcome of the thread's judgment of method m on the
// question and contact.
func (e *env) outcome(t *testing.T, th thread, m judgment.MethodRef, question judgment.Question, contact *uuid.UUID) judgment.OutcomeView {
	t.Helper()
	views, err := e.store.ListJudgments(e.ctx, judgment.JudgmentQuery{Thread: &th.id})
	require.NoError(t, err)
	var found []judgment.OutcomeView
	for _, v := range views {
		if v.Method != m {
			continue
		}
		for _, o := range v.Outcomes {
			if o.Question == question && (contact == nil || (o.ContactID != nil && *o.ContactID == *contact)) {
				found = append(found, o)
			}
		}
	}
	require.Len(t, found, 1, "outcomes of %s on %s", m.Key, question)
	return found[0]
}

// todayDate is today's date as an obligation's due date reads back.
func todayDate() time.Time {
	now := accelerated.GetCurrentTime().In(time.Local)
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
}
