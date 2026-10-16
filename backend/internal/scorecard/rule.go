package scorecard

import (
	"context"
	"errors"
	"time"

	"personal-crm/backend/internal/cadence"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/judgment"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
)

// theyOweReply is the one outcome the timestamp rule is a reference for: it
// never produces "I owe".
var theyOweReply = OutcomeKey(judgment.QuestionBallInCourt, string(judgment.BallInCourtTheyOweReply))

// timestampRule is the awaiting-reply timestamp rule (CAD-040) evaluated at
// an example's cut-off: a contact owes the user a reply when its
// last_outreach_at, across all its interactions up to the cut-off, is
// strictly later than its last_response_at and the cut-off falls within the
// waiting period its cadence gives. A contact without a cadence has no
// waiting period, so it is never awaited.
type timestampRule struct {
	contacts *repository.ContactRepository
	watchdog config.WatchdogConfig
}

// reference scores the rule on they_owe_reply against the user's
// ball-in-court reviews of the examples. The rule answers they_owe_reply or
// nothing.
func (r *timestampRule) reference(ctx context.Context, examples []judgment.Example) (OutcomeMetrics, error) {
	var labels []label
	for _, ex := range examples {
		for _, rv := range ex.Reviews {
			if judgment.Question(rv.Question) != judgment.QuestionBallInCourt || rv.ContactID == nil {
				continue
			}
			awaited, err := r.awaits(ctx, *rv.ContactID, ex.CutOff)
			if err != nil {
				return OutcomeMetrics{}, failed(Failure{Reason: FailureReference, Example: ex.ID}, err)
			}
			judged := Unanswered
			if awaited {
				judged = string(judgment.BallInCourtTheyOweReply)
			}
			labels = append(labels, label{
				example: ex.ID,
				key:     answerKey{judgment.QuestionBallInCourt, *rv.ContactID},
				right:   rv.RightOutcome,
				judged:  judged,
			})
		}
	}
	return score(uuid.Nil, judgment.MethodRef{}, labels).PerOutcome[theyOweReply], nil
}

// awaits reports whether the rule shows the contact owing the user a reply at
// the cut-off.
func (r *timestampRule) awaits(ctx context.Context, contactID uuid.UUID, cutOff time.Time) (bool, error) {
	lastOutreach, lastResponse, err := r.contacts.InteractionTimestampsAt(ctx, contactID, cutOff)
	if err != nil {
		return false, err
	}
	if lastOutreach == nil {
		return false, nil
	}
	days, err := r.watchdogDays(ctx, contactID)
	if err != nil {
		return false, err
	}
	// The cadence updater sets no waiting period when the window is zero.
	var until *time.Time
	if days > 0 {
		u := cadence.AwaitingReplyUntil(*lastOutreach, days)
		until = &u
	}
	return cadence.IsAwaitingReply(lastOutreach, lastResponse, until, cutOff), nil
}

// watchdogDays is the waiting period the contact's cadence gives; zero for a
// contact without a cadence or no longer live.
func (r *timestampRule) watchdogDays(ctx context.Context, contactID uuid.UUID) (int, error) {
	contact, err := r.contacts.GetContact(ctx, contactID)
	if errors.Is(err, db.ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if contact.Cadence == nil {
		return 0, nil
	}
	return r.watchdog.DaysForCadence(*contact.Cadence), nil
}
