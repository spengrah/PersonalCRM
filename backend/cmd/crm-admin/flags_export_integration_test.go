//go:build integration_testdb

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/judgment"
	"personal-crm/backend/internal/langfuseexport"
	"personal-crm/backend/internal/scorecard"
	"personal-crm/backend/internal/threadctx"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sentScore is one score the export sent, as Langfuse receives it.
type sentScore struct {
	ID       string         `json:"id"`
	TraceID  string         `json:"traceId"`
	Name     string         `json:"name"`
	Value    any            `json:"value"`
	DataType string         `json:"dataType"`
	Metadata map[string]any `json:"metadata"`
}

// fakeLangfuse is Langfuse at its HTTP API: it holds the traces it received,
// keeps every score by ID as Langfuse upserts it, and records every request
// body.
type fakeLangfuse struct {
	server *httptest.Server
	mu     sync.Mutex
	bodies []string
	held   []string
	// runTraces maps each run's ID to its trace.
	runTraces map[string]string
	scores    map[string]sentScore
}

func newFakeLangfuse(t *testing.T) *fakeLangfuse {
	t.Helper()
	l := &fakeLangfuse{runTraces: map[string]string{}, scores: map[string]sentScore{}}
	l.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		l.mu.Lock()
		defer l.mu.Unlock()
		l.bodies = append(l.bodies, string(raw))
		switch r.URL.Path {
		case "/api/public/v2/observations":
			data := make([]map[string]string, len(l.held))
			for i, trace := range l.held {
				data[i] = map[string]string{"traceId": trace}
			}
			writeTestJSON(w, map[string]any{"data": data, "meta": map[string]any{}})
		case "/api/public/otel/v1/traces":
			l.keepSpans(t, raw)
			writeTestJSON(w, map[string]any{})
		case "/api/public/ingestion":
			var req struct {
				Batch []struct {
					Body sentScore `json:"body"`
				} `json:"batch"`
			}
			if !assert.NoError(t, json.Unmarshal(raw, &req)) {
				return
			}
			for _, e := range req.Batch {
				l.scores[e.Body.ID] = e.Body
			}
			writeTestJSON(w, map[string]any{"errors": []any{}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(l.server.Close)
	return l
}

// keepSpans holds each received span's trace, and the trace of each run.
func (l *fakeLangfuse) keepSpans(t *testing.T, raw []byte) {
	var req struct {
		ResourceSpans []struct {
			ScopeSpans []struct {
				Spans []struct {
					TraceID    string `json:"traceId"`
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
				l.held = append(l.held, s.TraceID)
				for _, a := range s.Attributes {
					if a.Key != "langfuse.trace.metadata" {
						continue
					}
					var metadata map[string]any
					require.NoError(t, json.Unmarshal([]byte(a.Value.StringValue), &metadata))
					if run, ok := metadata["run_id"].(string); ok {
						l.runTraces[run] = s.TraceID
					}
				}
			}
		}
	}
}

// runScores returns the scores Langfuse holds on the run's trace, by name.
func (l *fakeLangfuse) runScores(t *testing.T, run judgment.RunID) map[string]sentScore {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	trace, ok := l.runTraces[run.String()]
	require.True(t, ok, "the run's trace was sent")
	out := map[string]sentScore{}
	for _, s := range l.scores {
		if s.TraceID == trace {
			_, dup := out[s.Name]
			require.False(t, dup, "one score per metric: %s", s.Name)
			out[s.Name] = s
		}
	}
	return out
}

// sent is every request body the export sent, as one string.
func (l *fakeLangfuse) sent() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.bodies, "\n")
}

// langfuseExport runs crm-admin --langfuse-export against the fake Langfuse.
func (e *flagsEnv) langfuseExport(t *testing.T, l *fakeLangfuse) string {
	t.Helper()
	var stdout bytes.Buffer
	exporter := langfuseexport.New(config.LangfuseExtractionConfig{Host: l.server.URL, PublicKey: "pk", SecretKey: "sk"}, e.store, scorecard.NewReader(e.store))
	require.NoError(t, run(e.ctx, runOptions{langfuseExport: true}, adminDeps{langfuseExport: exporter, stdout: &stdout, stderr: &bytes.Buffer{}}))
	return stdout.String()
}

// value is a numeric score's value.
func value(t *testing.T, scores map[string]sentScore, name string) float64 {
	t.Helper()
	s, ok := scores[name]
	require.True(t, ok, "score %s was sent", name)
	assert.Equal(t, "NUMERIC", s.DataType)
	v, ok := s.Value.(float64)
	require.True(t, ok, "score %s is a number: %v", name, s.Value)
	return v
}

// The export sends each scorecard metric of a rerun, flag and ball-in-court
// alike, as its own numeric score on the rerun's trace, content-free; each
// export replaces the run's earlier value of each metric, and a metric with
// no labeled decision behind it is not sent.
func TestLangfuseExportSendsEachScorecardMetric(t *testing.T) {
	t.Run("a rerun with flag and clearing labels", func(t *testing.T) {
		// spec: KNW-088.each-metric-own-score
		s := clearedThread(t, 0.9)
		s.label(t, 0, judgment.FlagKindCommitment, true)
		s.labelClearing(t, jan(5, 0), true)
		reviewed := s.judgmentAt(t, s.set[jevKey].Ref(), jan(5, 0))
		require.NotEmpty(t, reviewed.Outcomes)
		_, err := s.e.store.ReviewOutcome(s.e.ctx, reviewed.Outcomes[0].ID, judgment.ReviewRequest{RightOutcome: string(judgment.BallInCourtNobodyOwes)})
		require.NoError(t, err)
		s.scorecard(t)
		rerun := s.e.lastRun(t, s.set[jevKey].Ref())
		require.Equal(t, judgment.RunKindRerun, rerun.Kind)
		l := newFakeLangfuse(t)

		s.e.langfuseExport(t, l)

		scores := l.runScores(t, rerun.ID)
		assert.InDelta(t, 1, value(t, scores, "flag.commitment.precision"), 1e-9)
		assert.InDelta(t, 1, value(t, scores, "flag.commitment.recall"), 1e-9)
		assert.InDelta(t, 1, value(t, scores, "flag.commitment.shown_held"), 1e-9)
		assert.InDelta(t, 1, value(t, scores, "clearing.accuracy"), 1e-9)
		assert.InDelta(t, 1, value(t, scores, "clearing.done_labeled_done"), 1e-9)
		assert.InDelta(t, 2, value(t, scores, "messages.judged"), 1e-9)
		assert.InDelta(t, 0, value(t, scores, "messages.unjudged.call_failed"), 1e-9)
		assert.InDelta(t, 1, value(t, scores, "messages.flagged.commitment"), 1e-9)
		assert.InDelta(t, 0, value(t, scores, "messages.flagged.claim"), 1e-9)
		assert.InDelta(t, 1, value(t, scores, "outcome.ball_in_court.nobody_owes.recall"), 1e-9)
		assert.InDelta(t, 1, value(t, scores, "outcome.ball_in_court.nobody_owes.precision"), 1e-9)
		assert.InDelta(t, 1, value(t, scores, "confusion.ball_in_court.nobody_owes.nobody_owes"), 1e-9)
		assert.InDelta(t, 0, value(t, scores, "i_owe_missed"), 1e-9)
		assert.InDelta(t, 0, value(t, scores, "corrections_caused"), 1e-9)
		for name, sc := range scores {
			assert.Equal(t, jevKey, sc.Metadata["method_key"], name)
			assert.Equal(t, s.set[jevKey].Ref().Version, sc.Metadata["method_version"], name)
		}
	})

	t.Run("a kind with no labeled decision", func(t *testing.T) {
		// spec: KNW-088.absent-metric-not-sent
		s := judgedThread(t, map[string]map[judgment.FlagKind]float64{promise: {judgment.FlagKindCommitment: 0.9, judgment.FlagKindBigMoment: 0.9}}, said{3, promise})
		s.label(t, 0, judgment.FlagKindCommitment, true)
		s.scorecard(t)
		l := newFakeLangfuse(t)

		s.e.langfuseExport(t, l)

		scores := l.runScores(t, s.e.lastRun(t, s.set[jevKey].Ref()).ID)
		require.Contains(t, scores, "flag.commitment.precision")
		for name := range scores {
			assert.False(t, strings.HasPrefix(name, "flag.big_moment."), "big moment's shown flag holds no label: %s", name)
		}
		assert.InDelta(t, 1, value(t, scores, "messages.flagged.big_moment"), 1e-9)
	})

	t.Run("a second export after more labels", func(t *testing.T) {
		// spec: KNW-088.later-export-replaces
		s := judgedThread(t, map[string]map[judgment.FlagKind]float64{promise: {judgment.FlagKindCommitment: 0.9}}, said{3, promise}, said{3, filler(0)})
		s.label(t, 0, judgment.FlagKindCommitment, true)
		s.scorecard(t)
		rerun := s.e.lastRun(t, s.set[jevKey].Ref()).ID
		l := newFakeLangfuse(t)
		s.e.langfuseExport(t, l)
		first := l.runScores(t, rerun)
		require.InDelta(t, 1, value(t, first, "flag.commitment.precision"), 1e-9)
		s.label(t, 0, judgment.FlagKindCommitment, false)
		s.label(t, 1, judgment.FlagKindCommitment, true)

		s.e.langfuseExport(t, l)

		second := l.runScores(t, rerun)
		assert.InDelta(t, 0, value(t, second, "flag.commitment.precision"), 1e-9)
		assert.InDelta(t, 0, value(t, second, "flag.commitment.recall"), 1e-9)
		assert.InDelta(t, 1, value(t, second, "flag.commitment.unshown_held"), 1e-9)
		assert.Equal(t, first["flag.commitment.precision"].ID, second["flag.commitment.precision"].ID, "the later value replaces the earlier")
		assert.Len(t, second, len(first), "each metric is one score")
	})

	t.Run("scores holding no message text, sender or contact identity", func(t *testing.T) {
		// spec: KNW-088.scores-content-free
		s := clearedThread(t, 0.9)
		s.label(t, 0, judgment.FlagKindCommitment, true)
		s.labelClearing(t, jan(5, 0), true)
		s.scorecard(t)
		l := newFakeLangfuse(t)

		s.e.langfuseExport(t, l)

		require.NotEmpty(t, l.runScores(t, s.e.lastRun(t, s.set[jevKey].Ref()).ID))
		assertContentFree(t, l.sent(), s.thr)
	})

	// Coordinator ruling PR3-R1: a stored stage-1 rerun's before_window record
	// is read where the export reads that rerun through the scorecard.
	t.Run("a stage-1 record unjudged before_window", func(t *testing.T) {
		// spec: KNW-087.stage-1-before-window-read, KNW-087.unjudged-never-a-no
		e := newFlagsEnv(t, "")
		th := e.newThread(t)
		th.say(t, jan(3, 9), false, filler(0))
		th.say(t, jan(3, 10), true, promise)
		keys := th.keys(t)
		stage1 := judgment.MethodRef{Key: jevKey, Version: "stage-1", WordingVersion: "stage-1"}
		rerun, err := e.store.StartRun(e.ctx, judgment.RunSpec{Kind: judgment.RunKindRerun, Method: stage1})
		require.NoError(t, err)
		_, err = e.store.RecordJudgment(e.ctx, rerun, judgment.Judgment{
			Method: stage1, Thread: th.venue, Kind: threadctx.KindConversation, CutOff: jan(4, 0), JudgedAt: jan(4, 0),
			InputHash: th.context(t, jan(4, 0)).InputHash, WindowStart: keys[1], NewMessages: keys[1:],
			Flags: []judgment.FlagDecision{{MessageKey: keys[1], Kind: judgment.FlagKindCommitment, Probability: 0.9}},
		})
		require.NoError(t, err)
		require.NoError(t, e.store.RecordUnjudged(e.ctx, rerun, []judgment.UnjudgedMessage{
			{Thread: th.venue, CutOff: jan(4, 0), MessageKey: keys[0], Reason: judgment.UnjudgedReasonBeforeWindow},
		}))
		require.NoError(t, e.store.FinishRun(e.ctx, rerun))
		threads := judgment.NewThreads(e.store, methods{})
		for _, key := range keys {
			_, err := threads.LabelFlag(e.ctx, rerun, th.venue, key, judgment.FlagKindCommitment, true)
			require.NoError(t, err)
		}
		l := newFakeLangfuse(t)

		e.langfuseExport(t, l)

		scores := l.runScores(t, rerun)
		assert.InDelta(t, 1, value(t, scores, "messages.unjudged.before_window"), 1e-9)
		assert.InDelta(t, 1, value(t, scores, "messages.judged"), 1e-9)
		assert.InDelta(t, 1, value(t, scores, "flag.commitment.recall"), 1e-9, "the unjudged message is outside recall's denominator")
		assert.InDelta(t, 0, value(t, scores, "flag.commitment.unshown_held"), 1e-9, "and never counted as answered no")
	})
}
