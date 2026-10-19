//go:build integration_testdb

package main

import (
	"strings"
	"testing"

	"personal-crm/backend/internal/decision"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// assertContentFree fails when the output names any of the threads' message
// texts, message keys or contacts.
func assertContentFree(t *testing.T, out string, threads ...*thr) {
	t.Helper()
	assert.NotEmpty(t, out)
	for _, th := range threads {
		secrets := []string{th.name, th.contact.String(), "@example.test", th.cont}
		secrets = append(secrets, th.keys(t)...)
		for _, e := range th.context(t, jan(31, 0).AddDate(0, 1, 0)).Entries {
			secrets = append(secrets, strings.TrimSpace(e.Text))
		}
		for _, secret := range secrets {
			assert.NotContains(t, out, secret)
		}
	}
	for _, word := range []string{"zanzibar", "garden", "weather"} {
		assert.NotContains(t, out, word)
	}
}

// A run's output holds only IDs, counts, probabilities and reasons, with no
// message text, sender or contact identity, and reports its unjudged messages
// counted by reason beside its judged, reused and incomplete counts.
// spec: KNW-054.content-free-never-blocking, KNW-054.summary-counts-unjudged, KNW-054.measuring-run-sends-nothing, KNW-052.summary-counts-both
func TestJudgeMonthReportsContentFree(t *testing.T) {
	t.Run("a run whose flag decisions name messages holding text, with unjudged messages of each reason", func(t *testing.T) {
		e := newFlagsEnv(t, "")
		// A thread whose call fails, one whose newest message alone is over the
		// limit, and one whose later cut-off has more new messages than its
		// window holds.
		failing := e.newThread(t)
		failing.say(t, jan(3, 9), false, "zanzibar failme in the garden")
		huge := e.newThread(t)
		huge.say(t, jan(3, 9), false, "zanzibar "+strings.Repeat("and the weather ", 400))
		batch := e.newThread(t)
		batch.sayFiller(t, jan(3, 9), false, 0)
		for i := 1; i <= 5; i++ {
			batch.sayFiller(t, jan(4, 8+i), i%2 == 0, i)
		}
		keys := batch.keys(t)
		previous := jan(4, 0)
		limit := e.tokensFor(t, fakeDefinition(jevKey, roomy), decision.Request{Context: batch.context(t, jan(5, 0)), NewAfter: &previous}, keys[3])
		v := newVendor(t, false)
		v.fail = func(c call) bool { return len(c.Texts) > 0 && strings.Contains(c.Texts[0], "failme") }
		m := e.method(jevKey, v, decision.Limits{Tokens: limit, Questions: 1 << 20})
		set := methods{jevKey: m}

		out := e.judgeMonth(t, set, jevKey)

		assert.Contains(t, out, "judged=2 reused=0 incomplete=2")
		assert.Contains(t, out, "incomplete reason=call_failed n=1")
		assert.Contains(t, out, "incomplete reason=message_over_limit n=1")
		assert.Contains(t, out, "unjudged=4")
		assert.Contains(t, out, "unjudged reason=before_window n=2")
		assert.Contains(t, out, "unjudged reason=call_failed n=1")
		assert.Contains(t, out, "unjudged reason=message_over_limit n=1")
		assertContentFree(t, out, failing, huge, batch)
	})

	t.Run("a measuring run, which sends and writes nothing", func(t *testing.T) {
		e := newFlagsEnv(t, "")
		th := e.chat(t, 6)
		huge := e.newThread(t)
		huge.say(t, jan(3, 9), false, "zanzibar "+strings.Repeat("and the weather ", 400))
		v := newVendor(t, false)
		limit := e.tokensFor(t, fakeDefinition(jevKey, roomy), decision.Request{Context: th.context(t, jan(4, 0))}, th.keys(t)[4])
		m := e.method(jevKey, v, decision.Limits{Tokens: limit, Questions: 1 << 20})

		out, err := e.tryJudgeMonth(januaryMonth, methods{jevKey: m}, jevKey, true)

		require.NoError(t, err)
		assert.Empty(t, v.all(), "nothing is sent")
		runs, err := e.store.ListRuns(e.ctx)
		require.NoError(t, err)
		assert.Empty(t, runs, "no run is written")
		assert.Empty(t, th.judgments(t, m.Ref()), "no judgment is written")
		assert.Contains(t, out, "nothing sent, nothing written")
		assert.Contains(t, out, "contexts=2")
		assert.Contains(t, out, "windowed=1 message_over_limit=1")
		assertContentFree(t, out, th, huge)
	})
}
