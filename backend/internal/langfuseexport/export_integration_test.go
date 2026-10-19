//go:build integration_testdb

package langfuseexport_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/judgment"
	"personal-crm/backend/internal/langfuseexport"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/synthetic"
	"personal-crm/backend/internal/synthetic/factory"
	"personal-crm/backend/internal/threadctx"
	"personal-crm/backend/tests/testsupport"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var method = judgment.MethodRef{Key: "export-method", Version: "v1", WordingVersion: "w1"}

// unusedAsserter, unusedCadence and unusedJobs stand in for the store's live
// effects, which a historical run with no production method never reaches.
type unusedAsserter struct{ judgment.Asserter }

type unusedCadence struct{}

func (unusedCadence) EndAwaitingReplyTx(context.Context, pgx.Tx, uuid.UUID) error { return nil }

type unusedJobs struct{}

func (unusedJobs) InsertTx(context.Context, pgx.Tx, river.JobArgs, *river.InsertOpts) (*rivertype.JobInsertResult, error) {
	return nil, errors.New("no job is inserted")
}

// langfuse is Langfuse at its HTTP API: it holds no traces and records every
// request body it receives.
type langfuse struct {
	server *httptest.Server
	mu     sync.Mutex
	bodies []string
	spans  map[string]map[string]any // trace name -> trace metadata
	judged map[string]map[string]any // judgment cut-off -> trace metadata
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
		switch r.URL.Path {
		case "/api/public/v2/observations":
			_, _ = w.Write([]byte(`{"data":[],"meta":{}}`))
		case "/api/public/otel/v1/traces":
			l.keepSpans(t, raw)
			_, _ = w.Write([]byte(`{}`))
		default:
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
				if cutOff, ok := metadata["cut_off"].(string); ok {
					l.judged[cutOff] = metadata
				}
			}
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
		Assert:   unusedAsserter{},
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
	exporter := langfuseexport.New(config.LangfuseExtractionConfig{Host: e.langfuse.server.URL, PublicKey: "pk", SecretKey: "sk"}, e.store, nil)
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
