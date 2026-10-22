//go:build integration_testdb

package main

import (
	"bytes"
	"net/http"
	"os"
	"testing"
	"time"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/decision"
	"personal-crm/backend/internal/judgment"
	"personal-crm/backend/internal/namemodel"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/threadctx"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sayCopy stores the contact's copy of a received message of the thread, with
// the external ID given, and returns the stored row's ID.
func (th *thr) sayCopy(t *testing.T, at time.Time, external string, contact uuid.UUID, text string) uuid.UUID {
	t.Helper()
	container, peer := th.cont, "peer-"+contact.String()+"@example.test"
	m, err := repository.NewCommsMessageRepository(th.e.database.Queries).UpsertMessage(th.e.ctx, repository.UpsertCommsMessageParams{
		Source: repository.InteractionSourceEmail, ExternalID: external, ThreadID: &container,
		Body: &text, PeerHandle: &peer, PeerNormalized: &peer, Direction: repository.InteractionDirectionInbound, SentAt: at,
		SourceMetadata: []byte("{}"), MatchedContactID: contact,
	})
	require.NoError(t, err)
	return m.ID
}

// Before masking, the masker ensures every entry has name spans for its
// message, its text and the model version, asking the name model only for the
// texts the span store lacks and storing what it answers.
// spec: KNW-090.spans-ensured-and-stored
func TestHistoricalRunAsksTheNameModelOnlyForWhatItLacks(t *testing.T) {
	t.Parallel()

	t.Run("a run whose entries all have stored spans asks the model nothing", func(t *testing.T) {
		t.Parallel()
		e := newFlagsEnv(t, "")
		e.daily(t)
		jev, clef := e.method(jevKey, newVendor(t), roomy), e.method(clefKey, newVendor(t), roomy)
		_, err := e.tryJudgeDays("2026-01-01", "2026-01-07", methods{jevKey: jev}, jevKey, false)
		require.NoError(t, err)
		e.names.reset()

		out, err := e.tryJudgeDays("2026-01-01", "2026-01-07", methods{clefKey: clef}, clefKey, false)

		require.NoError(t, err)
		assert.Contains(t, out, "judged=7")
		assert.Empty(t, e.names.requests())
	})

	t.Run("a run with some entries stored asks only for the others", func(t *testing.T) {
		t.Parallel()
		e := newFlagsEnv(t, "")
		e.daily(t)
		m := e.method(jevKey, newVendor(t), roomy)
		_, err := e.tryJudgeDays("2026-01-01", "2026-01-03", methods{jevKey: m}, jevKey, false)
		require.NoError(t, err)
		e.names.reset()

		_, err = e.tryJudgeDays("2026-01-01", "2026-01-07", methods{jevKey: m}, jevKey, false)

		require.NoError(t, err)
		assert.Equal(t, []string{filler(4), filler(5), filler(6), filler(7)}, e.names.texts())
	})

	t.Run("a message whose text changed is asked again", func(t *testing.T) {
		t.Parallel()
		e := newFlagsEnv(t, "")
		th := e.newThread(t)
		th.say(t, jan(1, 9), false, filler(1))
		changed := th.sayCopy(t, jan(2, 9), uuid.NewString(), th.contact, filler(2))
		m := e.method(jevKey, newVendor(t), roomy)
		_, err := e.tryJudgeDays("2026-01-01", "2026-01-02", methods{jevKey: m}, jevKey, false)
		require.NoError(t, err)
		e.names.reset()
		text := "the changed text"
		_, err = repository.NewCommsMessageRepository(e.database.Queries).UpdateBody(e.ctx, changed, &text)
		require.NoError(t, err)

		_, err = e.tryJudgeDays("2026-01-01", "2026-01-02", methods{jevKey: m}, jevKey, false)

		require.NoError(t, err)
		assert.Equal(t, []string{"the changed text"}, e.names.texts())
	})

	t.Run("a change of model version asks again", func(t *testing.T) {
		t.Parallel()
		e := newFlagsEnv(t, "")
		e.daily(t)
		v := newVendor(t)
		at05 := e.method(jevKey, v, roomy)
		_, err := e.tryJudgeDays("2026-01-01", "2026-01-03", methods{jevKey: at05}, jevKey, false)
		require.NoError(t, err)
		e.names.reset()
		at06 := e.methodAt(jevKey, threadctx.NewModelVersion(0.6), v)

		_, err = e.tryJudgeDays("2026-01-01", "2026-01-03", methods{jevKey: at06}, jevKey, false)

		require.NoError(t, err)
		assert.ElementsMatch(t, []string{filler(0), filler(1), filler(2), filler(3)}, e.names.texts())
		for _, r := range e.names.requests() {
			assert.InDelta(t, 0.6, r.Threshold, 0)
		}
	})

	t.Run("two per-contact copies of one message with the same text are one stored entry and one text asked", func(t *testing.T) {
		t.Parallel()
		e := newFlagsEnv(t, "")
		th := e.newThread(t)
		other, err := e.harness.SeedContact(e.ctx, e.harness.Generator().Contact())
		require.NoError(t, err)
		external := uuid.NewString()
		th.sayCopy(t, jan(1, 9), external, th.contact, "one message, two copies")
		th.sayCopy(t, jan(1, 9), external, other.ID, "one message, two copies")
		m := e.method(jevKey, newVendor(t), roomy)

		_, err = e.tryJudgeDays("2026-01-01", "2026-01-02", methods{jevKey: m}, jevKey, false)

		require.NoError(t, err)
		assert.Equal(t, []string{"one message, two copies"}, e.names.texts())
	})

	t.Run("a second run over the same range asks the model nothing", func(t *testing.T) {
		t.Parallel()
		e := newFlagsEnv(t, "")
		e.daily(t)
		m := e.method(jevKey, newVendor(t), roomy)
		_, err := e.tryJudgeDays("2026-01-01", "2026-01-07", methods{jevKey: m}, jevKey, false)
		require.NoError(t, err)
		require.NotEmpty(t, e.names.requests())
		e.names.reset()

		out, err := e.tryJudgeDays("2026-01-01", "2026-01-07", methods{jevKey: m}, jevKey, false)

		require.NoError(t, err)
		assert.Contains(t, out, "reused=7")
		assert.Empty(t, e.names.requests())
	})
}

// assertNotSent asserts that the run over the daily thread's seven days sent
// nothing to the vendor and recorded every cut-off as it records a failed
// call.
func (e *flagsEnv) assertNotSent(t *testing.T, out string, v *vendor, m decision.Method) {
	t.Helper()
	assert.Contains(t, out, "judged=0 reused=0 incomplete=7")
	assert.Contains(t, out, "incomplete reason=call_failed n=7")
	assert.Empty(t, v.all(), "nothing is sent")
	recorded := e.lastRun(t, m.Ref()).Incomplete
	assert.Len(t, recorded, 7)
	for _, r := range recorded {
		assert.Equal(t, judgment.IncompleteReasonCallFailed, r.Reason)
	}
}

// A request with an entry the name model has not covered is not sent to a
// vendor: the run records it as it records a failed call, and a rerun once
// the model answers sends it.
// spec: KNW-090.no-spans-no-send
func TestHistoricalRunSendsNothingTheNameModelHasNotCovered(t *testing.T) {
	t.Parallel()

	failures := []struct {
		name   string
		answer func(w http.ResponseWriter, r *http.Request, req spansRequest)
	}{
		{"a non-success status", func(w http.ResponseWriter, _ *http.Request, _ spansRequest) {
			http.Error(w, "model failed", http.StatusInternalServerError)
		}},
		{"a refusal of the model identity", func(w http.ResponseWriter, _ *http.Request, req spansRequest) {
			if req.Model != "another/model" {
				w.WriteHeader(http.StatusUnprocessableEntity)
				writeTestJSON(w, map[string]string{"error": "model must be another/model"})
				return
			}
			writeSpans(w, req, make([][]threadctx.NameSpan, len(req.Texts)))
		}},
		{"a response whose results do not align with the texts asked", func(w http.ResponseWriter, _ *http.Request, req spansRequest) {
			writeSpans(w, req, make([][]threadctx.NameSpan, len(req.Texts)+1))
		}},
		{"a response for another model version", func(w http.ResponseWriter, _ *http.Request, req spansRequest) {
			req.Threshold = 0.9
			writeSpans(w, req, make([][]threadctx.NameSpan, len(req.Texts)))
		}},
		{"a span outside its text", func(w http.ResponseWriter, _ *http.Request, req spansRequest) {
			spans := make([][]threadctx.NameSpan, len(req.Texts))
			spans[0] = []threadctx.NameSpan{{Start: 0, End: len(req.Texts[0]) + 1}}
			writeSpans(w, req, spans)
		}},
		{"a span off a UTF-8 boundary", func(w http.ResponseWriter, _ *http.Request, req spansRequest) {
			spans := make([][]threadctx.NameSpan, len(req.Texts))
			// Inside the two bytes of the é each daily text begins with.
			spans[0] = []threadctx.NameSpan{{Start: 1, End: 4}}
			writeSpans(w, req, spans)
		}},
		{"a null result for each text", func(w http.ResponseWriter, _ *http.Request, req spansRequest) {
			writeTestJSON(w, map[string]any{"model": req.Model, "label": req.Label, "threshold": req.Threshold, "spans": make([]any, len(req.Texts))})
		}},
		{"a null span", func(w http.ResponseWriter, _ *http.Request, req spansRequest) {
			spans := make([]any, len(req.Texts))
			for i := range spans {
				spans[i] = []any{}
			}
			spans[0] = []any{nil}
			writeTestJSON(w, map[string]any{"model": req.Model, "label": req.Label, "threshold": req.Threshold, "spans": spans})
		}},
		{"a span without its start", func(w http.ResponseWriter, _ *http.Request, req spansRequest) {
			spans := make([]any, len(req.Texts))
			for i := range spans {
				spans[i] = []any{}
			}
			spans[0] = []any{map[string]int{"end": 2}}
			writeTestJSON(w, map[string]any{"model": req.Model, "label": req.Label, "threshold": req.Threshold, "spans": spans})
		}},
	}
	for _, tc := range failures {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newFlagsEnv(t, "")
			e.accented(t)
			e.names.setAnswer(tc.answer)
			v := newVendor(t)
			m := e.method(jevKey, v, roomy)

			out, err := e.tryJudgeDays("2026-01-01", "2026-01-07", methods{jevKey: m}, jevKey, false)

			require.NoError(t, err)
			require.NotEmpty(t, e.names.requests(), "the model is asked")
			e.assertNotSent(t, out, v, m)
		})
	}

	t.Run("no listener on the socket", func(t *testing.T) {
		t.Parallel()
		e := newFlagsEnv(t, "")
		e.daily(t)
		e.names.stop()
		v := newVendor(t)
		m := e.method(jevKey, v, roomy)

		out, err := e.tryJudgeDays("2026-01-01", "2026-01-07", methods{jevKey: m}, jevKey, false)

		require.NoError(t, err)
		e.assertNotSent(t, out, v, m)
	})

	t.Run("a deadline expiry", func(t *testing.T) {
		t.Parallel()
		e := newFlagsEnv(t, "")
		e.daily(t)
		e.names.setAnswer(func(_ http.ResponseWriter, r *http.Request, _ spansRequest) { <-r.Context().Done() })
		v := newVendor(t)
		short := e.maskerOf(t, e.names.socket, namemodel.Deadline{Start: 100 * time.Millisecond})
		m := decision.NewMaskedMethod(fakeDefinition(jevKey, roomy), defaultNames, short, decision.NewSystemOneAdapter(v.url, "key", "fake-1"))

		out, err := e.tryJudgeDays("2026-01-01", "2026-01-07", methods{jevKey: m}, jevKey, false)

		require.NoError(t, err)
		require.NotEmpty(t, e.names.requests(), "the model is asked")
		e.assertNotSent(t, out, v, m)
	})

	t.Run("some entries stored and the model down for the others, then a rerun once the model answers", func(t *testing.T) {
		t.Parallel()
		e := newFlagsEnv(t, "")
		th := e.daily(t)
		v := newVendor(t)
		m := e.method(jevKey, v, roomy)
		_, err := e.tryJudgeDays("2026-01-01", "2026-01-03", methods{jevKey: m}, jevKey, false)
		require.NoError(t, err)
		v.reset()
		e.names.stop()

		out, err := e.tryJudgeDays("2026-01-01", "2026-01-07", methods{jevKey: m}, jevKey, false)

		require.NoError(t, err)
		assert.Contains(t, out, "judged=0 reused=3 incomplete=4")
		assert.Contains(t, out, "incomplete reason=call_failed n=4")
		assert.Empty(t, v.all(), "nothing is sent")
		assert.Equal(t, januaryCutOffs(1, 3), cutOffsOf(t, th, m.Ref()))

		e.names.start(t)
		out, err = e.tryJudgeDays("2026-01-01", "2026-01-07", methods{jevKey: m}, jevKey, false)

		require.NoError(t, err)
		assert.Contains(t, out, "judged=4 reused=3 incomplete=0")
		assert.Len(t, v.all(), 4, "the requests the model now covers are sent")
		assert.Equal(t, januaryCutOffs(1, 7), cutOffsOf(t, th, m.Ref()))
	})
}

// accented is a thread with a message on each of January's first seven days,
// each text beginning with a two-byte character.
func (e *flagsEnv) accented(t *testing.T) *thr {
	t.Helper()
	th := e.newThread(t)
	for day := 1; day <= 7; day++ {
		th.say(t, jan(day, 9), false, "école "+filler(day))
	}
	return th
}

// A historical run with no name model to ask fails before any call: building
// the command's stack refuses when NAME_MODEL_SOCKET is unset, so no run
// starts and no vendor is called.
// spec: KNW-090.no-spans-no-send
func TestHistoricalRunRefusesWithoutTheNameModelSocket(t *testing.T) {
	e := newFlagsEnv(t, "")
	e.chat(t, 2)
	venice := newVendor(t)
	routeVendors(t, map[string]*vendor{"api.venice.ai": venice})
	// config.Load requires a database URL; the stack runs on the env's
	// own database.
	t.Setenv("DATABASE_URL", "postgres://unused-never-connected-to")
	t.Setenv("NODE_ENV", "development")
	t.Setenv("DECISION_PRODUCTION_METHOD", "")
	t.Setenv("VENICE_API_KEY", "key")
	t.Setenv("OWN_NAMES", "Marlowe Quill")
	t.Setenv("NAME_MODEL_SOCKET", "")
	cfg, err := config.Load()
	require.NoError(t, err)

	_, _, err = buildProductionDeps(e.ctx, cfg, e.database, runOptions{judgeStartDay: "2026-01-01", judgeEndDay: "2026-01-07", judgeMethod: jevKey})

	require.ErrorContains(t, err, "NAME_MODEL_SOCKET")
	assert.Empty(t, venice.all(), "nothing is sent")
	runs, err := e.store.ListRuns(e.ctx)
	require.NoError(t, err)
	assert.Empty(t, runs, "no run is started")
}

// thresholdRun is the command's historical run over the env's database with
// NAME_MODEL_THRESHOLD as given, unset when nil.
type thresholdRun struct {
	e      *flagsEnv
	venice *vendor
}

func newThresholdRun(t *testing.T) *thresholdRun {
	t.Helper()
	r := &thresholdRun{e: newFlagsEnv(t, ""), venice: newVendor(t)}
	routeVendors(t, map[string]*vendor{"api.venice.ai": r.venice})
	// config.Load requires a database URL; the stack runs on the env's own
	// database.
	t.Setenv("DATABASE_URL", "postgres://unused-never-connected-to")
	t.Setenv("NODE_ENV", "development")
	t.Setenv("DECISION_PRODUCTION_METHOD", "")
	t.Setenv("VENICE_API_KEY", "key")
	t.Setenv("OPENROUTER_API_KEY", "key")
	t.Setenv("OWN_NAMES", "Marlowe Quill")
	t.Setenv("NAME_MODEL_SOCKET", r.e.names.socket)
	return r
}

// setThreshold sets NAME_MODEL_THRESHOLD, or unsets it for "".
func setThreshold(t *testing.T, value string) {
	t.Helper()
	t.Setenv("NAME_MODEL_THRESHOLD", value)
	if value == "" {
		require.NoError(t, os.Unsetenv("NAME_MODEL_THRESHOLD"))
	}
}

// judge runs the command's historical run of jev-masked over January's first
// three days at the threshold, and returns every masked method's version as
// the API's registry derives it from the same configuration.
func (r *thresholdRun) judge(t *testing.T, threshold string) map[string]string {
	t.Helper()
	setThreshold(t, threshold)
	cfg, err := config.Load()
	require.NoError(t, err)
	opts := runOptions{judgeStartDay: "2026-01-01", judgeEndDay: "2026-01-03", judgeMethod: jevKey}
	deps, cleanup, err := buildProductionDeps(r.e.ctx, cfg, r.e.database, opts)
	require.NoError(t, err)
	t.Cleanup(cleanup)
	deps.stdout, deps.stderr = &bytes.Buffer{}, &bytes.Buffer{}
	require.NoError(t, run(r.e.ctx, opts, deps))
	registry := decision.NewRegistry(cfg.Decision, nil)
	versions := map[string]string{}
	for _, key := range registry.Keys() {
		m, err := registry.Get(key)
		require.NoError(t, err)
		versions[key] = m.Ref().Version
	}
	return versions
}

// The score cutoff a run asks the name model for is NAME_MODEL_THRESHOLD,
// 0.5 when unset, read by the configuration the API and crm-admin both load;
// a change to it changes every masked method's version and the stored spans'
// key, so a rerun judges again and asks the model again. A value that is not
// a number or lies outside (0, 1] is refused before any call.
// spec: KNW-090.threshold-setting
func TestHistoricalRunAsksTheNameModelAtTheThresholdSetting(t *testing.T) {
	t.Run("unset, the run asks the model at 0.5", func(t *testing.T) {
		r := newThresholdRun(t)
		r.e.daily(t)

		r.judge(t, "")

		require.NotEmpty(t, r.e.names.requests())
		for _, req := range r.e.names.requests() {
			assert.InDelta(t, 0.5, req.Threshold, 0)
		}
		assert.NotEmpty(t, r.venice.all(), "the run sends its masked contexts")
	})

	t.Run("0.6 changes every masked method's version and asks the model again at 0.6", func(t *testing.T) {
		r := newThresholdRun(t)
		th := r.e.daily(t)
		at05 := r.judge(t, "0.5")
		r.e.names.reset()
		r.venice.reset()

		at06 := r.judge(t, "0.6")

		require.Len(t, at06, len(at05))
		for key, version := range at05 {
			assert.NotEqual(t, version, at06[key], "%s's version", key)
		}
		require.NotEmpty(t, r.e.names.requests(), "spans stored at 0.5 are not reused")
		assert.ElementsMatch(t, []string{filler(0), filler(1), filler(2), filler(3)}, r.e.names.texts())
		for _, req := range r.e.names.requests() {
			assert.InDelta(t, 0.6, req.Threshold, 0)
		}
		assert.Len(t, r.venice.all(), 3, "the rerun judges again")
		views, err := r.e.store.ListJudgments(r.e.ctx, judgment.JudgmentQuery{Thread: &th.venue})
		require.NoError(t, err)
		judgedAt06 := 0
		for _, v := range views {
			if v.Method.Version == at06[jevKey] {
				judgedAt06++
			}
		}
		assert.Equal(t, 3, judgedAt06, "the rerun's judgments are of the version at 0.6")
	})

	t.Run(`"0.50" names the version and the stored spans "0.5" does`, func(t *testing.T) {
		r := newThresholdRun(t)
		r.e.daily(t)
		at05 := r.judge(t, "0.5")
		r.e.names.reset()
		r.venice.reset()

		at050 := r.judge(t, "0.50")

		assert.Equal(t, at05, at050)
		assert.Empty(t, r.e.names.requests(), "the spans stored at 0.5 are read")
		assert.Empty(t, r.venice.all(), "the judgments made at 0.5 are reused")
	})

	for _, value := range []string{"half", "0", "-0.5", "1.5", "NaN"} {
		t.Run("a value of "+value+" is refused before any call", func(t *testing.T) {
			r := newThresholdRun(t)
			r.e.daily(t)
			setThreshold(t, value)

			err := runMain([]string{"--judge-start-day", "2026-01-01", "--judge-end-day", "2026-01-03", "--method", jevKey})

			require.ErrorContains(t, err, "NAME_MODEL_THRESHOLD")
			assert.Empty(t, r.e.names.requests(), "the model is asked nothing")
			assert.Empty(t, r.venice.all(), "nothing is sent")
		})
	}
}
