//go:build integration_testdb

// End-to-end coverage of the bench's negative-decision labels as crm-admin
// reads them: --scorecard counts labels Confirm Nones, inference on a close
// and an Add after a Confirm Nones recorded, and --infer-negative-labels
// brings proposals reviewed before the labeling rule existed under it.
package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/consumer/consumerjobs"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/decision"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/judgerun"
	"personal-crm/backend/internal/judgment"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/scorecard"
	"personal-crm/backend/internal/service"
	"personal-crm/backend/internal/synthetic"
	"personal-crm/backend/internal/synthetic/factory"
	"personal-crm/backend/internal/testdb"
	"personal-crm/backend/internal/threadctx"
	"personal-crm/backend/tests/testsupport"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	labelsProd  = judgment.MethodRef{Key: "jev-masked", Version: "v1", WordingVersion: "w1"}
	labelsOther = judgment.MethodRef{Key: "other-method", Version: "v1", WordingVersion: "w1"}
)

// labelsEnv is the judgment store over an isolated clone, as crm-admin wires
// it, with what a test needs to seed judged threads.
type labelsEnv struct {
	ctx      context.Context
	database *db.Database
	cfg      *config.Config
	store    *judgment.Store
	asserts  *service.AssertService
	contexts *threadctx.Builder
	contacts *repository.ContactRepository
	harness  *synthetic.Harness
	base     time.Time
}

type noopTaskOps struct {
	river.WorkerDefaults[consumerjobs.TodoistTaskOpArgs]
}

func (*noopTaskOps) Work(context.Context, *river.Job[consumerjobs.TodoistTaskOpArgs]) error {
	return nil
}

type noopKnowledge struct {
	river.WorkerDefaults[consumerjobs.KnowledgeCacheUpdaterJobArgs]
}

func (*noopKnowledge) Work(context.Context, *river.Job[consumerjobs.KnowledgeCacheUpdaterJobArgs]) error {
	return nil
}

func newLabelsEnv(t *testing.T) *labelsEnv {
	t.Helper()
	return newLabelsEnvFor(t, labelsProd.Key)
}

// newLabelsEnvFor is the env whose store designates productionMethod ("" for
// none).
func newLabelsEnvFor(t *testing.T, productionMethod string) *labelsEnv {
	t.Helper()
	url, drop := testdb.NewEphemeralClone(t)
	t.Cleanup(drop)
	database := testsupport.OpenDatabaseAt(t, url)
	ctx := t.Context()
	workers := river.NewWorkers()
	river.AddWorker(workers, &noopTaskOps{})
	river.AddWorker(workers, &noopKnowledge{})
	jobs, err := river.NewClient(riverpgxv5.New(database.Pool), &river.Config{
		Queues: map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 1}}, Workers: workers, TestOnly: true,
	})
	require.NoError(t, err)
	bus := events.NewBus(database.Pool, jobs, repository.NewEventRepository(database.Queries))
	asserts := service.NewAssertService(database.Pool, repository.NewNodeRepository(database.Queries),
		repository.NewEntityRepository(database.Queries), repository.NewPredicateRepository(database.Queries),
		repository.NewAssertionRepository(database.Queries), bus)
	contacts := repository.NewContactRepository(database.Queries)
	contacts.SetPool(database.Pool)
	contexts := threadctx.NewBuilder(repository.NewThreadContentRepository(database.Queries), repository.NewVenueRepository(database.Queries))
	cfg := config.TestConfig()
	cfg.Decision.ProductionMethod = productionMethod
	return &labelsEnv{
		ctx: ctx, database: database, cfg: cfg,
		store:    buildJudgmentStore(cfg, database, asserts, contacts, jobs, contexts),
		asserts:  asserts,
		contexts: contexts,
		contacts: contacts,
		harness:  synthetic.NewHarnessForNamespace(t, ctx, database, "l"+uuid.NewString()[:8], factory.DefaultSeed),
		base:     accelerated.GetCurrentTime().UTC().Add(-90 * 24 * time.Hour).Truncate(time.Second),
	}
}

// judged is one email thread with one message from a synthetic contact, and
// its content at a cut-off after that message.
type judged struct {
	contact uuid.UUID
	context threadctx.Context
}

func (e *labelsEnv) thread(t *testing.T, day int) judged {
	t.Helper()
	c, err := e.harness.SeedContact(e.ctx, e.harness.Generator().Contact())
	require.NoError(t, err)
	container := "thread-" + uuid.NewString()
	tx, err := e.database.Pool.Begin(e.ctx)
	require.NoError(t, err)
	venues := repository.NewVenueRepository(e.database.Queries)
	venue, err := venues.ResolveVenueForInteraction(e.ctx, tx, repository.InteractionSourceEmail, repository.VenueKindEmailThread, container, "")
	require.NoError(t, err)
	require.NoError(t, tx.Commit(e.ctx))
	sent := e.base.Add(time.Duration(day) * 24 * time.Hour)
	body, peer := "synthetic message body", "peer-"+c.ID.String()+"@example.test"
	_, err = repository.NewCommsMessageRepository(e.database.Queries).UpsertMessage(e.ctx, repository.UpsertCommsMessageParams{
		Source: repository.InteractionSourceEmail, ExternalID: uuid.NewString(), ThreadID: &container,
		Body: &body, PeerHandle: &peer, PeerNormalized: &peer, Direction: repository.InteractionDirectionInbound, SentAt: sent,
		SourceMetadata: []byte("{}"), MatchedContactID: c.ID,
	})
	require.NoError(t, err)
	built, err := e.contexts.Build(e.ctx, venue, sent.Add(time.Minute))
	require.NoError(t, err)
	return judged{contact: c.ID, context: built}
}

// record records a judgment of the thread's content by the method, in a run
// of the method's kind (scheduled for the production method).
func (e *labelsEnv) record(t *testing.T, m judgment.MethodRef, th judged, outcomes judgment.Outcomes) uuid.UUID {
	t.Helper()
	kind := judgment.RunKindRerun
	if m == labelsProd {
		kind = judgment.RunKindScheduled
	}
	run, err := e.store.StartRun(e.ctx, judgment.RunSpec{Kind: kind, Method: m})
	require.NoError(t, err)
	_, err = e.store.RecordJudgment(e.ctx, run, judgment.Judgment{
		Method: m, Thread: th.context.Thread, Kind: th.context.Kind, CutOff: th.context.CutOff, JudgedAt: th.context.CutOff,
		InputHash: th.context.InputHash, Outcomes: outcomes,
		Questions: []judgment.AnsweredQuestion{{ID: "big_moment", Probabilities: map[string]float64{"none": 1}}},
	})
	require.NoError(t, err)
	views, err := e.store.ListJudgments(e.ctx, judgment.JudgmentQuery{Thread: &th.context.Thread})
	require.NoError(t, err)
	for _, v := range views {
		if v.Method == m {
			return v.ID
		}
	}
	require.FailNow(t, "judgment not recorded")
	return uuid.Nil
}

// outcome returns the one outcome of the method's judgment of the thread on
// the question.
func (e *labelsEnv) outcome(t *testing.T, m judgment.MethodRef, th judged, question judgment.Question) uuid.UUID {
	t.Helper()
	views, err := e.store.ListJudgments(e.ctx, judgment.JudgmentQuery{Thread: &th.context.Thread})
	require.NoError(t, err)
	for _, v := range views {
		for _, o := range v.Outcomes {
			if v.Method == m && o.Question == question {
				return o.ID
			}
		}
	}
	require.FailNow(t, "no outcome", "%s %s", m.Key, question)
	return uuid.Nil
}

// reviewedBefore records a review the way the bench recorded one before the
// labeling rule existed: the review alone, with no rule run after it.
func (e *labelsEnv) reviewedBefore(t *testing.T, outcomeID uuid.UUID, right string) {
	t.Helper()
	locator := service.ProvenanceLocator{SourceKind: repository.SourceKindUser, SourceID: uuid.NewString(), ProducerKind: repository.ProducerKindUser}
	keep := false
	_, err := e.asserts.Assert(e.ctx, service.AssertRequest{SubjectNodeID: outcomeID, PredicateKey: "judgment_review", ValueText: &right, Confidence: 100, Locators: []service.ProvenanceLocator{locator}})
	require.NoError(t, err)
	_, err = e.asserts.Assert(e.ctx, service.AssertRequest{SubjectNodeID: outcomeID, PredicateKey: "judgment_review_keeps_date", ValueBool: &keep, Confidence: 100, Locators: []service.ProvenanceLocator{locator}})
	require.NoError(t, err)
}

// labels reads every label as the scorecard reads it, keyed by thread,
// question and contact.
func (e *labelsEnv) labels(t *testing.T) map[string]string {
	t.Helper()
	examples, err := e.store.ReviewedExamples(e.ctx)
	require.NoError(t, err)
	out := map[string]string{}
	for _, ex := range examples {
		for _, r := range ex.Reviews {
			contact := ""
			if r.ContactID != nil {
				contact = r.ContactID.String()
			}
			out[ex.Thread.String()+"/"+r.Question+"/"+contact] = r.RightOutcome
		}
	}
	return out
}

func labelKey(th judged, question judgment.Question, contact *uuid.UUID) string {
	c := ""
	if contact != nil {
		c = contact.String()
	}
	return th.context.Thread.String() + "/" + string(question) + "/" + c
}

func nothing(x uuid.UUID) judgment.Outcomes {
	return judgment.Outcomes{
		BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{{QuestionID: "ball_in_court/THEM", ContactID: x, Outcome: judgment.BallInCourtNobodyOwes, Confidence: 0.8}},
		Commitments: []judgment.CommitmentOutcome{
			{QuestionID: "commitment_mine/THEM", ContactID: x, Direction: judgment.CommitmentDirectionMine, Outcome: judgment.CommitmentStateNone, Confidence: 0.9},
		},
		BigMoment: judgment.Scored[judgment.BigMoment]{QuestionID: "big_moment", Outcome: judgment.BigMomentNone, Confidence: 0.9},
	}
}

// reusingMethod is a registry of the production method whose rerun reuses
// the judgments it already holds; a call to judge fails the rerun.
type reusingMethod struct{}

func (reusingMethod) Ref() judgment.MethodRef { return labelsProd }

// Plan reads what the judgments recorded without a window read: the whole
// context, so the rerun finds them.
func (reusingMethod) Plan(context.Context, decision.Request) (*decision.Plan, error) {
	return &decision.Plan{}, nil
}
func (reusingMethod) Judge(context.Context, *decision.Plan) (decision.Result, error) {
	return nil, errors.New("the scorecard rerun reuses every judgment and sends nothing")
}
func (reusingMethod) Measure(context.Context, threadctx.Context) (decision.Size, error) {
	return decision.Size{}, nil
}
func (reusingMethod) Terms() decision.TermsStatus { return decision.TermsStatus{} }

type reusingMethods struct{}

func (reusingMethods) Get(string) (decision.Method, error) { return reusingMethod{}, nil }
func (reusingMethods) Keys() []string                      { return []string{labelsProd.Key} }

// confusion returns the confusion lines the scorecard prints under an
// outcome, trimmed.
func confusion(out, outcome string) []string {
	var lines []string
	in := false
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "  outcome="+outcome+" "):
			in = true
		case in && strings.HasPrefix(line, "    reviewed="):
			lines = append(lines, strings.TrimSpace(line))
		case in:
			return lines
		}
	}
	return lines
}

// The scorecard counts the labels Confirm Nones, inference on a close and an
// Add after a Confirm Nones record as reviewed decisions, as it counts clicked
// labels.
func TestScorecardCountsNegativeLabels(t *testing.T) {
	e := newLabelsEnv(t)
	confirmed, inferred, added := e.thread(t, 1), e.thread(t, 2), e.thread(t, 3)
	require.NoError(t, e.store.ConfirmNones(e.ctx, e.record(t, labelsProd, confirmed, nothing(confirmed.contact))))
	owed := nothing(inferred.contact)
	owed.BallInCourt[0].Outcome = judgment.BallInCourtIOweReply
	inferredJudgment := e.record(t, labelsProd, inferred, owed)
	_, err := e.store.ReviewOutcome(e.ctx, e.outcome(t, labelsProd, inferred, judgment.QuestionBallInCourt), judgment.ReviewRequest{RightOutcome: "i_owe_reply"})
	require.NoError(t, err)
	require.NoError(t, e.store.CloseRow(e.ctx, []uuid.UUID{inferredJudgment}))
	require.NoError(t, e.store.ConfirmNones(e.ctx, e.record(t, labelsProd, added, nothing(added.contact))))
	_, err = e.store.ReviewOutcome(e.ctx, e.outcome(t, labelsProd, added, judgment.QuestionBallInCourt), judgment.ReviewRequest{RightOutcome: "i_owe_reply"})
	require.NoError(t, err)
	runner, err := judgerun.New(e.contexts, e.store, reusingMethods{})
	require.NoError(t, err)
	var stdout bytes.Buffer

	err = run(e.ctx, runOptions{scorecard: true, judgeMethod: labelsProd.Key}, adminDeps{
		scorecard: scorecard.New(e.store, runner, reusingMethods{}, e.contacts, e.cfg.Watchdog),
		stdout:    &stdout, stderr: &bytes.Buffer{},
	})

	require.NoError(t, err)
	out := stdout.String()
	assert.Contains(t, out, "scorecard: reviewed_examples=3\n")
	assert.Equal(t, []string{
		"reviewed=i_owe_reply judged=i_owe_reply n=1",
		"reviewed=i_owe_reply judged=nobody_owes n=1",
		"reviewed=nobody_owes judged=nobody_owes n=1",
	}, confusion(out, "ball_in_court.nobody_owes"), "the Confirm Nones label, the inferred item's and the Add's")
	assert.Equal(t, []string{"reviewed=none judged=none n=3"}, confusion(out, "commitment_mine.none"), "confirmed twice and inferred once")
	assert.Equal(t, []string{"reviewed=none judged=none n=3"}, confusion(out, "big_moment.none"))
}

// --infer-negative-labels labels the negative decisions of every proposal
// that held a label before the labeling rule existed, as a close of its row
// would, and nothing else.
func TestInferNegativeLabels(t *testing.T) {
	e := newLabelsEnv(t)
	// A proposal whose one item was labeled before the rule, beside another
	// method's proposal that labeled the same contact's reply differently.
	reviewed := e.thread(t, 1)
	committed := nothing(reviewed.contact)
	committed.Commitments[0].Outcome = judgment.CommitmentStateOpen
	e.record(t, labelsProd, reviewed, committed)
	e.record(t, labelsOther, reviewed, judgment.Outcomes{
		BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{{QuestionID: "ball_in_court/THEM", ContactID: reviewed.contact, Outcome: judgment.BallInCourtIOweReply, Confidence: 0.6}},
		BigMoment:   judgment.Scored[judgment.BigMoment]{QuestionID: "big_moment", Outcome: judgment.BigMomentNone, Confidence: 0.6},
	})
	e.reviewedBefore(t, e.outcome(t, labelsProd, reviewed, judgment.QuestionCommitmentMine), "open")
	e.reviewedBefore(t, e.outcome(t, labelsOther, reviewed, judgment.QuestionBallInCourt), "not_applicable")
	// A proposal with no items whose negative decision another method's
	// proposal labeled.
	itemless := e.thread(t, 2)
	e.record(t, labelsProd, itemless, nothing(itemless.contact))
	other := nothing(itemless.contact)
	other.BallInCourt[0].Outcome = judgment.BallInCourtTheyOweReply
	e.record(t, labelsOther, itemless, other)
	e.reviewedBefore(t, e.outcome(t, labelsOther, itemless, judgment.QuestionBallInCourt), "nobody_owes")
	// A proposal holding no label on content another method's proposal
	// labeled a decision of.
	unlabeled := e.thread(t, 3)
	e.record(t, labelsProd, unlabeled, nothing(unlabeled.contact))
	e.record(t, labelsOther, unlabeled, judgment.Outcomes{Commitments: []judgment.CommitmentOutcome{
		{QuestionID: "commitment_theirs/THEM", ContactID: unlabeled.contact, Direction: judgment.CommitmentDirectionTheirs, Outcome: judgment.CommitmentStateOpen, Confidence: 0.9},
	}})
	e.reviewedBefore(t, e.outcome(t, labelsOther, unlabeled, judgment.QuestionCommitmentTheirs), "open")
	before := e.labels(t)
	infer := func() string {
		var stdout bytes.Buffer
		require.NoError(t, run(e.ctx, runOptions{inferNegativeLabels: true}, adminDeps{negativeLabels: e.store, stdout: &stdout, stderr: &bytes.Buffer{}}))
		return stdout.String()
	}

	first := infer()
	after := e.labels(t)
	second := infer()

	x, y, z := reviewed.contact, itemless.contact, unlabeled.contact
	assert.Equal(t, "infer-negative-labels: labeled=3\n", first)
	for key, right := range before {
		assert.Equal(t, right, after[key], "an existing label never changes: %s", key)
	}
	assert.Equal(t, "none", after[labelKey(reviewed, judgment.QuestionBigMoment, nil)], "the reviewed proposal's unlabeled negative")
	assert.Equal(t, "not_applicable", after[labelKey(reviewed, judgment.QuestionBallInCourt, &x)], "labeled differently in another proposal, and kept")
	assert.Equal(t, "open", after[labelKey(reviewed, judgment.QuestionCommitmentMine, &x)])
	assert.Equal(t, "none", after[labelKey(itemless, judgment.QuestionCommitmentMine, &y)], "a proposal holding another proposal's label")
	assert.Equal(t, "none", after[labelKey(itemless, judgment.QuestionBigMoment, nil)])
	for _, key := range []string{
		labelKey(unlabeled, judgment.QuestionBallInCourt, &z),
		labelKey(unlabeled, judgment.QuestionCommitmentMine, &z),
		labelKey(unlabeled, judgment.QuestionBigMoment, nil),
	} {
		assert.NotContains(t, after, key, "a proposal holding no label gains none")
	}
	assert.Len(t, after, len(before)+3)
	assert.Equal(t, "infer-negative-labels: labeled=0\n", second)
	assert.Equal(t, after, e.labels(t))
}
