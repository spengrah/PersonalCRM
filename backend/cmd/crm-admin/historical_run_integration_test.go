//go:build integration_testdb

package main

import (
	"bytes"
	"os"
	"testing"
	"time"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/judgment"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dec31 is a time on December 31, 2025, the day before January's first, in
// the server's timezone.
func dec31(hour, minute int) time.Time {
	return time.Date(2025, time.December, 31, hour, minute, 0, 0, time.Local)
}

// cutOffsOf returns the cut-offs of a thread's judgments by the method
// version, in order, written in UTC.
func cutOffsOf(t *testing.T, th *thr, m judgment.MethodRef) []string {
	t.Helper()
	var out []string
	for _, j := range th.judgments(t, m) {
		out = append(out, j.CutOff.UTC().Format(time.RFC3339))
	}
	return out
}

// januaryCutOffs returns the cut-offs that close January's days first to
// last, written in UTC.
func januaryCutOffs(first, last int) []string {
	var out []string
	for day := first; day <= last; day++ {
		out = append(out, jan(day+1, 0).UTC().Format(time.RFC3339))
	}
	return out
}

// daily is a thread with a message on December 31 and on each of January's
// first eight days.
func (e *flagsEnv) daily(t *testing.T) *thr {
	t.Helper()
	th := e.newThread(t)
	th.say(t, dec31(9, 0), false, filler(0))
	for day := 1; day <= 8; day++ {
		th.say(t, jan(day, 9), day%2 == 0, filler(day))
	}
	return th
}

// A historical run over a range of days judges at the nightly cut-off that
// closes each day of the range, both ends included, and at no other; content
// before the range reads as context but makes no thread eligible; --measure
// over the range builds the contexts a run over it judges and sends nothing.
// spec: KNW-051.nightly-cut-offs, KNW-051.gained-content-eligible, KNW-051.whole-content-context, KNW-054.measuring-run-sends-nothing
func TestHistoricalRunJudgesTheCutOffsOfItsDays(t *testing.T) {
	t.Parallel()

	t.Run("2026-01-01 to 2026-01-07, judged at exactly the seven cut-offs closing those days", func(t *testing.T) {
		t.Parallel()
		e := newFlagsEnv(t, "")
		th := e.daily(t)
		m := e.method(jevKey, newVendor(t), roomy)

		out, err := e.tryJudgeDays("2026-01-01", "2026-01-07", methods{jevKey: m}, jevKey, false)

		require.NoError(t, err)
		assert.Contains(t, out, "cut_offs=7 eligible=7")
		assert.Equal(t, januaryCutOffs(1, 7), cutOffsOf(t, th, m.Ref()))
	})

	t.Run("a thread whose only content is on the day before the start or the day after the end, not judged", func(t *testing.T) {
		t.Parallel()
		e := newFlagsEnv(t, "")
		before, after, inside := e.newThread(t), e.newThread(t), e.newThread(t)
		before.say(t, dec31(12, 0), false, filler(0))
		after.say(t, jan(8, 12), false, filler(1))
		inside.say(t, jan(4, 12), false, filler(2))
		m := e.method(jevKey, newVendor(t), roomy)

		_, err := e.tryJudgeDays("2026-01-01", "2026-01-07", methods{jevKey: m}, jevKey, false)

		require.NoError(t, err)
		assert.Empty(t, before.judgments(t, m.Ref()), "content on the day before the start")
		assert.Empty(t, after.judgments(t, m.Ref()), "content on the day after the end")
		assert.Equal(t, januaryCutOffs(4, 4), cutOffsOf(t, inside, m.Ref()), "content inside the range")
	})

	t.Run("content just before the start day's opening, read as context but making no thread eligible", func(t *testing.T) {
		t.Parallel()
		e := newFlagsEnv(t, "")
		early, only := e.newThread(t), e.newThread(t)
		early.say(t, dec31(23, 59), false, "the early note")
		early.say(t, jan(3, 9), true, filler(1))
		only.say(t, dec31(23, 59), false, filler(2))
		v := newVendor(t)
		m := e.method(jevKey, v, roomy)

		_, err := e.tryJudgeDays("2026-01-01", "2026-01-07", methods{jevKey: m}, jevKey, false)

		require.NoError(t, err)
		assert.Empty(t, only.judgments(t, m.Ref()), "a thread whose only content precedes the start day")
		assert.Equal(t, januaryCutOffs(3, 3), cutOffsOf(t, early, m.Ref()))
		require.Len(t, v.all(), 1)
		assert.Contains(t, v.all()[0].Texts, "the early note", "the earlier content is read as context")
	})

	t.Run("a range of one day, judged at one cut-off", func(t *testing.T) {
		t.Parallel()
		e := newFlagsEnv(t, "")
		th := e.daily(t)
		m := e.method(jevKey, newVendor(t), roomy)

		out, err := e.tryJudgeDays("2026-01-03", "2026-01-03", methods{jevKey: m}, jevKey, false)

		require.NoError(t, err)
		assert.Contains(t, out, "cut_offs=1 eligible=1")
		assert.Equal(t, januaryCutOffs(3, 3), cutOffsOf(t, th, m.Ref()))
	})

	t.Run("--measure over the range, building the contexts a run over the range judges and sending nothing", func(t *testing.T) {
		t.Parallel()
		e := newFlagsEnv(t, "")
		th := e.daily(t)
		v := newVendor(t)
		m := e.method(jevKey, v, roomy)

		out, err := e.tryJudgeDays("2026-01-01", "2026-01-07", methods{jevKey: m}, jevKey, true)

		require.NoError(t, err)
		assert.Contains(t, out, "cut_offs=7 contexts=7")
		assert.Empty(t, v.all(), "nothing is sent")
		assert.Empty(t, th.judgments(t, m.Ref()), "no judgment is written")
		runs, err := e.store.ListRuns(e.ctx)
		require.NoError(t, err)
		assert.Empty(t, runs, "no run is written")

		judged, err := e.tryJudgeDays("2026-01-01", "2026-01-07", methods{jevKey: m}, jevKey, false)
		require.NoError(t, err)
		assert.Contains(t, judged, "eligible=7", "a run over the range judges the contexts the measuring run built")
	})
}

// A historical run whose masker would not know the user's own names fails
// before any call: building the command's stack refuses the masker when
// OWN_NAMES is unset or holds only separators and spaces, so no run starts
// and no vendor is called. With OWN_NAMES set, the run judges. The setting
// reaches the stack through config.Load, the command's one reader of it.
// spec: KNW-089.refused-without-own-names
func TestHistoricalRunRefusesWithoutOwnNames(t *testing.T) {
	cases := []struct {
		name  string
		set   bool
		value string
		runs  bool
	}{
		{name: "OWN_NAMES unset"},
		{name: "OWN_NAMES of only separators and spaces", set: true, value: " , ,  "},
		{name: "OWN_NAMES set", set: true, value: "Marlowe Quill, Marlowe", runs: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
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
			t.Setenv("NAME_MODEL_SOCKET", e.names.socket)
			t.Setenv("OWN_NAMES", tc.value)
			if !tc.set {
				require.NoError(t, os.Unsetenv("OWN_NAMES"))
			}
			cfg, err := config.Load()
			require.NoError(t, err)
			opts := runOptions{judgeStartDay: "2026-01-01", judgeEndDay: "2026-01-07", judgeMethod: jevKey}

			deps, cleanup, err := buildProductionDeps(e.ctx, cfg, e.database, opts)

			runs := func() []judgment.RunSummary {
				listed, err := e.store.ListRuns(e.ctx)
				require.NoError(t, err)
				return listed
			}
			if !tc.runs {
				require.ErrorContains(t, err, "OWN_NAMES")
				assert.Empty(t, venice.all(), "nothing is sent")
				assert.Empty(t, runs(), "no run is started")
				return
			}
			require.NoError(t, err)
			t.Cleanup(cleanup)
			deps.stdout, deps.stderr = &bytes.Buffer{}, &bytes.Buffer{}
			require.NoError(t, run(e.ctx, opts, deps))
			assert.NotEmpty(t, venice.all(), "the run sends its masked contexts")
			assert.Len(t, runs(), 1)
		})
	}
}
