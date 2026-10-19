//go:build integration_testdb

package judgment_test

import (
	"net/http"
	"testing"
	"time"

	"personal-crm/backend/internal/api/handlers"
	"personal-crm/backend/internal/judgment"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func runIDs(runs []handlers.ReviewRunResponse) []string {
	ids := make([]string, len(runs))
	for i, r := range runs {
		ids[i] = r.ID
	}
	return ids
}

// emailThread seeds an email thread with one message from the contact on the
// day, and returns it with the message's key.
func (b *benchEnv) emailThread(t *testing.T, c uuid.UUID, day int) (thread, string) {
	t.Helper()
	th := b.venueThread(t, repository.InteractionSourceEmail, repository.VenueKindEmailThread, "mail-"+uuid.NewString(), "")
	key := b.say(t, th, c, false, b.day(day))
	b.joined(t, th, c, b.day(day))
	return th, key
}

// The review runs route lists the historical and scheduled runs of the
// compared methods, newest first.
func TestListReviewRuns(t *testing.T) {
	list := func(t *testing.T, b *benchEnv) []handlers.ReviewRunResponse {
		t.Helper()
		return decodeData[handlers.ReviewRunsResponse](t, b.get("/api/v1/review-runs")).Runs
	}

	t.Run("a scorecard rerun of a compared method is never listed, a scheduled run is", func(t *testing.T) {
		// spec: IXN-088.no-rerun-no-removed-method
		b := newBenchEnv(t)
		rerun := b.startRun(t, judgment.RunKindRerun, b.compared(0))
		scheduled := b.startRun(t, judgment.RunKindScheduled, b.compared(0))

		runs := list(t, b)

		assert.Equal(t, []string{scheduled.String()}, runIDs(runs))
		assert.NotContains(t, runIDs(runs), rerun.String())
		assert.Equal(t, "scheduled", runs[0].Kind)
		assert.Equal(t, b.compared(0).Key, runs[0].MethodKey)
		assert.Equal(t, "v1", runs[0].MethodVersion)
		assert.False(t, runs[0].StartedAt.IsZero())
	})

	t.Run("a historical run of a method the registry does not list is never listed", func(t *testing.T) {
		// spec: IXN-088.no-rerun-no-removed-method
		b := newBenchEnv(t)
		b.startRun(t, judgment.RunKindHistorical, otherMethod)
		listed := b.startRun(t, judgment.RunKindHistorical, b.compared(1))

		assert.Equal(t, []string{listed.String()}, runIDs(list(t, b)))
	})

	t.Run("two historical runs list newest first, each with its window", func(t *testing.T) {
		// spec: IXN-088.newest-first
		b := newBenchEnv(t)
		from, to := b.day(1), b.day(30)
		older, err := b.store.StartRun(b.ctx, judgment.RunSpec{Kind: judgment.RunKindHistorical, Method: b.compared(0), From: &from, To: &to})
		require.NoError(t, err)
		time.Sleep(5 * time.Millisecond)
		newer := b.startRun(t, judgment.RunKindHistorical, b.compared(1))

		runs := list(t, b)

		assert.Equal(t, []string{newer.String(), older.String()}, runIDs(runs))
		require.NotNil(t, runs[1].From)
		require.NotNil(t, runs[1].To)
		assert.True(t, from.Equal(*runs[1].From))
		assert.True(t, to.Equal(*runs[1].To))
	})
}

// With a run, the threads list keeps the threads the run judged, adds each
// one's judged count and status, and adds the run's summary.
func TestListThreadsInRun(t *testing.T) {
	t.Run("a thread judged only in another run is left out, and the order is the list's order", func(t *testing.T) {
		// spec: IXN-087.run-keeps-its-threads
		b := newBenchEnv(t)
		c := b.contact(t)
		inRun, inOther, alsoInRun := make([]thread, 0, 2), make([]thread, 0, 1), make([]thread, 0, 1)
		m := b.compared(0)
		run, other := b.startRun(t, judgment.RunKindHistorical, m), b.startRun(t, judgment.RunKindHistorical, m)
		for day := 1; day <= 3; day++ {
			th, key := b.emailThread(t, c, day)
			target := run
			switch day {
			case 2:
				target = other
				inOther = append(inOther, th)
			case 1:
				inRun = append(inRun, th)
			default:
				alsoInRun = append(alsoInRun, th)
			}
			b.judged(t, target, m, th, b.day(day).Add(time.Hour), key, []string{key}, nil)
		}

		list, meta := b.threadList(t, "run_id="+run.String())
		everything, _ := b.threadList(t, "")

		assert.Equal(t, []string{alsoInRun[0].id.String(), inRun[0].id.String()}, threadIDs(list.Items))
		assert.Equal(t, int64(2), meta.Total)
		assert.NotContains(t, threadIDs(list.Items), inOther[0].id.String())
		assert.Equal(t, []string{everything.Items[0].ThreadID, everything.Items[2].ThreadID}, threadIDs(list.Items), "the order without the run")
	})

	t.Run("a thread whose judgment the run reused is kept and counted", func(t *testing.T) {
		// spec: IXN-087.run-keeps-its-threads
		b := newBenchEnv(t)
		c := b.contact(t)
		th, key := b.emailThread(t, c, 1)
		m := b.compared(0)
		first := b.startRun(t, judgment.RunKindHistorical, m)
		cutOff := b.day(1).Add(time.Hour)
		b.judged(t, first, m, th, cutOff, key, []string{key}, nil)
		reusing := b.startRun(t, judgment.RunKindHistorical, m)
		j := windowed(m, th, cutOff, "h-"+cutOff.UTC().Format(time.RFC3339Nano)+"-"+key, key, []string{key}, judgment.Outcomes{})
		_, reused, err := b.store.ReuseRead(b.ctx, reusing, th.id, cutOff, j.InputHash, judgment.Read{WindowStart: key, NewMessages: []string{key}})
		require.NoError(t, err)
		require.True(t, reused)

		list, _ := b.threadList(t, "run_id="+reusing.String())

		require.Len(t, list.Items, 1)
		require.NotNil(t, list.Items[0].Review)
		assert.Equal(t, 1, list.Items[0].Review.JudgedCount)
	})

	t.Run("a night holding a message_over_limit message leaves it out of the judged count", func(t *testing.T) {
		// spec: IXN-087.run-keeps-its-threads
		b := newBenchEnv(t)
		c := b.contact(t)
		th := b.venueThread(t, repository.InteractionSourceEmail, repository.VenueKindEmailThread, "mail-"+uuid.NewString(), "")
		keys := b.talk(t, th, 3, c)
		m := b.compared(0)
		run := b.startRun(t, judgment.RunKindHistorical, m)
		cutOff := b.day(3).Add(time.Hour)
		b.judged(t, run, m, th, cutOff, keys[0], keys[:2], nil)
		require.NoError(t, b.store.RecordUnjudged(b.ctx, run, []judgment.UnjudgedMessage{
			{Thread: th.id, CutOff: cutOff, MessageKey: keys[2], Reason: judgment.UnjudgedReasonMessageOverLimit},
		}))

		list, _ := b.threadList(t, "run_id="+run.String())

		require.Len(t, list.Items, 1)
		assert.Equal(t, 2, list.Items[0].Review.JudgedCount, "the unjudged message is no judged message")
	})

	t.Run("the summary counts equal the counts of the rows' statuses", func(t *testing.T) {
		// spec: IXN-087.run-summary
		b := newBenchEnv(t)
		c := b.contact(t)
		m := b.compared(0)
		run := b.startRun(t, judgment.RunKindHistorical, m)
		hot := b.threshold(t, m, judgment.FlagKindClaim) + 0.1
		want := map[string]int{}
		for i, shown := range []bool{true, true, false, false, false} {
			th, key := b.emailThread(t, c, i+1)
			p := map[string][3]float64{key: {0, 0, 0}}
			if shown {
				p[key] = [3]float64{0, 0, hot}
			}
			night := b.judged(t, run, m, th, b.day(i+1).Add(time.Hour), key, []string{key}, p)
			if i == 3 {
				w := b.post(t, "/api/v1/judgments/"+night.String()+"/confirm-nones?run_id="+run.String(), nil)
				require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			}
		}
		list, _ := b.threadList(t, "run_id="+run.String())
		for _, row := range list.Items {
			want[row.Review.Status]++
		}

		require.NotNil(t, list.Summary)
		assert.Equal(t, 5, list.Summary.Judged)
		assert.Equal(t, want["to_review"], list.Summary.ToReview)
		assert.Equal(t, want["nothing_flagged"], list.Summary.NothingFlagged)
		assert.Equal(t, want["reviewed"], list.Summary.Reviewed)
		assert.Equal(t, map[string]int{"to_review": 2, "nothing_flagged": 2, "reviewed": 1}, want)
	})

	t.Run("an unknown run is not found", func(t *testing.T) {
		// spec: IXN-087.unknown-run-not-found
		b := newBenchEnv(t)

		assert.Equal(t, http.StatusNotFound, b.get("/api/v1/threads?run_id="+uuid.NewString()).Code)
	})
}
