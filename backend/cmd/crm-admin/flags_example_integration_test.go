//go:build integration_testdb

package main

import (
	"testing"

	"personal-crm/backend/internal/decision"

	"github.com/stretchr/testify/assert"
)

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
	})

	t.Run("a context that fits every method whole, whose example is the one the same context had before the change", func(t *testing.T) {
		e, th, _, hash := setup(t)
		jev, clef := e.method(jevKey, newVendor(t, false), roomy), e.method(clefKey, newVendor(t, false), roomy)

		e.judgeMonth(t, methods{jevKey: jev}, jevKey)
		e.judgeMonth(t, methods{clefKey: clef}, clefKey)

		for _, m := range []decision.Method{jev, clef} {
			j := th.judgmentAt(t, m.Ref(), jan(4, 0))
			assert.Equal(t, hash, j.InputHash, "the hash the thread-context builder gives the whole context")
			assert.Equal(t, th.venue, j.Thread)
		}
	})
}
