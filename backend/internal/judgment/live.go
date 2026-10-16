package judgment

import (
	"context"
	"fmt"
	"slices"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
)

// liveState reads a thread's live state as the planner's rules leave it: its
// open obligations, the contacts the timestamp rule shows awaiting a reply,
// and its raised flags. The bench compiles each item's acting state from it,
// and a review reads it to tell an effect that no longer holds.
type liveState struct {
	q      db.Querier
	thread uuid.UUID
	open   []repository.ContactTask
}

func loadLiveState(ctx context.Context, q db.Querier, thread uuid.UUID) (*liveState, error) {
	tasks, err := repository.NewContactTaskRepository(q).ListObligationTasksByThread(ctx, thread)
	if err != nil {
		return nil, fmt.Errorf("list thread obligations: %w", err)
	}
	open := slices.DeleteFunc(tasks, func(t repository.ContactTask) bool { return !t.IsLive() })
	return &liveState{q: q, thread: thread, open: open}, nil
}

// holds reports whether value's live effect for the question and contact
// holds on the thread. A party's reply and commitment obligations on a thread
// are one obligation, the commitment superseding the reply, so an owed reply
// or an open commitment holds while the debtor's obligation is open; "they owe
// a reply" holds also while the timestamp rule shows the contact awaiting a
// reply; a big moment holds while its flag is raised.
func (l *liveState) holds(ctx context.Context, question Question, contact uuid.UUID, value string) (bool, error) {
	switch {
	case isNoEffectValue(value):
		return false, nil
	case question == QuestionBigMoment:
		return flagRaised(ctx, l.q, l.thread, value)
	case question == QuestionCommitmentMine, value == string(BallInCourtIOweReply):
		return liveObligation(l.open, repository.ObligationDebtorUser, contact) != nil, nil
	case question == QuestionCommitmentTheirs:
		return liveObligation(l.open, repository.ObligationDebtorContact, contact) != nil, nil
	default:
		if liveObligation(l.open, repository.ObligationDebtorContact, contact) != nil {
			return true, nil
		}
		return awaiting(ctx, l.q, contact)
	}
}

// effectGone reports whether the outcome acted on live state and its effect
// no longer holds: its obligation's task was completed or dismissed, a later
// message closed the reply, or its flag was withdrawn. The effect is the
// user's right outcome's once reviewed, else the live value a suggested change
// would take away, else the judged outcome's. Evidence never acted, and
// neither did a claim suggesting an addition, which awaits the user's OK.
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
	holds, err := l.holds(ctx, o.Question, contactOf(o.ContactID), value)
	return !holds, err
}
