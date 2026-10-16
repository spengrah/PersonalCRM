package scorecard

import (
	"context"
	"errors"
	"fmt"
	"time"

	"personal-crm/backend/internal/cadence"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/judgment"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/threadctx"

	"github.com/google/uuid"
)

// theyOweReply is the one outcome the timestamp rule is a reference for: it
// never produces "I owe".
var theyOweReply = OutcomeKey(judgment.QuestionBallInCourt, string(judgment.BallInCourtTheyOweReply))

// timestampRule is the awaiting-reply timestamp rule (CAD-040) applied to a
// reviewed thread at the example's cut-off: a participant owes the user a
// reply when the user's last message in the thread is strictly later than
// the participant's last message there, and the cut-off falls within the
// waiting period the participant's cadence gives. A contact without a
// cadence has no waiting period, so it is never awaited.
type timestampRule struct {
	contexts *threadctx.Builder
	contacts *repository.ContactRepository
	watchdog config.WatchdogConfig
}

// reference scores the rule on they_owe_reply against the user's
// ball-in-court reviews of the examples. The rule answers they_owe_reply or
// nothing.
func (r *timestampRule) reference(ctx context.Context, examples []judgment.Example) (OutcomeMetrics, error) {
	var labels []label
	for _, ex := range examples {
		var c *threadctx.Context
		for _, rv := range ex.Reviews {
			if judgment.Question(rv.Question) != judgment.QuestionBallInCourt || rv.ContactID == nil {
				continue
			}
			if c == nil {
				built, err := r.contexts.Build(ctx, ex.Thread, ex.CutOff)
				if err != nil {
					return OutcomeMetrics{}, fmt.Errorf("timestamp rule: build example %s: %w", ex.ID, err)
				}
				c = &built
			}
			awaited, err := r.awaits(ctx, *c, *rv.ContactID)
			if err != nil {
				return OutcomeMetrics{}, err
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

// awaits reports whether the rule shows the contact owing the user a reply on
// the thread at the context's cut-off.
func (r *timestampRule) awaits(ctx context.Context, c threadctx.Context, contactID uuid.UUID) (bool, error) {
	var lastOutreach, lastResponse *time.Time
	for _, e := range c.Entries {
		p := c.Participants[e.Role]
		switch {
		case p.IsUser:
			lastOutreach = later(lastOutreach, e.SentAt)
		case p.ContactID != nil && *p.ContactID == contactID:
			lastResponse = later(lastResponse, e.SentAt)
		}
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
	return cadence.IsAwaitingReply(lastOutreach, lastResponse, until, c.CutOff), nil
}

// watchdogDays is the waiting period the contact's cadence gives; zero for a
// contact without a cadence or no longer live.
func (r *timestampRule) watchdogDays(ctx context.Context, contactID uuid.UUID) (int, error) {
	contact, err := r.contacts.GetContact(ctx, contactID)
	if errors.Is(err, db.ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("timestamp rule: get contact: %w", err)
	}
	if contact.Cadence == nil {
		return 0, nil
	}
	return r.watchdog.DaysForCadence(*contact.Cadence), nil
}

func later(t *time.Time, u time.Time) *time.Time {
	if t == nil || u.After(*t) {
		return &u
	}
	return t
}
