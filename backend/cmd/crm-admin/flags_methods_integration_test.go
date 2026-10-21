//go:build integration_testdb

package main

import (
	"bytes"
	"testing"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/decision"
	"personal-crm/backend/internal/judgerun"
	"personal-crm/backend/internal/judgment"
	"personal-crm/backend/internal/scorecard"
	"personal-crm/backend/internal/threadctx"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// removedMethods are the method keys runs and the scorecard no longer accept.
var removedMethods = []string{"sage-masked", "clef-flash-masked"}

// registryEnv is the real method registry with every route configured, its
// endpoints reaching fake vendors.
type registryEnv struct {
	registry *decision.Registry
	vendors  []*vendor
}

func (e *flagsEnv) newRegistryEnv(t *testing.T) registryEnv {
	t.Helper()
	openRouter, venice := newVendor(t), newVendor(t)
	routeVendors(t, map[string]*vendor{"openrouter.ai": openRouter, "api.venice.ai": venice})
	return registryEnv{
		registry: decision.NewRegistry(config.DecisionConfig{OpenRouterAPIKey: "key", VeniceAPIKey: "key"}, e.masker),
		vendors:  []*vendor{openRouter, venice},
	}
}

// sent counts the calls every vendor received.
func (r registryEnv) sent() int {
	n := 0
	for _, v := range r.vendors {
		n += len(v.all())
	}
	return n
}

// tryScorecard runs crm-admin --scorecard for the method, compared with the
// compare method when one is named.
func (e *flagsEnv) tryScorecard(set judgerun.Methods, key, compare string) error {
	runner, err := judgerun.New(e.contexts, e.store, set)
	if err != nil {
		return err
	}
	return run(e.ctx, runOptions{scorecard: true, judgeMethod: key, compareMethod: compare}, adminDeps{
		scorecard: scorecard.New(e.store, runner, set, e.contacts, e.cfg.Watchdog),
		stdout:    &bytes.Buffer{}, stderr: &bytes.Buffer{},
	})
}

// Runs and the scorecard accept only the compared methods, clef-masked,
// clef-masked-reasoned and jev-masked: naming sage-masked or clef-flash-masked
// is refused before any call, and their stored judgments and runs still read.
// spec: KNW-074.compared-methods, KNW-074.removed-refused-before-any-call, KNW-074.designation-refused-as-unknown, KNW-074.stored-judgments-still-read
func TestRunsAndTheScorecardAcceptOnlyTheComparedMethods(t *testing.T) {
	t.Run("a historical run naming sage-masked", func(t *testing.T) {
		e := newFlagsEnv(t, "")
		e.chat(t, 3)
		r := e.newRegistryEnv(t)

		_, err := e.tryJudgeMonth(januaryMonth, r.registry, "sage-masked", false)

		require.ErrorContains(t, err, `unknown decision method "sage-masked"`)
		assert.Zero(t, r.sent(), "nothing is sent")
		runs, err := e.store.ListRuns(e.ctx)
		require.NoError(t, err)
		assert.Empty(t, runs, "no run is started")
	})

	for _, tc := range []struct{ name, method, compare string }{
		{"--scorecard naming clef-flash-masked as its method", "clef-flash-masked", ""},
		{"--scorecard naming clef-flash-masked as its compared method", jevKey, "clef-flash-masked"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newFlagsEnv(t, "")
			reviewedThread(t, e, methods{jevKey: e.method(jevKey, vendorFlagging(t, 0.9), roomy)}, jevKey)
			r := e.newRegistryEnv(t)

			err := e.tryScorecard(r.registry, tc.method, tc.compare)

			require.ErrorContains(t, err, "unknown decision method method=clef-flash-masked")
			assert.Zero(t, r.sent(), "nothing is sent")
		})
	}

	t.Run("a production designation naming a removed method, refused as an unknown method is", func(t *testing.T) {
		for _, key := range append([]string{"no-such-method"}, removedMethods...) {
			t.Run(key, func(t *testing.T) {
				e := newFlagsEnv(t, key)
				e.chat(t, 3)
				r := e.newRegistryEnv(t)

				_, err := e.tryJudgeMonth(januaryMonth, r.registry, jevKey, false)

				require.EqualError(t, err, `DECISION_PRODUCTION_METHOD names unknown decision method "`+key+`"`)
				assert.Zero(t, r.sent(), "nothing is sent")
			})
		}
	})

	t.Run("a stored clef-flash-masked judgment with flag decisions, returned by the store's reads", func(t *testing.T) {
		e := newFlagsEnv(t, "")
		th := e.chat(t, 2)
		keys := th.keys(t)
		removed := judgment.MethodRef{Key: "clef-flash-masked", Version: "stored", WordingVersion: "stored"}
		from, to := jan(1, 0), jan(1, 0).AddDate(0, 1, 0)
		stored, err := e.store.StartRun(e.ctx, judgment.RunSpec{Kind: judgment.RunKindHistorical, Method: removed, From: &from, To: &to})
		require.NoError(t, err)
		flags := []judgment.FlagDecision{{MessageKey: keys[0], Kind: judgment.FlagKindCommitment, Probability: 0.8}, {MessageKey: keys[1], Kind: judgment.FlagKindClaim, Probability: 0.4}}
		_, err = e.store.RecordJudgment(e.ctx, stored, judgment.Judgment{
			Method: removed, Thread: th.venue, Kind: threadctx.KindConversation, CutOff: jan(4, 0), JudgedAt: jan(4, 0),
			InputHash: th.context(t, jan(4, 0)).InputHash, WindowStart: keys[0], NewMessages: keys, Flags: flags,
		})
		require.NoError(t, err)
		require.NoError(t, e.store.FinishRun(e.ctx, stored))
		e.newRegistryEnv(t)

		j := th.judgmentAt(t, removed, jan(4, 0))
		assert.Equal(t, keys, j.NewMessages)
		assert.Equal(t, flags, j.Flags)
		assert.Equal(t, stored, e.lastRun(t, removed).ID, "the run still reads")
	})
}
