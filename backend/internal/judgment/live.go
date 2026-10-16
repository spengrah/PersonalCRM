package judgment

import (
	"context"
	"fmt"
	"slices"
	"time"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
)

// liveState reads a thread's live state as the planner's rules leave it: its
// open obligations, the contacts the timestamp rule shows awaiting a reply,
// and its raised flags. The bench compiles each item's acting state from it,
// and a review reads it to tell an effect that no longer holds.
//
// through holds, per judgment read, the cut-off of its latest appearance. An
// obligation is a judgment's effect only when it opened at or before that
// cut-off: every obligation records the cut-off of the judgment that opened
// it, so one opened at a later cut-off is a later judgment's, never this one's.
type liveState struct {
	q       db.Querier
	thread  uuid.UUID
	open    []repository.ContactTask
	through map[uuid.UUID]time.Time
}

func loadLiveState(ctx context.Context, q db.Querier, thread uuid.UUID, judgmentIDs []uuid.UUID) (*liveState, error) {
	tasks, err := repository.NewContactTaskRepository(q).ListObligationTasksByThread(ctx, thread)
	if err != nil {
		return nil, fmt.Errorf("list thread obligations: %w", err)
	}
	open := slices.DeleteFunc(tasks, func(t repository.ContactTask) bool { return !t.IsLive() })
	through, err := latestAppearances(ctx, q, judgmentIDs)
	if err != nil {
		return nil, err
	}
	return &liveState{q: q, thread: thread, open: open, through: through}, nil
}

// latestAppearances returns the cut-off of each judgment's latest appearance.
func latestAppearances(ctx context.Context, q db.Querier, judgmentIDs []uuid.UUID) (map[uuid.UUID]time.Time, error) {
	through := make(map[uuid.UUID]time.Time, len(judgmentIDs))
	if len(judgmentIDs) == 0 {
		return through, nil
	}
	rows, err := q.ListJudgmentAppearances(ctx, db.ListJudgmentAppearancesParams{JudgmentIds: judgmentIDs})
	if err != nil {
		return nil, fmt.Errorf("list appearances: %w", err)
	}
	for _, r := range rows {
		if r.CutOff.After(through[r.JudgmentID]) {
			through[r.JudgmentID] = r.CutOff
		}
	}
	return through, nil
}

// holds reports whether value's live effect for the question and contact
// holds on the thread as the effect of a judgment whose latest appearance is
// at through. A party's reply and commitment obligations on a thread are one
// obligation, the commitment superseding the reply, so an owed reply or an
// open commitment holds while the debtor's obligation that opened at or before
// through is open; "they owe a reply" holds also while the timestamp rule
// shows the contact awaiting a reply; a big moment holds while its flag is
// raised.
func (l *liveState) holds(ctx context.Context, question Question, contact uuid.UUID, value string, through time.Time) (bool, error) {
	has := func(debtor string) bool {
		t := liveObligation(l.open, debtor, contact)
		return t != nil && !t.Obligation.CutOff.After(through)
	}
	switch {
	case isNoEffectValue(value):
		return false, nil
	case question == QuestionBigMoment:
		return flagRaised(ctx, l.q, l.thread, value)
	case question == QuestionCommitmentMine, value == string(BallInCourtIOweReply):
		return has(repository.ObligationDebtorUser), nil
	case question == QuestionCommitmentTheirs:
		return has(repository.ObligationDebtorContact), nil
	default:
		if has(repository.ObligationDebtorContact) {
			return true, nil
		}
		return awaiting(ctx, l.q, contact)
	}
}

// effectGone reports whether the outcome acted on live state and its effect
// no longer holds: its obligation's task was completed or dismissed, a later
// message closed the reply, or its flag was withdrawn. An obligation a later
// judgment opened is not the outcome's effect. The effect is the user's right
// outcome's once reviewed, else the live value a suggested change would take
// away, else the judged outcome's. Evidence never acted, and neither did a
// claim suggesting an addition, which awaits the user's OK.
func (l *liveState) effectGone(ctx context.Context, o *outcomeRecord) (bool, error) {
	if o.state() == OutcomeStateEvidence {
		return false, nil
	}
	value := o.actingValue()
	if o.review == nil && o.kept() == "" && o.state() == OutcomeStateClaim {
		return false, nil
	}
	if value == "" || isNoEffectValue(value) {
		return false, nil
	}
	holds, err := l.holds(ctx, o.Question, contactOf(o.ContactID), value, l.through[o.JudgmentID])
	return !holds, err
}
