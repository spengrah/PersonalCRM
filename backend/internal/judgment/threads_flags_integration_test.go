//go:build integration_testdb

package judgment_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"personal-crm/backend/internal/api/handlers"
	"personal-crm/backend/internal/judgment"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// labelFlag posts the user's label of a flag decision.
func (b *benchEnv) labelFlag(t *testing.T, th thread, run judgment.RunID, key string, kind string, holds bool) *httptest.ResponseRecorder {
	t.Helper()
	return b.post(t, "/api/v1/threads/"+th.id.String()+"/flag-labels?run_id="+run.String(),
		map[string]any{"message_key": key, "kind": kind, "holds": holds})
}

// labelClearing posts the user's label of a clearing decision.
func (b *benchEnv) labelClearing(t *testing.T, th thread, run judgment.RunID, key string, cutOff time.Time, done bool) *httptest.ResponseRecorder {
	t.Helper()
	return b.post(t, "/api/v1/threads/"+th.id.String()+"/clearing-labels?run_id="+run.String(),
		map[string]any{"commitment_message_key": key, "cut_off": cutOff, "done": done})
}

// labeled posts a flag label that must succeed and returns the answer's state.
func (b *benchEnv) labeled(t *testing.T, th thread, run judgment.RunID, key, kind string, holds bool) handlers.ThreadReviewResponse {
	t.Helper()
	return decodeData[handlers.ThreadReviewResponse](t, b.labelFlag(t, th, run, key, kind, holds))
}

// scenario is a thread of three messages judged by the first compared method in
// one run, in one night after the third message.
type scenario struct {
	th    thread
	keys  []string
	m     judgment.MethodRef
	run   judgment.RunID
	night uuid.UUID
}

// judgedNight seeds the scenario with the probabilities per message, in
// commitment, big moment, claim order.
func (b *benchEnv) judgedNight(t *testing.T, p ...[3]float64) scenario {
	t.Helper()
	th, keys := b.conversation(t, 3)
	m := b.compared(0)
	run := b.startRun(t, judgment.RunKindHistorical, m)
	probabilities := map[string][3]float64{}
	for i, v := range p {
		probabilities[keys[i]] = v
	}
	night := b.judged(t, run, m, th, b.cutAfter(3), keys[0], keys, probabilities)
	return scenario{th: th, keys: keys, m: m, run: run, night: night}
}

// A message shows, per flag kind, a chip under its night's method.
func TestReadThreadChips(t *testing.T) {
	t.Run("a probability exactly at the display threshold is shown, to review", func(t *testing.T) {
		// spec: IXN-089.night-contents
		b := newBenchEnv(t)
		m := b.compared(0)
		at := b.threshold(t, m, judgment.FlagKindCommitment)
		s := b.judgedNight(t, [3]float64{at, 0, 0})

		night := b.reviewOf(t, s.th, s.run).Nights[0]

		chip := chipOf(night, s.keys[0], judgment.FlagKindCommitment)
		require.NotNil(t, chip)
		assert.Equal(t, "to_review", chip.State)
		require.NotNil(t, chip.Probability)
		assert.Equal(t, at, *chip.Probability)
	})

	t.Run("a probability below the threshold with no label shows no chip", func(t *testing.T) {
		// spec: IXN-089.night-contents
		b := newBenchEnv(t)
		below := b.threshold(t, b.compared(0), judgment.FlagKindClaim) - 0.1
		s := b.judgedNight(t, [3]float64{0, 0, below})

		night := b.reviewOf(t, s.th, s.run).Nights[0]

		assert.Empty(t, nightMessage(t, night, s.keys[0]).Chips)
	})

	t.Run("a label made while another method's run was read shows under this method's chip", func(t *testing.T) {
		// spec: IXN-090.shared-by-every-judgment
		b := newBenchEnv(t)
		hot := b.threshold(t, b.compared(0), judgment.FlagKindClaim) + 0.2
		th, keys := b.conversation(t, 1)
		first, second := b.compared(0), b.compared(1)
		runA, runB := b.startRun(t, judgment.RunKindHistorical, first), b.startRun(t, judgment.RunKindHistorical, second)
		p := map[string][3]float64{keys[0]: {0, 0, hot}}
		b.judged(t, runA, first, th, b.cutAfter(1), keys[0], keys, p)
		b.judged(t, runB, second, th, b.cutAfter(1), keys[0], keys, p)
		before := b.reviewOf(t, th, runB).Nights[0]
		require.Equal(t, "to_review", chipOf(before, keys[0], judgment.FlagKindClaim).State)

		b.labeled(t, th, runA, keys[0], "claim", true)

		after := b.reviewOf(t, th, runB).Nights[0]
		assert.Equal(t, "confirmed", chipOf(after, keys[0], judgment.FlagKindClaim).State)
	})

	t.Run("an unflagged message labeled as holding a kind is added, with no probability", func(t *testing.T) {
		// spec: IXN-089.night-contents
		b := newBenchEnv(t)
		s := b.judgedNight(t, [3]float64{0, 0.02, 0})
		b.labeled(t, s.th, s.run, s.keys[0], "big_moment", true)

		night := b.reviewOf(t, s.th, s.run).Nights[0]

		chip := chipOf(night, s.keys[0], judgment.FlagKindBigMoment)
		require.NotNil(t, chip)
		assert.Equal(t, "added", chip.State)
		assert.Nil(t, chip.Probability)
	})

	t.Run("an unjudged message labeled as holding a kind is added", func(t *testing.T) {
		// spec: IXN-089.night-contents
		b := newBenchEnv(t)
		th, keys := b.conversation(t, 2)
		m := b.compared(0)
		run := b.startRun(t, judgment.RunKindHistorical, m)
		b.judged(t, run, m, th, b.cutAfter(2), keys[0], keys[:1], nil)
		require.False(t, nightMessage(t, b.reviewOf(t, th, run).Nights[0], keys[1]).Judged)

		b.labeled(t, th, run, keys[1], "commitment", true)

		night := b.reviewOf(t, th, run).Nights[0]
		chip := chipOf(night, keys[1], judgment.FlagKindCommitment)
		require.NotNil(t, chip)
		assert.Equal(t, "added", chip.State)
		assert.Nil(t, chip.Probability)
	})

	t.Run("a shown flag the user removed reads removed, and one the user confirmed reads confirmed", func(t *testing.T) {
		// spec: IXN-090.label-recorded
		b := newBenchEnv(t)
		hot := b.threshold(t, b.compared(0), judgment.FlagKindClaim) + 0.2
		s := b.judgedNight(t, [3]float64{hot, 0, hot})
		b.labeled(t, s.th, s.run, s.keys[0], "claim", false)
		b.labeled(t, s.th, s.run, s.keys[0], "commitment", true)

		night := b.reviewOf(t, s.th, s.run).Nights[0]

		assert.Equal(t, "removed", chipOf(night, s.keys[0], judgment.FlagKindClaim).State)
		assert.Equal(t, "confirmed", chipOf(night, s.keys[0], judgment.FlagKindCommitment).State)
		assert.NotNil(t, chipOf(night, s.keys[0], judgment.FlagKindClaim).Probability, "a shown chip keeps its probability")
	})

	clearing := func(t *testing.T, done bool) (*benchEnv, scenario, handlers.ThreadReviewResponse, time.Time) {
		t.Helper()
		b := newBenchEnv(t)
		m := b.compared(0)
		hot := b.threshold(t, m, judgment.FlagKindCommitment) + 0.2
		th, keys := b.conversation(t, 2)
		run := b.startRun(t, judgment.RunKindHistorical, m)
		first := b.judged(t, run, m, th, b.cutAfter(1), keys[0], keys[:1], map[string][3]float64{keys[0]: {hot, 0, 0}})
		later := windowed(m, th, b.cutAfter(2), "h-later", keys[0], keys[1:], judgment.Outcomes{})
		later.Flags = flagsOf(keys[1], 0, 0, 0)
		later.Clearings = []judgment.ClearingDecision{{CommitmentMessageKey: keys[0], CommitmentJudgment: first, Done: done, Probability: 0.9}}
		_, err := b.store.RecordJudgment(b.ctx, run, later)
		require.NoError(t, err)
		return b, scenario{th: th, keys: keys, m: m, run: run}, b.reviewOf(t, th, run), b.cutAfter(2)
	}

	t.Run("a commitment a later night judged done names that night's cut-off", func(t *testing.T) {
		// spec: IXN-089.night-contents
		_, s, review, cleared := clearing(t, true)

		chip := chipOf(review.Nights[0], s.keys[0], judgment.FlagKindCommitment)

		require.NotNil(t, chip)
		require.NotNil(t, chip.ClearedAt)
		assert.True(t, cleared.Equal(*chip.ClearedAt))
	})

	t.Run("a commitment a later night judged not done names none", func(t *testing.T) {
		// spec: IXN-089.night-contents
		_, s, review, _ := clearing(t, false)

		chip := chipOf(review.Nights[0], s.keys[0], judgment.FlagKindCommitment)

		require.NotNil(t, chip)
		assert.Nil(t, chip.ClearedAt)
	})
}

// A night also carries its clearing decisions, the messages it came closest
// to flagging, and its status.
func TestReadThreadNightContents(t *testing.T) {
	t.Run("a night with two clearing decisions, one labeled", func(t *testing.T) {
		// spec: IXN-089.night-contents
		b := newBenchEnv(t)
		m := b.compared(0)
		hot := b.threshold(t, m, judgment.FlagKindCommitment) + 0.2
		th, keys := b.conversation(t, 3)
		run := b.startRun(t, judgment.RunKindHistorical, m)
		first := b.judged(t, run, m, th, b.cutAfter(2), keys[0], keys[:2], map[string][3]float64{keys[0]: {hot, 0, 0}, keys[1]: {hot, 0, 0}})
		later := windowed(m, th, b.cutAfter(3), "h-later", keys[0], keys[2:], judgment.Outcomes{})
		later.Flags = flagsOf(keys[2], 0, 0, 0)
		later.Clearings = []judgment.ClearingDecision{
			{CommitmentMessageKey: keys[0], CommitmentJudgment: first, Done: true, Probability: 0.9},
			{CommitmentMessageKey: keys[1], CommitmentJudgment: first, Done: false, Probability: 0.6},
		}
		_, err := b.store.RecordJudgment(b.ctx, run, later)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, b.labelClearing(t, th, run, keys[0], b.cutAfter(3), true).Code)

		night := b.reviewOf(t, th, run).Nights[1]

		require.Len(t, night.Clearings, 2)
		assert.Equal(t, keys[0], night.Clearings[0].CommitmentMessageKey)
		assert.True(t, b.day(1).Equal(night.Clearings[0].CommitmentSentAt))
		assert.True(t, night.Clearings[0].Done)
		assert.Equal(t, 0.9, night.Clearings[0].Probability)
		assert.Equal(t, "confirmed", night.Clearings[0].Label)
		assert.Equal(t, "none", night.Clearings[1].Label)
		assert.False(t, night.Clearings[1].Done)
		assert.Equal(t, 1, night.ToReview, "the clearing decision without a label")
		assert.True(t, night.HoldsLabel)
		assert.False(t, night.ConfirmNones)
	})

	t.Run("a night shows the clearing decisions only of commitments that show", func(t *testing.T) {
		// spec: IXN-089.clearings-of-shown-commitments
		b := newBenchEnv(t)
		m := b.compared(0)
		threshold := b.threshold(t, m, judgment.FlagKindCommitment)
		hot, faint := threshold+0.2, threshold-0.1
		th, keys := b.conversation(t, 4)
		run := b.startRun(t, judgment.RunKindHistorical, m)
		first := b.judged(t, run, m, th, b.cutAfter(3), keys[0], keys[:3],
			map[string][3]float64{keys[0]: {hot, 0, 0}, keys[1]: {faint, 0, 0}, keys[2]: {faint, 0, 0}})
		later := windowed(m, th, b.cutAfter(4), "h-later", keys[0], keys[3:], judgment.Outcomes{})
		later.Flags = flagsOf(keys[3], 0, 0, 0)
		later.Clearings = []judgment.ClearingDecision{
			{CommitmentMessageKey: keys[0], CommitmentJudgment: first, Done: true, Probability: 0.9},
			{CommitmentMessageKey: keys[1], CommitmentJudgment: first, Done: true, Probability: 0.9},
			{CommitmentMessageKey: keys[2], CommitmentJudgment: first, Done: true, Probability: 0.9},
		}
		_, err := b.store.RecordJudgment(b.ctx, run, later)
		require.NoError(t, err)
		b.labeled(t, th, run, keys[2], "commitment", true)

		night := b.reviewOf(t, th, run).Nights[1]

		var shown []string
		for _, c := range night.Clearings {
			shown = append(shown, c.CommitmentMessageKey)
		}
		assert.Equal(t, []string{keys[0], keys[2]}, shown, "the commitment below the display threshold, never shown, is not re-checked on screen; the one the user added is")
		assert.Equal(t, 2, night.ToReview, "only the clearing decisions shown await a label")
	})

	closest := func(t *testing.T, kind judgment.FlagKind, probabilities ...float64) handlers.ClosestUnflaggedResponse {
		t.Helper()
		b := newBenchEnv(t)
		var rows [][3]float64
		for _, p := range probabilities {
			var row [3]float64
			row[indexOfKind(kind)] = p
			rows = append(rows, row)
		}
		s := b.judgedNight(t, rows...)
		night := b.reviewOf(t, s.th, s.run).Nights[0]
		require.Len(t, night.ClosestUnflagged, 3, "one entry per flag kind")
		for _, c := range night.ClosestUnflagged {
			if c.Kind == string(kind) {
				if c.Message != nil {
					require.NotEmpty(t, c.Message.MessageKey)
					assert.Contains(t, s.keys, c.Message.MessageKey)
					assert.Equal(t, b.contactNameOfSender(t, s, c.Message.MessageKey), c.Message.Sender)
				}
				return c
			}
		}
		require.Fail(t, "no entry for kind")
		return handlers.ClosestUnflaggedResponse{}
	}

	t.Run("a kind whose highest unflagged probability is just below the floor names none", func(t *testing.T) {
		// spec: IXN-089.night-contents
		got := closest(t, judgment.FlagKindClaim, judgment.ClosestFloor-0.01, 0.02)

		assert.Nil(t, got.Message)
		assert.Nil(t, got.Probability)
	})

	t.Run("a kind whose only unflagged probability above zero is at the floor names that message", func(t *testing.T) {
		// spec: IXN-089.night-contents
		got := closest(t, judgment.FlagKindClaim, 0, judgment.ClosestFloor)

		require.NotNil(t, got.Message)
		require.NotNil(t, got.Probability)
		assert.Equal(t, judgment.ClosestFloor, *got.Probability)
	})

	t.Run("a judged message showing no chip carries its highest kind at or above the floor as its near miss", func(t *testing.T) {
		b := newBenchEnv(t)
		floor := judgment.ClosestFloor
		require.Less(t, floor+0.05, b.threshold(t, b.compared(0), judgment.FlagKindBigMoment), "below the display threshold")
		s := b.judgedNight(t,
			[3]float64{floor + 0.01, floor + 0.05, 0}, // both reach the floor: the higher, big moment
			[3]float64{floor - 0.01, 0, 0},            // below the floor: none
			[3]float64{floor, floor, 0},               // a tie: the first kind
		)

		messages := b.reviewOf(t, s.th, s.run).Nights[0].Messages

		require.Len(t, messages, 3)
		require.NotNil(t, messages[0].NearMiss)
		assert.Equal(t, "big_moment", messages[0].NearMiss.Kind)
		assert.Equal(t, floor+0.05, messages[0].NearMiss.Probability)
		assert.Nil(t, messages[1].NearMiss)
		require.NotNil(t, messages[2].NearMiss)
		assert.Equal(t, "commitment", messages[2].NearMiss.Kind)
	})

	t.Run("a message showing a chip carries no near miss", func(t *testing.T) {
		b := newBenchEnv(t)
		shown := b.threshold(t, b.compared(0), judgment.FlagKindCommitment) + 0.1
		s := b.judgedNight(t, [3]float64{shown, judgment.ClosestFloor + 0.05, 0})

		messages := b.reviewOf(t, s.th, s.run).Nights[0].Messages

		require.NotEmpty(t, messages)
		require.Len(t, messages[0].Chips, 1)
		assert.Nil(t, messages[0].NearMiss)
	})

	t.Run("unflagged messages below the threshold and a shown chip name the highest unflagged one", func(t *testing.T) {
		// spec: IXN-089.night-contents
		b := newBenchEnv(t)
		at := b.threshold(t, b.compared(0), judgment.FlagKindClaim)
		s := b.judgedNight(t, [3]float64{0, 0, 0.1}, [3]float64{0, 0, at * 0.8}, [3]float64{0, 0, at + 0.6})

		night := b.reviewOf(t, s.th, s.run).Nights[0]

		var got handlers.ClosestUnflaggedResponse
		for _, c := range night.ClosestUnflagged {
			if c.Kind == "claim" {
				got = c
			}
		}
		require.NotNil(t, got.Message)
		assert.Equal(t, s.keys[1], got.Message.MessageKey, "not the shown message, and not the lower one")
		assert.Equal(t, at*0.8, *got.Probability)
	})

	t.Run("a night whose only shown item is a reply item offers no Confirm Nones", func(t *testing.T) {
		// spec: IXN-094.night-status
		b := newBenchEnv(t)
		th, keys := b.conversation(t, 1)
		x := b.contact(t)
		m := b.compared(0)
		run := b.startRun(t, judgment.RunKindHistorical, m)
		j := windowed(m, th, b.cutAfter(1), "h1", keys[0], keys, judgment.Outcomes{
			BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(x, judgment.BallInCourtIOweReply, 0.9)},
		})
		j.Flags = flagsOf(keys[0], 0, 0, 0)
		_, err := b.store.RecordJudgment(b.ctx, run, j)
		require.NoError(t, err)

		night := b.reviewOf(t, th, run).Nights[0]

		assert.False(t, night.ConfirmNones)
		assert.Equal(t, 1, night.ToReview)
		assert.False(t, night.HoldsLabel)
		require.Len(t, night.Proposal.Items, 1)
	})

	t.Run("a night showing nothing and holding no label offers Confirm Nones", func(t *testing.T) {
		// spec: IXN-094.night-status
		b := newBenchEnv(t)
		s := b.judgedNight(t, [3]float64{0, 0, 0})

		night := b.reviewOf(t, s.th, s.run).Nights[0]

		assert.True(t, night.ConfirmNones)
		assert.Zero(t, night.ToReview)
		assert.False(t, night.HoldsLabel)
	})

	t.Run("a night whose only label came from another run of the same thread holds a label", func(t *testing.T) {
		// spec: IXN-094.night-status
		b := newBenchEnv(t)
		th, keys := b.conversation(t, 1)
		first, second := b.compared(0), b.compared(1)
		runA, runB := b.startRun(t, judgment.RunKindHistorical, first), b.startRun(t, judgment.RunKindHistorical, second)
		b.judged(t, runA, first, th, b.cutAfter(1), keys[0], keys, nil)
		b.judged(t, runB, second, th, b.cutAfter(1), keys[0], keys, nil)
		require.True(t, b.reviewOf(t, th, runB).Nights[0].ConfirmNones)

		b.labeled(t, th, runA, keys[0], "claim", true)

		night := b.reviewOf(t, th, runB).Nights[0]
		assert.True(t, night.HoldsLabel)
		assert.False(t, night.ConfirmNones)
	})
}

func indexOfKind(kind judgment.FlagKind) int {
	for i, k := range judgment.FlagKinds {
		if k == kind {
			return i
		}
	}
	return -1
}

// contactNameOfSender is the name a closest unflagged message's sender should
// read: the only contact of the scenario's conversation.
func (b *benchEnv) contactNameOfSender(t *testing.T, s scenario, key string) string {
	t.Helper()
	read := b.readThread(t, s.th.id.String(), "")
	for _, m := range read.Messages {
		if m.MessageKey == key {
			return m.Sender
		}
	}
	require.Failf(t, "no message", key)
	return ""
}

// The flag label route records whether a message holds a kind.
func TestLabelFlag(t *testing.T) {
	t.Run("a confirm on a shown flag reads confirmed, a remove removed, and a second label replaces the first", func(t *testing.T) {
		// spec: IXN-090.label-recorded, IXN-090.answers-review-state
		b := newBenchEnv(t)
		hot := b.threshold(t, b.compared(0), judgment.FlagKindClaim) + 0.2
		s := b.judgedNight(t, [3]float64{0, 0, hot})
		state := func() string {
			return chipOf(b.reviewOf(t, s.th, s.run).Nights[0], s.keys[0], judgment.FlagKindClaim).State
		}
		require.Equal(t, "to_review", state())

		answer := b.labeled(t, s.th, s.run, s.keys[0], "claim", true)
		confirmed := state()
		b.labeled(t, s.th, s.run, s.keys[0], "claim", false)
		removed := state()

		assert.Equal(t, "confirmed", confirmed)
		assert.Equal(t, "removed", removed)
		assert.Equal(t, "claim", chipOf(answer.Nights[0], s.keys[0], judgment.FlagKindClaim).Kind)
		assert.Equal(t, "confirmed", chipOf(answer.Nights[0], s.keys[0], judgment.FlagKindClaim).State, "the answer is the thread's review state in the run")
		labels, err := b.store.LabelsByThread(b.ctx, s.th.id)
		require.NoError(t, err)
		assert.Equal(t, []judgment.FlagLabel{{Thread: s.th.id, MessageKey: s.keys[0], Kind: judgment.FlagKindClaim, Holds: false}}, labels.Flags, "one current label")
	})

	t.Run("a label on a kind the method did not flag reads added", func(t *testing.T) {
		// spec: IXN-090.label-recorded
		b := newBenchEnv(t)
		s := b.judgedNight(t, [3]float64{0, 0, 0})

		answer := b.labeled(t, s.th, s.run, s.keys[1], "commitment", true)

		chip := chipOf(answer.Nights[0], s.keys[1], judgment.FlagKindCommitment)
		require.NotNil(t, chip)
		assert.Equal(t, "added", chip.State)
	})

	t.Run("a request that cannot be honoured is refused and records nothing", func(t *testing.T) {
		// spec: IXN-090.invalid-refused
		b := newBenchEnv(t)
		s := b.judgedNight(t, [3]float64{0, 0, 0})
		otherThread, otherKeys := b.conversation(t, 1)
		path := "/api/v1/threads/" + s.th.id.String() + "/flag-labels"
		body := map[string]any{"message_key": s.keys[0], "kind": "claim", "holds": true}
		with := func(overrides map[string]any) map[string]any {
			out := map[string]any{}
			for k, v := range body {
				out[k] = v
			}
			for k, v := range overrides {
				out[k] = v
			}
			return out
		}

		cases := []struct {
			name string
			path string
			body map[string]any
			want int
		}{
			{"an unknown kind", path + "?run_id=" + s.run.String(), with(map[string]any{"kind": "wish"}), http.StatusBadRequest},
			{"a message of another thread", path + "?run_id=" + s.run.String(), with(map[string]any{"message_key": otherKeys[0]}), http.StatusBadRequest},
			{"a request without run_id", path, body, http.StatusBadRequest},
			{"an unknown thread", "/api/v1/threads/" + uuid.NewString() + "/flag-labels?run_id=" + s.run.String(), body, http.StatusNotFound},
			{"an unknown run", path + "?run_id=" + uuid.NewString(), body, http.StatusNotFound},
			{"a missing holds", path + "?run_id=" + s.run.String(), map[string]any{"message_key": s.keys[0], "kind": "claim"}, http.StatusBadRequest},
		}
		for _, c := range cases {
			assert.Equal(t, c.want, b.post(t, c.path, c.body).Code, c.name)
		}

		for _, th := range []thread{s.th, otherThread} {
			labels, err := b.store.LabelsByThread(b.ctx, th.id)
			require.NoError(t, err)
			assert.Empty(t, labels.Flags, "nothing recorded")
		}
	})
}

// The clearing label route records whether a commitment is done as of a
// cut-off.
func TestLabelClearing(t *testing.T) {
	// seeded is a commitment flagged in one night and re-checked in the next by
	// each of the given methods' runs, answering done.
	type seeded struct {
		b       *benchEnv
		th      thread
		keys    []string
		runs    []judgment.RunID
		cutOff  time.Time
		methods []judgment.MethodRef
	}
	seed := func(t *testing.T, done bool, methods ...int) seeded {
		t.Helper()
		b := newBenchEnv(t)
		th, keys := b.conversation(t, 2)
		s := seeded{b: b, th: th, keys: keys, cutOff: b.cutAfter(2)}
		for _, mi := range methods {
			m := b.compared(mi)
			hot := b.threshold(t, m, judgment.FlagKindCommitment) + 0.2
			run := b.startRun(t, judgment.RunKindHistorical, m)
			first := b.judged(t, run, m, th, b.cutAfter(1), keys[0], keys[:1], map[string][3]float64{keys[0]: {hot, 0, 0}})
			later := windowed(m, th, s.cutOff, "h-later", keys[0], keys[1:], judgment.Outcomes{})
			later.Flags = flagsOf(keys[1], 0, 0, 0)
			later.Clearings = []judgment.ClearingDecision{{CommitmentMessageKey: keys[0], CommitmentJudgment: first, Done: done, Probability: 0.8}}
			_, err := b.store.RecordJudgment(b.ctx, run, later)
			require.NoError(t, err)
			s.runs = append(s.runs, run)
			s.methods = append(s.methods, m)
		}
		return s
	}
	label := func(t *testing.T, s seeded, run int) string {
		t.Helper()
		return s.b.reviewOf(t, s.th, s.runs[run]).Nights[1].Clearings[0].Label
	}

	for _, c := range []struct {
		name    string
		answer  bool
		label   bool
		want    string
		comment string
	}{
		{"a done answer confirmed", true, true, "confirmed", ""},
		{"a not-done answer removed when the label says done", false, true, "removed", ""},
		{"a done answer removed when the label says not done", true, false, "removed", ""},
		{"a not-done answer confirmed", false, false, "confirmed", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			// spec: IXN-091.label-recorded, IXN-091.answers-review-state, IXN-094.returned-with-every-answer
			s := seed(t, c.answer, 0)

			w := s.b.labelClearing(t, s.th, s.runs[0], s.keys[0], s.cutOff, c.label)

			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			answer := decodeData[handlers.ThreadReviewResponse](t, w)
			assert.Equal(t, c.want, answer.Nights[1].Clearings[0].Label, "the answer is the thread's review state")
			assert.Equal(t, "to_review", answer.Status, "the first night's commitment chip still awaits review")
			assert.Equal(t, 1, answer.ToReview)
			assert.Equal(t, c.want, label(t, s, 0))
		})
	}

	t.Run("a second label on the same clearing decision replaces the first", func(t *testing.T) {
		// spec: IXN-091.label-recorded
		s := seed(t, true, 0)
		require.Equal(t, http.StatusOK, s.b.labelClearing(t, s.th, s.runs[0], s.keys[0], s.cutOff, true).Code)
		require.Equal(t, http.StatusOK, s.b.labelClearing(t, s.th, s.runs[0], s.keys[0], s.cutOff, false).Code)

		assert.Equal(t, "removed", label(t, s, 0))
		labels, err := s.b.store.LabelsByThread(s.b.ctx, s.th.id)
		require.NoError(t, err)
		require.Len(t, labels.Clearings, 1)
		assert.False(t, labels.Clearings[0].Done)
	})

	t.Run("two methods' clearing decisions of one commitment at one cut-off both carry the label", func(t *testing.T) {
		// spec: IXN-091.shared-by-every-method
		s := seed(t, true, 0, 1)
		require.Equal(t, "none", label(t, s, 1))

		require.Equal(t, http.StatusOK, s.b.labelClearing(t, s.th, s.runs[0], s.keys[0], s.cutOff, true).Code)

		assert.Equal(t, "confirmed", label(t, s, 0))
		assert.Equal(t, "confirmed", label(t, s, 1))
	})

	t.Run("a request that cannot be honoured is refused and records nothing", func(t *testing.T) {
		// spec: IXN-091.invalid-refused
		s := seed(t, true, 0)
		path := "/api/v1/threads/" + s.th.id.String() + "/clearing-labels"
		body := map[string]any{"commitment_message_key": s.keys[0], "cut_off": s.cutOff, "done": true}

		cases := []struct {
			name string
			path string
			body map[string]any
			want int
		}{
			{"a cut-off at which no judgment re-checked the commitment", path + "?run_id=" + s.runs[0].String(),
				map[string]any{"commitment_message_key": s.keys[0], "cut_off": s.cutOff.Add(time.Hour), "done": true}, http.StatusBadRequest},
			{"a message no judgment re-checked", path + "?run_id=" + s.runs[0].String(),
				map[string]any{"commitment_message_key": s.keys[1], "cut_off": s.cutOff, "done": true}, http.StatusBadRequest},
			{"a request without run_id", path, body, http.StatusBadRequest},
			{"an unknown thread", "/api/v1/threads/" + uuid.NewString() + "/clearing-labels?run_id=" + s.runs[0].String(), body, http.StatusNotFound},
			{"an unknown run", path + "?run_id=" + uuid.NewString(), body, http.StatusNotFound},
		}
		for _, c := range cases {
			assert.Equal(t, c.want, s.b.post(t, c.path, c.body).Code, c.name)
		}

		labels, err := s.b.store.LabelsByThread(s.b.ctx, s.th.id)
		require.NoError(t, err)
		assert.Empty(t, labels.Clearings, "nothing recorded")
	})
}
