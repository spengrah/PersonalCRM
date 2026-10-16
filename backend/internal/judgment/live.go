package judgment

import (
	"context"
	"fmt"
	"time"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
)

// liveState reads a thread's live state as the planner's rules leave it: its
// obligation tasks, open and closed, the contacts the timestamp rule shows
// awaiting a reply, and its raised flags. The bench compiles each item's
// acting state from it, and a review reads it to tell an effect that no
// longer holds.
type liveState struct {
	q      db.Querier
	thread uuid.UUID
	tasks  map[uuid.UUID]*repository.ContactTask
}

func loadLiveState(ctx context.Context, q db.Querier, thread uuid.UUID) (*liveState, error) {
	tasks, err := repository.NewContactTaskRepository(q).ListObligationTasksByThread(ctx, thread)
	if err != nil {
		return nil, fmt.Errorf("list thread obligations: %w", err)
	}
	byID := make(map[uuid.UUID]*repository.ContactTask, len(tasks))
	for i := range tasks {
		byID[tasks[i].ID] = &tasks[i]
	}
	return &liveState{q: q, thread: thread, tasks: byID}, nil
}

// obligation is the task holding the obligation an outcome's live effect is:
// the task the outcome recorded, or the last task continuing its obligation
// (a commitment that superseded the reply, or the task a merge kept). It is
// nil when the outcome's effect is no obligation.
func (l *liveState) obligation(o *outcomeRecord) *repository.ContactTask {
	if o.ObligationTaskID == nil {
		return nil
	}
	t := l.tasks[*o.ObligationTaskID]
	for range len(l.tasks) {
		if t == nil || t.Obligation.ContinuedBy == nil {
			break
		}
		t = l.tasks[*t.Obligation.ContinuedBy]
	}
	return t
}

// obligationDue is the due date of the obligation an outcome's live effect
// is; nil when it has none or its date does not parse.
func (l *liveState) obligationDue(o *outcomeRecord) *time.Time {
	t := l.obligation(o)
	if t == nil {
		return nil
	}
	due, err := time.Parse(dateFormat, dueOf(t))
	// best-effort: a task whose due date does not parse shows no date.
	if err != nil {
		return nil
	}
	return &due
}

// effectGone reports whether the outcome acted on live state and its effect
// no longer holds. The effect is the user's right outcome's once reviewed,
// else the live value a suggested change would take away, else the judged
// outcome's. An owed reply or a commitment holds exactly while the
// obligation the outcome recorded stays open, so another obligation of the
// same party, whatever opened it, never stands in for it; "they owe a reply"
// holds also while the timestamp rule shows the contact awaiting a reply; a
// big moment holds while its flag is raised. Evidence never acted, and
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
	if o.Question == QuestionBigMoment {
		raised, err := flagRaised(ctx, l.q, l.thread, value)
		return !raised, err
	}
	if t := l.obligation(o); t != nil && t.IsLive() {
		return false, nil
	}
	if value != string(BallInCourtTheyOweReply) {
		return true, nil
	}
	waiting, err := awaiting(ctx, l.q, contactOf(o.ContactID))
	return !waiting, err
}
