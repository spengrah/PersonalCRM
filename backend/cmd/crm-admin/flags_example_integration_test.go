//go:build integration_testdb

package main

import (
	"testing"
	"time"

	"personal-crm/backend/internal/decision"
	"personal-crm/backend/internal/judgment"
	"personal-crm/backend/internal/threadctx"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// exampleAt returns the example the method's latest run recorded its judgment
// at the cut-off under, read through the store's export.
func (e *flagsEnv) exampleAt(t *testing.T, m judgment.MethodRef, cutOff time.Time) judgment.ExampleID {
	t.Helper()
	records, err := e.store.JudgmentRecords(e.ctx, e.lastRun(t, m).ID)
	require.NoError(t, err)
	var found []judgment.ExampleID
	for _, r := range records {
		if r.CutOff.Equal(cutOff) {
			found = append(found, r.Example)
		}
	}
	require.Len(t, found, 1, "records of %s at %s", m.Key, cutOff)
	return found[0]
}

// legacyExample records a judgment of the thread's context at the cut-off by a
// method version before the change, which read the whole context and holds no
// window, and returns the example the store issued for it.
func (e *flagsEnv) legacyExample(t *testing.T, th *thr, cutOff time.Time) judgment.ExampleID {
	t.Helper()
	legacy := judgment.MethodRef{Key: jevKey, Version: "before-windowing", WordingVersion: "before-windowing"}
	from, to := cutOff.AddDate(0, -1, 0), cutOff
	run, err := e.store.StartRun(e.ctx, judgment.RunSpec{Kind: judgment.RunKindHistorical, Method: legacy, From: &from, To: &to})
	require.NoError(t, err)
	c := th.context(t, cutOff)
	_, err = e.store.RecordJudgment(e.ctx, run, judgment.Judgment{
		Method: legacy, Thread: th.venue, Kind: threadctx.KindConversation, CutOff: cutOff, JudgedAt: cutOff, InputHash: c.InputHash,
	})
	require.NoError(t, err)
	records, err := e.store.JudgmentRecords(e.ctx, run)
	require.NoError(t, err)
	require.Len(t, records, 1)
	return records[0].Example
}

// Every method's judgment of a thread context at a cut-off records the same
// example, its thread, cut-off and the full context's input hash, whatever
// window the method read.
// spec: KNW-065.one-example-per-context, KNW-065.hash-of-full-context-whatever-window
func TestJudgeMonthRecordsOneExamplePerThreadContext(t *testing.T) {
	setup := func(t *testing.T) (*flagsEnv, *thr, []string, string) {
		e := newFlagsEnv(t, "")
		th := e.chat(t, 6)
		return e, th, th.keys(t), th.context(t, jan(4, 0)).InputHash
	}
	t.Run("one method windows the context while another reads it whole", func(t *testing.T) {
		e, th, keys, hash := setup(t)
		limit := e.tokensFor(t, fakeDefinition(jevKey, roomy), decision.Request{Context: th.context(t, jan(4, 0))}, keys[3])
		windowing := e.method(jevKey, newVendor(t, false), decision.Limits{Tokens: limit, Questions: 1 << 20})
		whole := e.method(clefKey, newVendor(t, false), roomy)

		e.judgeMonth(t, methods{jevKey: windowing}, jevKey)
		e.judgeMonth(t, methods{clefKey: whole}, clefKey)

		a, b := th.judgmentAt(t, windowing.Ref(), jan(4, 0)), th.judgmentAt(t, whole.Ref(), jan(4, 0))
		assert.Equal(t, keys[3], a.WindowStart)
		assert.Equal(t, keys[0], b.WindowStart)
		assert.Equal(t, hash, a.InputHash)
		assert.Equal(t, hash, b.InputHash)
		assert.Equal(t, e.exampleAt(t, windowing.Ref(), jan(4, 0)), e.exampleAt(t, whole.Ref(), jan(4, 0)), "one example for both windows")
	})

	t.Run("two methods whose windows start at different messages", func(t *testing.T) {
		e, th, keys, hash := setup(t)
		req := decision.Request{Context: th.context(t, jan(4, 0))}
		jev := e.method(jevKey, newVendor(t, false), decision.Limits{Tokens: e.tokensFor(t, fakeDefinition(jevKey, roomy), req, keys[2]), Questions: 1 << 20})
		clef := e.method(clefKey, newVendor(t, false), decision.Limits{Tokens: e.tokensFor(t, fakeDefinition(clefKey, roomy), req, keys[4]), Questions: 1 << 20})

		e.judgeMonth(t, methods{jevKey: jev}, jevKey)
		e.judgeMonth(t, methods{clefKey: clef}, clefKey)

		a, b := th.judgmentAt(t, jev.Ref(), jan(4, 0)), th.judgmentAt(t, clef.Ref(), jan(4, 0))
		assert.Equal(t, keys[2], a.WindowStart)
		assert.Equal(t, keys[4], b.WindowStart)
		assert.Equal(t, hash, a.InputHash)
		assert.Equal(t, hash, b.InputHash)
		assert.Equal(t, e.exampleAt(t, jev.Ref(), jan(4, 0)), e.exampleAt(t, clef.Ref(), jan(4, 0)), "one example for both windows")
	})

	t.Run("a context that fits every method whole, whose example is the one the same context had before the change", func(t *testing.T) {
		e, th, _, hash := setup(t)
		before := e.legacyExample(t, th, jan(4, 0))
		jev, clef := e.method(jevKey, newVendor(t, false), roomy), e.method(clefKey, newVendor(t, false), roomy)

		e.judgeMonth(t, methods{jevKey: jev}, jevKey)
		e.judgeMonth(t, methods{clefKey: clef}, clefKey)

		for _, m := range []decision.Method{jev, clef} {
			j := th.judgmentAt(t, m.Ref(), jan(4, 0))
			assert.Equal(t, hash, j.InputHash, "the hash the thread-context builder gives the whole context")
			assert.Equal(t, th.venue, j.Thread)
			assert.Equal(t, before, e.exampleAt(t, m.Ref(), jan(4, 0)), "the example the legacy judgment of the context was issued")
		}
	})
}
