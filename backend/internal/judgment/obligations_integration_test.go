//go:build integration_testdb

package judgment_test

import (
	"testing"
	"time"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/contacttask"
	"personal-crm/backend/internal/judgment"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Each open obligation is keyed by thread and owing party and has exactly one
// task: a commitment supersedes a reply for the same party, several
// commitments of one party are one obligation at the earliest date, and the
// user's obligation to several participants belongs to the most confidently
// owed one.
// spec: CAD-054.one-open-per-thread-and-party, CAD-054.kind-by-what-completion-records, CAD-054.user-obligation-on-most-confident-contact, CAD-055.commitment-supersedes-reply, CAD-055.earliest-known-date
func TestOneObligationPerThreadAndOwingParty(t *testing.T) {
	type want struct {
		debtor, basis, kind string
		contact             func(cs []uuid.UUID) uuid.UUID
		due                 func(e *env) time.Time
	}
	cases := []struct {
		name     string
		contacts int
		outcomes func(e *env, cs []uuid.UUID) judgment.Outcomes
		want     want
	}{
		{
			name:     "the user's reply that states a commitment is one obligation with one reminder task",
			contacts: 1,
			outcomes: func(e *env, cs []uuid.UUID) judgment.Outcomes {
				return judgment.Outcomes{
					BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(cs[0], judgment.BallInCourtIOweReply, 0.9)},
					Commitments: []judgment.CommitmentOutcome{commitment(cs[0], judgment.CommitmentDirectionMine, judgment.CommitmentStateOpen, 0.8, nil)},
				}
			},
			want: want{repository.ObligationDebtorUser, repository.ObligationBasisCommitment, contacttask.KindReminder,
				func(cs []uuid.UUID) uuid.UUID { return cs[0] }, func(*env) time.Time { return todayDate() }},
		},
		{
			name:     "two commitments of one party with different dates are one obligation at the earlier date",
			contacts: 1,
			outcomes: func(e *env, cs []uuid.UUID) judgment.Outcomes {
				return judgment.Outcomes{Commitments: []judgment.CommitmentOutcome{
					commitment(cs[0], judgment.CommitmentDirectionMine, judgment.CommitmentStateOpen, 0.9, date(e.day(120))),
					commitment(cs[0], judgment.CommitmentDirectionMine, judgment.CommitmentStateOpen, 0.6, date(e.day(110))),
				}}
			},
			want: want{repository.ObligationDebtorUser, repository.ObligationBasisCommitment, contacttask.KindReminder,
				func(cs []uuid.UUID) uuid.UUID { return cs[0] }, func(e *env) time.Time { return *date(e.day(110)) }},
		},
		{
			name:     "i_owe_reply toward two participants is one obligation on the more confidently owed contact",
			contacts: 2,
			outcomes: func(e *env, cs []uuid.UUID) judgment.Outcomes {
				return judgment.Outcomes{BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{
					bic(cs[0], judgment.BallInCourtIOweReply, 0.6),
					bic(cs[1], judgment.BallInCourtIOweReply, 0.9),
				}}
			},
			want: want{repository.ObligationDebtorUser, repository.ObligationBasisReply, contacttask.KindReachOut,
				func(cs []uuid.UUID) uuid.UUID { return cs[1] }, func(*env) time.Time { return todayDate() }},
		},
		{
			name:     "i_owe_reply toward two participants whose confidences differ within one percent goes to the more confident",
			contacts: 2,
			outcomes: func(e *env, cs []uuid.UUID) judgment.Outcomes {
				// Question IDs order the less confident outcome first, so only
				// the confidence itself can pick the right contact.
				return judgment.Outcomes{BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{
					{QuestionID: "ball_in_court:a", ContactID: cs[0], Outcome: judgment.BallInCourtIOweReply, Confidence: 0.901},
					{QuestionID: "ball_in_court:b", ContactID: cs[1], Outcome: judgment.BallInCourtIOweReply, Confidence: 0.909},
				}}
			},
			want: want{repository.ObligationDebtorUser, repository.ObligationBasisReply, contacttask.KindReachOut,
				func(cs []uuid.UUID) uuid.UUID { return cs[1] }, func(*env) time.Time { return todayDate() }},
		},
		{
			name:     "a participant owing a reply and a commitment is one obligation with one reach_out task",
			contacts: 1,
			outcomes: func(e *env, cs []uuid.UUID) judgment.Outcomes {
				return judgment.Outcomes{
					BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(cs[0], judgment.BallInCourtTheyOweReply, 0.9)},
					Commitments: []judgment.CommitmentOutcome{commitment(cs[0], judgment.CommitmentDirectionTheirs, judgment.CommitmentStateOpen, 0.9, date(e.day(100)))},
				}
			},
			want: want{repository.ObligationDebtorContact, repository.ObligationBasisCommitment, contacttask.KindReachOut,
				func(cs []uuid.UUID) uuid.UUID { return cs[0] }, func(e *env) time.Time { return *date(e.day(100)) }},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, production)
			th := e.thread(t)
			cs := make([]uuid.UUID, tc.contacts)
			for i := range cs {
				cs[i] = e.contact(t)
				e.message(t, th, cs[i], false, e.day(1))
			}

			_, effects := e.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, e.day(1), "h1", tc.outcomes(e, cs)))

			assert.Equal(t, judgment.Effects{ObligationsOpened: 1}, effects)
			all, err := e.store.Obligations(e.ctx, th.id)
			require.NoError(t, err)
			require.Len(t, all, 1, "one obligation, one task, no superseded task")
			o := all[0]
			assert.True(t, o.Open)
			assert.Equal(t, tc.want.debtor, o.Debtor)
			assert.Equal(t, tc.want.basis, o.Basis)
			assert.Equal(t, tc.want.kind, o.Kind)
			assert.Equal(t, tc.want.contact(cs), o.ContactID)
			assert.Equal(t, tc.want.due(e), o.DueDate)
		})
	}
}

// An obligation a participant owes is due on the commitment's selected date,
// else the contact's cadence watchdog days after it opens, else seven days
// after; one the user owes is due on the selected date, else the day it opens.
// spec: CAD-056
func TestObligationTaskTiming(t *testing.T) {
	weeklyDays := config.TestConfig().Watchdog.DaysForCadence("weekly")
	require.Positive(t, weeklyDays)
	cases := []struct {
		name    string
		cadence []string
		outcome func(e *env, c uuid.UUID) judgment.Outcomes
		due     func(e *env) time.Time
	}{
		{
			name: "a they-owe commitment with a selected date is due on it",
			outcome: func(e *env, c uuid.UUID) judgment.Outcomes {
				return judgment.Outcomes{Commitments: []judgment.CommitmentOutcome{commitment(c, judgment.CommitmentDirectionTheirs, judgment.CommitmentStateOpen, 0.9, date(e.day(95)))}}
			},
			due: func(e *env) time.Time { return *date(e.day(95)) },
		},
		{
			name:    "a they-owe commitment without a date is due the contact's watchdog days out",
			cadence: []string{"weekly"},
			outcome: func(e *env, c uuid.UUID) judgment.Outcomes {
				return judgment.Outcomes{Commitments: []judgment.CommitmentOutcome{commitment(c, judgment.CommitmentDirectionTheirs, judgment.CommitmentStateOpen, 0.9, nil)}}
			},
			due: func(*env) time.Time { return todayDate().AddDate(0, 0, weeklyDays) },
		},
		{
			name: "a they-owe commitment without a date, for a contact without a cadence, is due seven days out",
			outcome: func(e *env, c uuid.UUID) judgment.Outcomes {
				return judgment.Outcomes{Commitments: []judgment.CommitmentOutcome{commitment(c, judgment.CommitmentDirectionTheirs, judgment.CommitmentStateOpen, 0.9, nil)}}
			},
			due: func(*env) time.Time { return todayDate().AddDate(0, 0, 7) },
		},
		{
			name: "an I-owe commitment without a date is due the day it opens",
			outcome: func(e *env, c uuid.UUID) judgment.Outcomes {
				return judgment.Outcomes{Commitments: []judgment.CommitmentOutcome{commitment(c, judgment.CommitmentDirectionMine, judgment.CommitmentStateOpen, 0.9, nil)}}
			},
			due: func(*env) time.Time { return todayDate() },
		},
		{
			name: "an i_owe_reply obligation is due the day it opens",
			outcome: func(e *env, c uuid.UUID) judgment.Outcomes {
				return judgment.Outcomes{BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(c, judgment.BallInCourtIOweReply, 0.9)}}
			},
			due: func(*env) time.Time { return todayDate() },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, production)
			c, th := e.contact(t, tc.cadence...), e.thread(t)
			e.message(t, th, c, false, e.day(1))

			e.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, e.day(1), "h1", tc.outcome(e, c)))

			open := e.openObligations(t, th)
			require.Len(t, open, 1)
			assert.Equal(t, tc.due(e), open[0].DueDate)
		})
	}
}

// A commitment that supersedes an open reply obligation is due on its
// selected date. A selected date later than the reply's delays the
// obligation, so it is a claim that keeps the reply obligation open until the
// user confirms it.
// spec: CAD-055.upgrade-takes-selected-date, KNW-044.removal-always-claim
func TestCommitmentUpgradesAnOpenReplyObligation(t *testing.T) {
	t.Run("a later selected date is a claim, and confirming it moves the obligation to that date", func(t *testing.T) {
		e := newEnv(t, production)
		c, th := e.contact(t), e.thread(t)
		e.message(t, th, c, false, e.day(1))
		e.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, e.day(1), "h1",
			judgment.Outcomes{BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(c, judgment.BallInCourtIOweReply, 0.9)}}))
		before := e.openObligations(t, th)
		require.Len(t, before, 1)
		require.Equal(t, todayDate(), before[0].DueDate)

		e.message(t, th, c, false, e.day(2))
		selected := date(e.day(120))
		_, effects := e.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, e.day(2), "h2",
			judgment.Outcomes{Commitments: []judgment.CommitmentOutcome{commitment(c, judgment.CommitmentDirectionMine, judgment.CommitmentStateOpen, 0.9, selected)}}))

		assert.Equal(t, judgment.Effects{ClaimsRaised: 1}, effects)
		assert.Equal(t, before, e.openObligations(t, th), "the reply obligation stays as it was")
		claim := e.outcome(t, th, prodMethod, judgment.QuestionCommitmentMine, &c)
		require.Equal(t, judgment.OutcomeStateClaim, claim.State)

		_, err := e.store.ReviewOutcome(e.ctx, claim.ID, judgment.ReviewRequest{RightOutcome: string(judgment.CommitmentStateOpen)})
		require.NoError(t, err)

		open := e.openObligations(t, th)
		require.Len(t, open, 1)
		assert.Equal(t, repository.ObligationBasisCommitment, open[0].Basis)
		assert.Equal(t, contacttask.KindReminder, open[0].Kind)
		assert.Equal(t, *selected, open[0].DueDate)
	})

	t.Run("an earlier selected date applies at once on that date", func(t *testing.T) {
		e := newEnv(t, production)
		c, th := e.contact(t), e.thread(t)
		e.message(t, th, c, false, e.day(1))
		e.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, e.day(1), "h1",
			judgment.Outcomes{BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(c, judgment.BallInCourtTheyOweReply, 0.9)}}))
		before := e.openObligations(t, th)
		require.Len(t, before, 1)
		require.Equal(t, todayDate().AddDate(0, 0, 7), before[0].DueDate)

		e.message(t, th, c, false, e.day(2))
		selected := date(e.day(93))
		_, effects := e.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, e.day(2), "h2",
			judgment.Outcomes{Commitments: []judgment.CommitmentOutcome{commitment(c, judgment.CommitmentDirectionTheirs, judgment.CommitmentStateOpen, 0.9, selected)}}))

		assert.Equal(t, judgment.Effects{ObligationsOpened: 1}, effects)
		open := e.openObligations(t, th)
		require.Len(t, open, 1)
		assert.Equal(t, repository.ObligationBasisCommitment, open[0].Basis)
		assert.Equal(t, *selected, open[0].DueDate)
		assert.Equal(t, judgment.OutcomeStateApplied, e.outcome(t, th, prodMethod, judgment.QuestionCommitmentTheirs, &c).State)
	})
}
