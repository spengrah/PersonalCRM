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
	"personal-crm/backend/internal/contacttask"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/judgment"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	"personal-crm/backend/internal/synthetic"
	"personal-crm/backend/internal/synthetic/factory"
	"personal-crm/backend/internal/threadctx"
	"personal-crm/backend/internal/todoist"
	"personal-crm/backend/tests/testsupport"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
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

			item := todoist.SyncItem{ID: external, Labels: []string{obligationLabel}, Deadline: &todoist.SyncDate{Date: "2030-01-01"}}
			tc.act(&item)
			e.sync(t, item)

			task, err := e.tasks.GetContactTask(e.ctx, open[0].TaskID)
			require.NoError(t, err)
			assert.Equal(t, tc.state, task.State)
			assert.Empty(t, e.openObligations(t, th), "the obligation closed")
			if tc.outreach {
				published := e.bus.completions()
				require.Len(t, published, 1)
				assert.Equal(t, c, published[0].ContactID)
				assert.Equal(t, repository.InteractionDirectionOutbound, published[0].Direction)
			} else {
				assert.Empty(t, e.bus.completions(), "no correspondence is recorded")
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
// tasks through the Todoist provider, with the Todoist API faked.
type obligationEnv struct {
	ctx      context.Context
	harness  *synthetic.Harness
	store    *judgment.Store
	tasks    *repository.ContactTaskRepository
	comms    *repository.CommsMessageRepository
	venues   *repository.VenueRepository
	provider *todoist.CadenceSyncProvider
	pool     *pgxpool.Pool
	bus      *completionRecorder
	feed     *fakeFeed
}

func newObligationEnv(t *testing.T) *obligationEnv {
	t.Helper()
	database := testsupport.OpenDatabase(t)
	ctx := t.Context()
	jobs, err := river.NewClient(riverpgxv5.New(database.Pool), &river.Config{SkipUnknownJobCheck: true})
	require.NoError(t, err)
	contactRepo := repository.NewContactRepository(database.Queries)
	contactRepo.SetPool(database.Pool)
	nodes := repository.NewNodeRepository(database.Queries)
	asserts := service.NewAssertService(database.Pool, nodes, repository.NewEntityRepository(database.Queries),
		repository.NewPredicateRepository(database.Queries), repository.NewAssertionRepository(database.Queries),
		events.NewBus(database.Pool, jobs, repository.NewEventRepository(database.Queries)))
	cadence := consumer.NewCadenceUpdater(repository.NewEventConsumerClaimRepository(database.Queries), contactRepo,
		database.Queries, consumer.CadenceModeCutover, false, config.TestConfig().Watchdog)
	venues := repository.NewVenueRepository(database.Queries)
	store := judgment.NewStore(judgment.StoreDeps{
		Pool: database.Pool, Queries: database.Queries, Assert: asserts,
		Contexts: threadctx.NewBuilder(repository.NewThreadContentRepository(database.Queries), venues),
		Cadence:  cadence, Jobs: jobs, Watchdog: config.TestConfig().Watchdog, ProductionMethod: "prod-method",
	})
	tasks := repository.NewContactTaskRepository(database.Queries)
	bus := &completionRecorder{}
	feed := &fakeFeed{}
	provider := todoist.NewCadenceSyncProvider(fakeAuth{}, tasks, contactRepo, repository.NewSyncRepository(database.Queries),
		config.TestConfig(), bus, cadence, database.Pool, func(string) todoist.Client { return feed }, jobs, false)
	return &obligationEnv{
		ctx:      ctx,
		harness:  synthetic.NewHarnessForNamespace(t, ctx, database, "o"+uuid.NewString()[:8], factory.DefaultSeed),
		store:    store,
		tasks:    tasks,
		comms:    repository.NewCommsMessageRepository(database.Queries),
		venues:   venues,
		provider: provider,
		pool:     database.Pool,
		bus:      bus,
		feed:     feed,
	}
}

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

// completionRecorder records the task.completed events the provider publishes.
type completionRecorder struct {
	mu        sync.Mutex
	completed []events.TaskCompletedPayload
}

func (r *completionRecorder) PublishTx(_ context.Context, _ pgx.Tx, env *events.Envelope) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	env.ID = uuid.New()
	if env.Kind == events.KindTaskCompleted {
		var p events.TaskCompletedPayload
		if err := events.Unmarshal(env, &p); err != nil {
			return err
		}
		r.completed = append(r.completed, p)
	}
	return nil
}

func (r *completionRecorder) completions() []events.TaskCompletedPayload {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]events.TaskCompletedPayload(nil), r.completed...)
}
