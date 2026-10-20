//go:build integration_testdb

package main

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"personal-crm/backend/internal/decision"
	"personal-crm/backend/internal/judgment"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// labelsSeen is what a run over a month shows of the two compared methods:
// every request each vendor received, and every judgment each method recorded,
// with message keys and judgment IDs replaced by their positions so that two
// envs compare.
type labelsSeen struct {
	requests  []string
	judgments []string
	// februaryClearings is how many clearing decisions the second month's
	// judgments hold, with Done as the method answered.
	februaryClearings []bool
}

// labelCase is one thread over two months, judged by both compared methods,
// and the label the user states between the months.
type labelCase struct {
	name     string
	flagP    float64
	messages func(t *testing.T, th *thr)
	label    func(t *testing.T, e *flagsEnv, th *thr, m decision.Method)
}

// observe judges January, states the case's label when labeled, judges
// February, and returns what both months showed.
func (c labelCase) observe(t *testing.T, labeled bool) labelsSeen {
	t.Helper()
	e := newFlagsEnv(t, "")
	th := e.newThread(t)
	c.messages(t, th)
	vendors := map[string]*vendor{clefKey: vendorFlagging(t, c.flagP), jevKey: vendorFlagging(t, c.flagP)}
	clef, jev := e.method(clefKey, vendors[clefKey], roomy), e.method(jevKey, vendors[jevKey], roomy)
	set := methods{clefKey: clef, jevKey: jev}

	for key := range set {
		e.judgeMonth(t, set, key)
	}
	if labeled {
		c.label(t, e, th, jev)
	}
	for key := range set {
		e.judgeMonthOf(t, "2026-02", set, key)
	}

	keys := th.keys(t)
	var seen labelsSeen
	for _, key := range []string{clefKey, jevKey} {
		for _, call := range vendors[key].all() {
			var asks []string
			for _, a := range call.Asks {
				asks = append(asks, fmt.Sprintf("%s(%s)", a.ID, a.Text))
			}
			seen.requests = append(seen.requests, fmt.Sprintf("%s texts=%q asks=%v", key, call.Texts, asks))
		}
		views := th.judgments(t, set[key].Ref())
		for i, v := range views {
			var flags, clearings []string
			for _, f := range v.Flags {
				flags = append(flags, fmt.Sprintf("%d/%s=%.2f", slices.Index(keys, f.MessageKey), f.Kind, f.Probability))
			}
			slices.Sort(flags)
			for _, cl := range v.Clearings {
				earlier := slices.IndexFunc(views, func(w judgment.JudgmentView) bool { return w.ID == cl.CommitmentJudgment })
				clearings = append(clearings, fmt.Sprintf("%d@%d done=%t p=%.2f", slices.Index(keys, cl.CommitmentMessageKey), earlier, cl.Done, cl.Probability))
				if key == jevKey && v.CutOff.After(time.Date(2026, time.February, 1, 0, 0, 0, 0, time.Local)) {
					seen.februaryClearings = append(seen.februaryClearings, cl.Done)
				}
			}
			seen.judgments = append(seen.judgments, fmt.Sprintf("%s #%d cut-off=%s window=%d new=%d flags=%v clearings=%v",
				key, i, v.CutOff.Format(time.RFC3339), slices.Index(keys, v.WindowStart), len(v.NewMessages), flags, clearings))
		}
	}
	return seen
}

// A rerun of a month over threads whose flag and clearing decisions carry
// labels judges and re-checks exactly as the same rerun over the same threads
// without labels: the same requests to each method, the same commitments
// re-checked, the same records.
// spec: KNW-067.reads-no-review-or-label
func TestJudgeMonthReadsNoLabel(t *testing.T) {
	feb := func(day, hour int) time.Time { return time.Date(2026, time.February, day, hour, 0, 0, 0, time.Local) }
	cases := []labelCase{
		{
			name:  "a commitment flag the user labeled as not holding, which the method's next night still re-checks",
			flagP: 0.9,
			messages: func(t *testing.T, th *thr) {
				th.say(t, jan(30, 9), true, promise)
				th.say(t, feb(2, 9), false, "thanks")
			},
			label: func(t *testing.T, e *flagsEnv, th *thr, m decision.Method) {
				_, err := e.threads.LabelFlag(e.ctx, e.lastRun(t, m.Ref()).ID, th.venue, th.keys(t)[0], judgment.FlagKindCommitment, false)
				require.NoError(t, err)
			},
		},
		{
			name:  "a commitment the user Added on a message the method did not flag, which no method re-checks",
			flagP: 0.05,
			messages: func(t *testing.T, th *thr) {
				th.say(t, jan(30, 9), true, promise)
				th.say(t, feb(2, 9), false, "thanks")
			},
			label: func(t *testing.T, e *flagsEnv, th *thr, m decision.Method) {
				_, err := e.threads.LabelFlag(e.ctx, e.lastRun(t, m.Ref()).ID, th.venue, th.keys(t)[0], judgment.FlagKindCommitment, true)
				require.NoError(t, err)
			},
		},
		{
			name:  "a clearing decision the user labeled done where the method answered not done, whose commitment the next night still re-checks",
			flagP: 0.9,
			messages: func(t *testing.T, th *thr) {
				th.say(t, jan(30, 9), true, promise)
				th.say(t, jan(31, 9), false, "thanks")
				th.say(t, feb(2, 9), false, "still waiting")
			},
			label: func(t *testing.T, e *flagsEnv, th *thr, m decision.Method) {
				cleared := th.judgmentAt(t, m.Ref(), time.Date(2026, time.February, 1, 0, 0, 0, 0, time.Local))
				require.Len(t, cleared.Clearings, 1)
				require.False(t, cleared.Clearings[0].Done)
				_, err := e.threads.LabelClearing(e.ctx, e.lastRun(t, m.Ref()).ID, th.venue, cleared.Clearings[0].CommitmentMessageKey, cleared.CutOff, true)
				require.NoError(t, err)
			},
		},
	}
	wantFebruary := [][]bool{{false}, nil, {false}}

	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			without := c.observe(t, false)
			with := c.observe(t, true)

			assert.Equal(t, wantFebruary[i], without.februaryClearings, "the case re-checks as it is meant to")
			assert.Equal(t, without, with)
		})
	}
}
