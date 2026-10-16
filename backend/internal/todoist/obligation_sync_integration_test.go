//go:build integration_testdb

package todoist_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/consumer"
	"personal-crm/backend/internal/consumer/consumerjobs"
	"personal-crm/backend/internal/contacttask"
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
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The user's act on an obligation task in Todoist closes its obligation:
// completing a reach_out task records outreach as any reach_out completion
// does, completing an I-owe commitment reminder records nothing, and
// dismissing either records nothing.
// spec: CAD-057.reach-out-completion-records-outreach, CAD-057.reminder-completion-records-nothing, CAD-057.dismissal-closes-and-records-nothing
func TestObligationTaskSync(t *testing.T) {
	cases := []struct {
		name     string
		outcomes func(c uuid.UUID) judgment.Outcomes
		kind     string
		act      func(item *todoist.SyncItem)
		state    repository.ContactTaskState
		outreach bool
	}{
		{
			name: "completing an i_owe_reply reach_out task records outreach and closes it",
			outcomes: func(c uuid.UUID) judgment.Outcomes {
				return judgment.Outcomes{BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{{QuestionID: "ball_in_court:THEM", ContactID: c, Outcome: judgment.BallInCourtIOweReply, Confidence: 0.9}}}
			},
			kind:     contacttask.KindReachOut,
			act:      func(item *todoist.SyncItem) { item.Checked = true },
			state:    repository.ContactTaskStateCompleted,
			outreach: true,
		},
		{
			name:     "completing an I-owe commitment reminder closes it and records nothing",
			outcomes: commitmentMine,
			kind:     contacttask.KindReminder,
			act:      func(item *todoist.SyncItem) { item.Checked = true },
			state:    repository.ContactTaskStateCompleted,
		},
		{
			name:     "deleting an obligation task in Todoist dismisses it",
			outcomes: commitmentMine,
			kind:     contacttask.KindReminder,
			act:      func(item *todoist.SyncItem) { item.IsDeleted = true },
			state:    repository.ContactTaskStateDismissed,
		},
		{
			name:     "removing an obligation task's CRM label dismisses it",
			outcomes: commitmentMine,
			kind:     contacttask.KindReminder,
			act:      func(item *todoist.SyncItem) { item.Labels = nil },
			state:    repository.ContactTaskStateDismissed,
		},
		{
			name:     "removing an obligation task's deadline dismisses it",
			outcomes: commitmentMine,
			kind:     contacttask.KindReminder,
			act:      func(item *todoist.SyncItem) { item.Deadline = nil },
			state:    repository.ContactTaskStateDismissed,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newObligationEnv(t)
			c := e.contact(t)
			th := e.judge(t, c, tc.outcomes(c))
			open := e.openObligations(t, th)
			require.Len(t, open, 1)
			require.Equal(t, tc.kind, open[0].Kind)
			external := e.finalize(t, open[0].TaskID)
			before, err := e.people.GetContact(e.ctx, c)
			require.NoError(t, err)

			item := todoist.SyncItem{ID: external, Labels: []string{obligationLabel}, Deadline: &todoist.SyncDate{Date: "2030-01-01"}}
			tc.act(&item)
			e.sync(t, item)
			e.drainJobs(t)

			task, err := e.tasks.GetContactTask(e.ctx, open[0].TaskID)
			require.NoError(t, err)
			assert.Equal(t, tc.state, task.State)
			assert.Empty(t, e.openObligations(t, th), "the obligation closed")
			recorded := e.taskCorrespondence(t, c)
			after, err := e.people.GetContact(e.ctx, c)
			require.NoError(t, err)
			if tc.outreach {
				require.Len(t, recorded, 1, "completing the task records one interaction")
				assert.Equal(t, repository.InteractionDirectionOutbound, recorded[0].Direction)
				require.NotNil(t, after.LastOutreachAt, "the completion is the contact's outreach")
				assert.True(t, recorded[0].OccurredAt.Equal(*after.LastOutreachAt))
			} else {
				assert.Empty(t, recorded, "no correspondence is recorded")
				assert.Equal(t, before.LastOutreachAt, after.LastOutreachAt)
				assert.Equal(t, before.LastContacted, after.LastContacted)
			}
		})
	}
}

const obligationLabel = "crm"

func commitmentMine(c uuid.UUID) judgment.Outcomes {
	return judgment.Outcomes{Commitments: []judgment.CommitmentOutcome{{
		QuestionID: "commitment_mine:THEM", ContactID: c, Direction: judgment.CommitmentDirectionMine,
		Outcome: judgment.CommitmentStateOpen, Confidence: 0.9,
	}}}
}

// obligationEnv opens obligations through the judgment store and syncs their
// tasks through the Todoist provider, with the Todoist API faked. The provider
// publishes on the real event bus, and a started River client runs the real
// interaction recorder on what it published. The database is a per-test clone
// so this client works only this test's jobs.
type obligationEnv struct {
	ctx          context.Context
	harness      *synthetic.Harness
	store        *judgment.Store
	tasks        *repository.ContactTaskRepository
	people       *repository.ContactRepository
	interactions *repository.InteractionRepository
	comms        *repository.CommsMessageRepository
	venues       *repository.VenueRepository
	provider     *todoist.CadenceSyncProvider
	pool         *pgxpool.Pool
	jobs         *river.Client[pgx.Tx]
	feed         *fakeFeed
}

func newObligationEnv(t *testing.T) *obligationEnv {
	t.Helper()
	cloneURL, drop := testdb.NewEphemeralClone(t)
	t.Cleanup(drop)
	database := testsupport.OpenDatabaseAt(t, cloneURL)
	ctx := t.Context()

	// The recorder needs the bus and the bus needs the client, so the
	// recorder worker is filled in once both exist, before the client starts.
	recorderWorker := &deferredRecorderWorker{}
	workers := river.NewWorkers()
	river.AddWorker(workers, recorderWorker)
	river.AddWorker(workers, &noopWorker[consumerjobs.CadenceUpdaterJobArgs]{})
	river.AddWorker(workers, &noopWorker[consumerjobs.FollowUpManagerJobArgs]{})
	river.AddWorker(workers, &noopWorker[consumerjobs.KnowledgeCacheUpdaterJobArgs]{})
	jobs, err := river.NewClient(riverpgxv5.New(database.Pool), &river.Config{
		Queues:   map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 2}},
		Workers:  workers,
		TestOnly: true,
	})
	require.NoError(t, err)
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
		Pool: database.Pool, Queries: database.Queries, Assert: asserts,
		Contexts: threadctx.NewBuilder(repository.NewThreadContentRepository(database.Queries), venues),
		Cadence:  cadence, Jobs: jobs, Watchdog: config.TestConfig().Watchdog, ProductionMethod: "prod-method",
	})
	cadence.SetObligationCloser(store)
	tasks := repository.NewContactTaskRepository(database.Queries)
	interactions := repository.NewInteractionRepository(database.Queries)
	contacts := service.NewContactService(database, contactRepo, repository.NewContactMethodRepository(database.Queries),
		interactions, tasks, nil, nil, cadence, asserts,
		consumer.NewKnowledgeCacheUpdater(assertions, nodes, contactRepo), nil)
	recorder := consumer.NewInteractionRecorder(contacts, repository.NewStagingProcessorRegistry(nil), bus, cadence, nil, nil)
	recorderWorker.real = consumer.NewInteractionRecorderWorker(bus, database.Pool, recorder, nil)

	// The client outlives t.Context(), which is cancelled before cleanups run,
	// so the synthetic harness's teardown still sees jobs drain.
	require.NoError(t, jobs.Start(context.WithoutCancel(ctx)))
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = jobs.Stop(stopCtx)
	})

	feed := &fakeFeed{}
	provider := todoist.NewCadenceSyncProvider(fakeAuth{}, tasks, contactRepo, repository.NewSyncRepository(database.Queries),
		config.TestConfig(), bus, cadence, database.Pool, func(string) todoist.Client { return feed }, jobs, false)
	return &obligationEnv{
		ctx:          ctx,
		harness:      synthetic.NewHarnessForNamespace(t, ctx, database, "o"+uuid.NewString()[:8], factory.DefaultSeed),
		store:        store,
		tasks:        tasks,
		people:       contactRepo,
		interactions: interactions,
		comms:        repository.NewCommsMessageRepository(database.Queries),
		venues:       venues,
		provider:     provider,
		pool:         database.Pool,
		jobs:         jobs,
		feed:         feed,
	}
}

// drainJobs waits until River has finished every job the test enqueued, and
// fails when one did not complete.
func (e *obligationEnv) drainJobs(t *testing.T) {
	t.Helper()
	unfinished := []rivertype.JobState{
		rivertype.JobStateAvailable, rivertype.JobStateRunning, rivertype.JobStateRetryable,
		rivertype.JobStateScheduled, rivertype.JobStatePending,
	}
	require.Eventually(t, func() bool {
		res, err := e.jobs.JobList(e.ctx, river.NewJobListParams().States(unfinished...).First(1000))
		require.NoError(t, err)
		return len(res.Jobs) == 0
	}, 10*time.Second, 20*time.Millisecond, "every enqueued job finishes")
	res, err := e.jobs.JobList(e.ctx, river.NewJobListParams().States(rivertype.JobStateDiscarded, rivertype.JobStateCancelled).First(1000))
	require.NoError(t, err)
	require.Empty(t, res.Jobs, "no job failed")
}

// taskCorrespondence returns the contact's interactions recorded from Todoist.
func (e *obligationEnv) taskCorrespondence(t *testing.T, c uuid.UUID) []repository.Interaction {
	t.Helper()
	all, err := e.interactions.ListContactInteractions(e.ctx, c, 1000, 0)
	require.NoError(t, err)
	var fromTasks []repository.Interaction
	for _, ix := range all {
		if ix.Source == repository.InteractionSourceTodoist {
			fromTasks = append(fromTasks, ix)
		}
	}
	return fromTasks
}

// deferredRecorderWorker runs the real interaction recorder worker, set once
// the bus it publishes on exists.
type deferredRecorderWorker struct {
	river.WorkerDefaults[consumerjobs.InteractionRecorderJobArgs]
	real *consumer.InteractionRecorderWorker
}

func (w *deferredRecorderWorker) Work(ctx context.Context, j *river.Job[consumerjobs.InteractionRecorderJobArgs]) error {
	return w.real.Work(ctx, j)
}

// noopWorker drains a job kind the recording publishes downstream of what the
// test observes.
type noopWorker[T river.JobArgs] struct {
	river.WorkerDefaults[T]
}

func (*noopWorker[T]) Work(context.Context, *river.Job[T]) error { return nil }

func (e *obligationEnv) contact(t *testing.T) uuid.UUID {
	t.Helper()
	c, err := e.harness.SeedContact(e.ctx, e.harness.Generator().Contact())
	require.NoError(t, err)
	return c.ID
}

// judge stores one inbound message from the contact on a new email thread and
// records the production method's judgment of it, which opens the outcomes'
// obligations.
func (e *obligationEnv) judge(t *testing.T, c uuid.UUID, outcomes judgment.Outcomes) threadctx.VenueID {
	t.Helper()
	container := "thread-" + uuid.NewString()
	tx, err := e.pool.Begin(e.ctx)
	require.NoError(t, err)
	thread, err := e.venues.ResolveVenueForInteraction(e.ctx, tx, repository.InteractionSourceEmail, repository.VenueKindEmailThread, container, "")
	require.NoError(t, err)
	require.NoError(t, tx.Commit(e.ctx))
	at := accelerated.GetCurrentTime().UTC().Add(-time.Hour).Truncate(time.Second)
	body, peer := "synthetic message body", "peer@example.test"
	_, err = e.comms.UpsertMessage(e.ctx, repository.UpsertCommsMessageParams{
		Source: repository.InteractionSourceEmail, ExternalID: uuid.NewString(), ThreadID: &container, Body: &body,
		PeerHandle: &peer, PeerNormalized: &peer, Direction: repository.InteractionDirectionInbound, SentAt: at,
		SourceMetadata: []byte("{}"), MatchedContactID: c,
	})
	require.NoError(t, err)
	method := judgment.MethodRef{Key: "prod-method", Version: "v1", WordingVersion: "w1"}
	run, err := e.store.StartRun(e.ctx, judgment.RunSpec{Kind: judgment.RunKindScheduled, Method: method})
	require.NoError(t, err)
	_, err = e.store.RecordJudgment(e.ctx, run, judgment.Judgment{
		Method: method, Thread: thread, Kind: threadctx.KindConversation, CutOff: at, JudgedAt: at,
		InputHash: "h1", Outcomes: outcomes,
	})
	require.NoError(t, err)
	return thread
}

func (e *obligationEnv) openObligations(t *testing.T, thread threadctx.VenueID) []judgment.Obligation {
	t.Helper()
	all, err := e.store.Obligations(e.ctx, thread)
	require.NoError(t, err)
	var open []judgment.Obligation
	for _, o := range all {
		if o.Open {
			open = append(open, o)
		}
	}
	return open
}

// finalize gives the task its Todoist ID, as the create op's finalize does.
func (e *obligationEnv) finalize(t *testing.T, taskID uuid.UUID) string {
	t.Helper()
	external := "todoist-" + uuid.NewString()[:8]
	tx, err := e.pool.Begin(e.ctx)
	require.NoError(t, err)
	_, err = e.tasks.UpdateContactTaskExternalIDTx(e.ctx, tx, taskID, external)
	require.NoError(t, err)
	require.NoError(t, tx.Commit(e.ctx))
	return external
}

// sync runs one Todoist sync whose feed delivers the item.
func (e *obligationEnv) sync(t *testing.T, item todoist.SyncItem) {
	t.Helper()
	e.feed.deliver(item)
	account := "account-" + uuid.NewString()[:8]
	_, err := e.provider.Sync(e.ctx, &repository.SyncState{ID: uuid.New(), Source: todoist.SourceName, AccountID: &account,
		Metadata: map[string]any{todoist.MetadataKeyProjectID: "project", todoist.MetadataKeyLabelID: "label", todoist.MetadataKeyLabelName: obligationLabel},
	}, nil)
	require.NoError(t, err)
}

type fakeAuth struct{}

func (fakeAuth) GetAccessToken(context.Context, string) (string, error) { return "token", nil }
func (fakeAuth) HasAnyAccount(context.Context) bool                     { return true }

// fakeFeed is a Todoist account whose next read delivers the queued items and
// which accepts every command.
type fakeFeed struct {
	mu    sync.Mutex
	items []todoist.SyncItem
}

func (f *fakeFeed) deliver(item todoist.SyncItem) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items = append(f.items, item)
}

func (f *fakeFeed) QuickAdd(context.Context, string, string) (*todoist.QuickAddTask, error) {
	return nil, errors.New("quick add is not used")
}

func (f *fakeFeed) Sync(_ context.Context, _ string, _ []string, commands []todoist.SyncCommand) (*todoist.SyncResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	resp := &todoist.SyncResponse{SyncToken: "token-" + uuid.NewString()[:8], TempIDMap: map[string]string{}}
	for _, cmd := range commands {
		if cmd.TempID != "" {
			resp.TempIDMap[cmd.TempID] = "real-" + cmd.TempID
		}
	}
	if commands == nil {
		resp.Items, f.items = f.items, nil
	}
	return resp, nil
}
