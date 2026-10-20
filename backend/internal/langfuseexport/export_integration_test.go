//go:build integration_testdb

package langfuseexport_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/consumer/consumerjobs"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/judgment"
	"personal-crm/backend/internal/langfuseexport"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/scorecard"
	"personal-crm/backend/internal/service"
	"personal-crm/backend/internal/synthetic"
	"personal-crm/backend/internal/synthetic/factory"
	"personal-crm/backend/internal/threadctx"
	"personal-crm/backend/tests/testsupport"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var method = judgment.MethodRef{Key: "export-method", Version: "v1", WordingVersion: "w1"}

// unusedCadence and unusedJobs stand in for the store's live effects, which a
// historical run with no production method never reaches.
type unusedCadence struct{}

func (unusedCadence) EndAwaitingReplyTx(context.Context, pgx.Tx, uuid.UUID) error { return nil }

type unusedJobs struct{}

func (unusedJobs) InsertTx(context.Context, pgx.Tx, river.JobArgs, *river.InsertOpts) (*rivertype.JobInsertResult, error) {
	return nil, errors.New("no job is inserted")
}

// langfuse is Langfuse at its HTTP API: it holds no traces, records every
// request body it receives and accepts every score deletion.
type langfuse struct {
	server *httptest.Server
	mu     sync.Mutex
	bodies []string
	spans  map[string]map[string]any // trace name -> trace metadata
	judged map[string]map[string]any // judgment cut-off -> trace metadata
	// held are the traces received, which the observations list returns.
	held []string
	// verdicts are the trace and score IDs of every verdict score received.
	verdicts []string
	// judgmentIDs are the trace and span IDs of every judgment span received.
	judgmentIDs []string
}

func newLangfuse(t *testing.T) *langfuse {
	t.Helper()
	l := &langfuse{spans: map[string]map[string]any{}, judged: map[string]map[string]any{}}
	l.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		l.mu.Lock()
		defer l.mu.Unlock()
		l.bodies = append(l.bodies, string(raw))
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		switch r.URL.Path {
		case "/api/public/v2/observations":
			data := make([]map[string]string, len(l.held))
			for i, trace := range l.held {
				data[i] = map[string]string{"traceId": trace}
			}
			require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": data, "meta": map[string]any{}}))
		case "/api/public/otel/v1/traces":
			l.keepSpans(t, raw)
			_, _ = w.Write([]byte(`{}`))
		default:
			l.keepScores(t, raw)
			_, _ = w.Write([]byte(`{"errors":[]}`))
		}
	}))
	t.Cleanup(l.server.Close)
	return l
}

// keepSpans keeps each received span's trace metadata, by trace name.
func (l *langfuse) keepSpans(t *testing.T, raw []byte) {
	var req struct {
		ResourceSpans []struct {
			ScopeSpans []struct {
				Spans []struct {
					TraceID    string `json:"traceId"`
					SpanID     string `json:"spanId"`
					Attributes []struct {
						Key   string `json:"key"`
						Value struct {
							StringValue string `json:"stringValue"`
						} `json:"value"`
					} `json:"attributes"`
				} `json:"spans"`
			} `json:"scopeSpans"`
		} `json:"resourceSpans"`
	}
	require.NoError(t, json.Unmarshal(raw, &req))
	for _, rs := range req.ResourceSpans {
		for _, ss := range rs.ScopeSpans {
			for _, s := range ss.Spans {
				var name string
				var metadata map[string]any
				for _, a := range s.Attributes {
					switch a.Key {
					case "langfuse.trace.name":
						name = a.Value.StringValue
					case "langfuse.trace.metadata":
						require.NoError(t, json.Unmarshal([]byte(a.Value.StringValue), &metadata))
					}
				}
				l.spans[name] = metadata
				l.held = append(l.held, s.TraceID)
				if name == "judgment" {
					l.judgmentIDs = append(l.judgmentIDs, s.TraceID+"/"+s.SpanID)
				}
				if cutOff, ok := metadata["cut_off"].(string); ok {
					l.judged[cutOff] = metadata
				}
			}
		}
	}
}

// keepScores keeps each received verdict score's trace and score IDs.
func (l *langfuse) keepScores(t *testing.T, raw []byte) {
	var req struct {
		Batch []struct {
			Body struct {
				ID      string `json:"id"`
				TraceID string `json:"traceId"`
				Name    string `json:"name"`
			} `json:"body"`
		} `json:"batch"`
	}
	require.NoError(t, json.Unmarshal(raw, &req))
	for _, e := range req.Batch {
		if e.Body.Name == "verdict" {
			l.verdicts = append(l.verdicts, e.Body.TraceID+"/"+e.Body.ID)
		}
	}
}

// judgmentsByCutOff is the trace metadata of the judgment cut off at the time.
func (l *langfuse) judgmentsByCutOff(cutOff time.Time) map[string]any {
	return l.judged[cutOff.UTC().Format(time.RFC3339)]
}

// sent is everything the export sent, as one string.
func (l *langfuse) sent() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.bodies, "\n")
}

// newAsserter is the real assertion write path, over a River client that is
// never started: an insert lands its job, which never runs.
func newAsserter(t *testing.T, database *db.Database) judgment.Asserter {
	t.Helper()
	workers := river.NewWorkers()
	river.AddWorker(workers, &noopWorker{})
	jobs, err := river.NewClient(riverpgxv5.New(database.Pool), &river.Config{
		Queues:   map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 1}},
		Workers:  workers,
		TestOnly: true,
	})
	require.NoError(t, err)
	bus := events.NewBus(database.Pool, jobs, repository.NewEventRepository(database.Queries))
	return service.NewAssertService(database.Pool, repository.NewNodeRepository(database.Queries),
		repository.NewEntityRepository(database.Queries), repository.NewPredicateRepository(database.Queries),
		repository.NewAssertionRepository(database.Queries), bus)
}

type noopWorker struct {
	river.WorkerDefaults[consumerjobs.KnowledgeCacheUpdaterJobArgs]
}

func (*noopWorker) Work(context.Context, *river.Job[consumerjobs.KnowledgeCacheUpdaterJobArgs]) error {
	return nil
}

// exportEnv is a judgment store over a real database, with a thread of
// messages from one contact.
type exportEnv struct {
	store    *judgment.Store
	langfuse *langfuse
	contact  uuid.UUID
	thread   uuid.UUID
	// identifying is what the export must not carry: the messages' text and
	// sender, the contact's and thread's identity and every raw message key.
	identifying []string
	keys        []string
}

func newExportEnv(t *testing.T, messages int) *exportEnv {
	t.Helper()
	ctx := t.Context()
	database := testsupport.OpenDatabase(t)
	harness := synthetic.NewHarnessForNamespace(t, ctx, database, "x"+uuid.NewString()[:8], factory.DefaultSeed)
	seeded, err := harness.SeedContact(ctx, harness.Generator().Contact())
	require.NoError(t, err)

	venues := repository.NewVenueRepository(database.Queries)
	container := "thread-" + uuid.NewString()
	tx, err := database.Pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	thread, err := venues.ResolveVenueForInteraction(ctx, tx, repository.InteractionSourceEmail, repository.VenueKindEmailThread, container, "")
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))

	text, peer := "synthetic message text", "peer-"+seeded.ID.String()+"@example.test"
	comms := repository.NewCommsMessageRepository(database.Queries)
	base := accelerated.GetCurrentTime().UTC().Add(-90 * 24 * time.Hour).Truncate(time.Second)
	for i := range messages {
		_, err := comms.UpsertMessage(ctx, repository.UpsertCommsMessageParams{
			Source: repository.InteractionSourceEmail, ExternalID: uuid.NewString(), ThreadID: &container,
			Body: &text, PeerHandle: &peer, PeerNormalized: &peer, Direction: repository.InteractionDirectionInbound,
			SentAt: base.Add(time.Duration(i) * time.Hour), SourceMetadata: []byte("{}"), MatchedContactID: seeded.ID,
		})
		require.NoError(t, err)
	}
	rows, err := repository.NewThreadContentRepository(database.Queries).ListThreadContent(ctx, thread, accelerated.GetCurrentTime())
	require.NoError(t, err)
	var keys []string
	for _, r := range rows {
		if len(keys) == 0 || keys[len(keys)-1] != r.MessageKey {
			keys = append(keys, r.MessageKey)
		}
	}
	require.Len(t, keys, messages)

	store := judgment.NewStore(judgment.StoreDeps{
		Pool: database.Pool, Queries: database.Queries,
		Assert:   newAsserter(t, database),
		Contexts: threadctx.NewBuilder(repository.NewThreadContentRepository(database.Queries), venues),
		Cadence:  unusedCadence{}, Jobs: unusedJobs{},
		Watchdog: config.TestConfig().Watchdog,
	})
	return &exportEnv{
		store: store, langfuse: newLangfuse(t), contact: seeded.ID, thread: thread, keys: keys,
		identifying: append([]string{text, peer, seeded.ID.String(), thread.String(), container}, keys...),
	}
}

// judgmentAt builds a judgment that read the window starting at the first
// message and answered for the new messages.
func (e *exportEnv) judgmentAt(cutOff time.Time, newMessages []string) judgment.Judgment {
	return judgment.Judgment{
		Method: method, Thread: e.thread, Kind: threadctx.KindConversation,
		CutOff: cutOff, JudgedAt: cutOff.Add(time.Hour), InputHash: "hash-" + cutOff.Format(time.RFC3339),
		WindowStart: e.keys[0], NewMessages: newMessages,
		Usage: judgment.Usage{ContextTokens: 420, Questions: 3, Latency: 250 * time.Millisecond},
	}
}

// record records the judgments and the unjudged messages in one run and
// finishes it.
func (e *exportEnv) record(t *testing.T, judgments []judgment.Judgment, unjudged []judgment.UnjudgedMessage) {
	t.Helper()
	ctx := t.Context()
	run, err := e.store.StartRun(ctx, judgment.RunSpec{Kind: judgment.RunKindHistorical, Method: method})
	require.NoError(t, err)
	for _, j := range judgments {
		_, err := e.store.RecordJudgment(ctx, run, j)
		require.NoError(t, err)
	}
	if len(unjudged) > 0 {
		require.NoError(t, e.store.RecordUnjudged(ctx, run, unjudged))
	}
	require.NoError(t, e.store.FinishRun(ctx, run))
}

// export runs the export against the faked Langfuse.
func (e *exportEnv) export(t *testing.T) {
	t.Helper()
	exporter := langfuseexport.New(config.LangfuseExtractionConfig{Host: e.langfuse.server.URL, PublicKey: "pk", SecretKey: "sk"}, e.store, scorecard.NewReader(e.store))
	report, err := exporter.Export(t.Context())
	require.NoError(t, err)
	require.True(t, report.Sent())
}

// assertContentFree fails for each identifying string the export sent.
func (e *exportEnv) assertContentFree(t *testing.T) {
	t.Helper()
	sent := e.langfuse.sent()
	for _, s := range e.identifying {
		assert.NotContains(t, sent, s)
	}
}

func flag(key string, kind judgment.FlagKind, p float64) judgment.FlagDecision {
	return judgment.FlagDecision{MessageKey: key, Kind: kind, Probability: p}
}

// The export sends each judgment's window start, flag decisions and clearing
// decisions and each run's unjudged messages, naming messages as opaque
// candidates.
func TestExportSendsMessageFlagRecordsContentFree(t *testing.T) {
	t.Run("a judgment whose flags name messages", func(t *testing.T) {
		// spec: KNW-066.new-records-exported
		// spec: KNW-066.no-text-sender-or-contact-identity
		// spec: KNW-066.message-keys-opaque-candidates
		e := newExportEnv(t, 2)
		j := e.judgmentAt(accelerated.GetCurrentTime().UTC().Add(-48*time.Hour).Truncate(time.Second), e.keys)
		j.Flags = []judgment.FlagDecision{
			flag(e.keys[0], judgment.FlagKindCommitment, 0.8), flag(e.keys[0], judgment.FlagKindBigMoment, 0.1),
			flag(e.keys[1], judgment.FlagKindClaim, 0.6),
		}
		e.record(t, []judgment.Judgment{j}, nil)

		e.export(t)

		meta := e.langfuse.spans["judgment"]
		require.NotNil(t, meta)
		flags, ok := meta["flags"].([]any)
		require.True(t, ok, "flags %v", meta["flags"])
		require.Len(t, flags, 3)
		first, second := flags[0].(map[string]any), flags[2].(map[string]any)
		assert.Equal(t, "commitment", first["kind"])
		assert.InDelta(t, 0.8, first["probability"], 1e-9)
		assert.Equal(t, "big_moment", flags[1].(map[string]any)["kind"])
		assert.Equal(t, "claim", second["kind"])
		assert.Regexp(t, `^candidate_\d+$`, first["message"])
		assert.Equal(t, first["message"], flags[1].(map[string]any)["message"], "one message is one candidate")
		assert.NotEqual(t, first["message"], second["message"])
		assert.Equal(t, first["message"], meta["window_start"], "the window starts at the first message")
		e.assertContentFree(t)
	})

	t.Run("a judgment with a clearing decision", func(t *testing.T) {
		// spec: KNW-066.new-records-exported
		// spec: KNW-066.no-text-sender-or-contact-identity
		// spec: KNW-066.message-keys-opaque-candidates
		e := newExportEnv(t, 2)
		now := accelerated.GetCurrentTime().UTC().Truncate(time.Second)
		earlier := e.judgmentAt(now.Add(-72*time.Hour), e.keys[:1])
		earlier.Flags = []judgment.FlagDecision{flag(e.keys[0], judgment.FlagKindCommitment, 0.9)}
		e.record(t, []judgment.Judgment{earlier}, nil)
		views, err := e.store.ListJudgments(t.Context(), judgment.JudgmentQuery{Thread: &e.thread})
		require.NoError(t, err)
		require.Len(t, views, 1)
		later := e.judgmentAt(now.Add(-48*time.Hour), e.keys[1:])
		later.Flags = []judgment.FlagDecision{flag(e.keys[1], judgment.FlagKindClaim, 0.3)}
		later.Clearings = []judgment.ClearingDecision{{CommitmentMessageKey: e.keys[0], CommitmentJudgment: views[0].ID, Done: true, Probability: 0.85}}
		e.record(t, []judgment.Judgment{later}, nil)

		e.export(t)

		clearings := e.langfuse.judgmentsByCutOff(later.CutOff)["clearings"].([]any)
		require.Len(t, clearings, 1)
		cleared := clearings[0].(map[string]any)
		assert.Equal(t, true, cleared["done"])
		assert.InDelta(t, 0.85, cleared["probability"], 1e-9)
		assert.Regexp(t, `^candidate_\d+$`, cleared["message"])
		assert.NotContains(t, e.langfuse.sent(), views[0].ID.String(), "the earlier judgment's store identifier")
		e.assertContentFree(t)
	})

	t.Run("a run with an unjudged message", func(t *testing.T) {
		// spec: KNW-066.new-records-exported
		// spec: KNW-066.no-text-sender-or-contact-identity
		// spec: KNW-066.message-keys-opaque-candidates
		e := newExportEnv(t, 2)
		cutOff := accelerated.GetCurrentTime().UTC().Add(-48 * time.Hour).Truncate(time.Second)
		e.record(t, nil, []judgment.UnjudgedMessage{
			{Thread: e.thread, CutOff: cutOff, MessageKey: e.keys[0], Reason: judgment.UnjudgedReasonCallFailed},
			{Thread: e.thread, CutOff: cutOff, MessageKey: e.keys[1], Reason: judgment.UnjudgedReasonBeforeWindow},
		})

		e.export(t)

		unjudged, ok := e.langfuse.spans["judgment_run"]["unjudged"].([]any)
		require.True(t, ok, "unjudged %v", e.langfuse.spans["judgment_run"]["unjudged"])
		require.Len(t, unjudged, 2)
		reasons := map[string]string{}
		for _, u := range unjudged {
			m := u.(map[string]any)
			assert.Regexp(t, `^candidate_\d+$`, m["message"])
			assert.Equal(t, cutOff.Format(time.RFC3339), m["cut_off"])
			reasons[m["message"].(string)] = m["reason"].(string)
		}
		assert.ElementsMatch(t, []string{"call_failed", "before_window"}, []string{reasons["candidate_1"], reasons["candidate_2"]})
		e.assertContentFree(t)
	})
}

// Two judgments of one method version and example that read different windows
// are two records: the export sends the later one although the earlier one is
// held, under a different trace and span.
func TestExportDistinguishesJudgmentsOfOneExampleByRead(t *testing.T) {
	// spec: KNW-066.new-records-exported
	e := newExportEnv(t, 2)
	cutOff := accelerated.GetCurrentTime().UTC().Add(-48 * time.Hour).Truncate(time.Second)
	whole := e.judgmentAt(cutOff, e.keys)
	later := e.judgmentAt(cutOff, e.keys[1:])
	later.WindowStart = e.keys[1]
	e.record(t, []judgment.Judgment{whole}, nil)
	e.export(t)
	sentBefore := slices.Clone(e.langfuse.judgmentIDs)

	e.record(t, []judgment.Judgment{later}, nil)
	e.export(t)

	sentAfter := e.langfuse.judgmentIDs[len(sentBefore):]
	require.Len(t, sentAfter, 1, "the later read is sent")
	assert.NotContains(t, sentBefore, sentAfter[0], "under its own trace and span")
}

// Reviews of two judgments of one method version and example that read
// different windows are scores on their own judgments' traces, each under its
// own ID.
func TestExportAttachesReviewsToTheirJudgmentsRead(t *testing.T) {
	// spec: KNW-066.new-records-exported
	e := newExportEnv(t, 2)
	cutOff := accelerated.GetCurrentTime().UTC().Add(-48 * time.Hour).Truncate(time.Second)
	whole := e.judgmentAt(cutOff, e.keys)
	later := e.judgmentAt(cutOff, e.keys[1:])
	later.WindowStart = e.keys[1]
	for _, j := range []*judgment.Judgment{&whole, &later} {
		j.Outcomes.BallInCourt = []judgment.ParticipantOutcome[judgment.BallInCourt]{
			{QuestionID: "ball_in_court:" + e.contact.String()[:6], ContactID: e.contact, Outcome: judgment.BallInCourtIOweReply, Confidence: 0.9},
		}
	}
	e.record(t, []judgment.Judgment{whole}, nil)
	e.record(t, []judgment.Judgment{later}, nil)
	views, err := e.store.ListJudgments(t.Context(), judgment.JudgmentQuery{Thread: &e.thread})
	require.NoError(t, err)
	require.Len(t, views, 2)
	for _, v := range views {
		require.Len(t, v.Outcomes, 1)
		_, err := e.store.ReviewOutcome(t.Context(), v.Outcomes[0].ID, judgment.ReviewRequest{RightOutcome: string(judgment.BallInCourtNobodyOwes)})
		require.NoError(t, err)
	}

	e.export(t)

	require.Len(t, e.langfuse.verdicts, 2)
	assert.NotEqual(t, e.langfuse.verdicts[0], e.langfuse.verdicts[1])
	traces := map[string]bool{}
	for _, id := range e.langfuse.judgmentIDs {
		traces[strings.Split(id, "/")[0]] = true
	}
	for _, v := range e.langfuse.verdicts {
		assert.True(t, traces[strings.Split(v, "/")[0]], "a verdict is scored on a judgment's trace")
	}
	assert.NotEqual(t, strings.Split(e.langfuse.verdicts[0], "/")[0], strings.Split(e.langfuse.verdicts[1], "/")[0])
}

// A rerun's exported scorecard score counts the judgments the rerun appeared
// with: a judgment of the same method version and example over another window,
// recorded in another run, does not stand in for the one the rerun reused.
func TestExportScoresARerunOnTheJudgmentsItReused(t *testing.T) {
	// spec: KNW-053.rerun-reviewed-examples
	e := newExportEnv(t, 2)
	cutOff := accelerated.GetCurrentTime().UTC().Add(-48 * time.Hour).Truncate(time.Second)
	whole := e.judgmentAt(cutOff, e.keys)
	later := e.judgmentAt(cutOff, e.keys[1:])
	later.WindowStart = e.keys[1]
	answer := func(j *judgment.Judgment, outcome judgment.BallInCourt) {
		j.Outcomes.BallInCourt = []judgment.ParticipantOutcome[judgment.BallInCourt]{
			{QuestionID: "ball_in_court:" + e.contact.String()[:6], ContactID: e.contact, Outcome: outcome, Confidence: 0.9},
		}
	}
	answer(&whole, judgment.BallInCourtNobodyOwes)
	answer(&later, judgment.BallInCourtIOweReply)
	e.record(t, []judgment.Judgment{whole}, nil)
	e.record(t, []judgment.Judgment{later}, nil)
	views, err := e.store.ListJudgments(t.Context(), judgment.JudgmentQuery{Thread: &e.thread})
	require.NoError(t, err)
	for _, v := range views {
		if v.WindowStart == e.keys[0] {
			require.Len(t, v.Outcomes, 1)
			_, err := e.store.ReviewOutcome(t.Context(), v.Outcomes[0].ID, judgment.ReviewRequest{RightOutcome: string(judgment.BallInCourtNobodyOwes)})
			require.NoError(t, err)
		}
	}
	rerun, err := e.store.StartRun(t.Context(), judgment.RunSpec{Kind: judgment.RunKindRerun, Method: method})
	require.NoError(t, err)
	_, reused, err := e.store.ReuseRead(t.Context(), rerun, e.thread, cutOff, whole.InputHash, judgment.Read{WindowStart: e.keys[0], NewMessages: e.keys})
	require.NoError(t, err)
	require.True(t, reused)
	require.NoError(t, e.store.FinishRun(t.Context(), rerun))

	e.export(t)

	assert.Contains(t, e.langfuse.sent(), `"name":"corrections_caused","value":0`)
}
