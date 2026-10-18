package judgment

import (
	"context"
	"fmt"
	"slices"
	"time"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ReviewOutcome records the user's review of an outcome, whatever its state,
// as an assertion with user provenance holding the right outcome, and returns
// the outcome with the verdict derived from it. The review labels the
// outcome's decision for every method: a later judgment of that input leaves
// its question to the user's verdict.
//
// A first review of a decision gives the thread the right outcome's live
// effect when the outcome is applied or a claim: confirming a claim applies
// it, and rejecting an applied addition withdraws it. That effect acts on the
// obligation the outcome's live effect is, followed through the tasks that
// continue it (a superseding commitment, the task a contact merge kept), and
// on the contact the judged contact now is after any merge; a review closes
// no other obligation, whatever opened it. Reviewing an evidence outcome
// records the label only, and so does any review of an outcome whose live
// effect no longer holds (not acting), so a review never reopens a closed
// obligation or touches one that replaced it.
//
// A review of a decision that already has a label, from any proposal on the
// same content, is a re-review: it records the new label and changes nothing
// live. The exception is an Add on a negative decision, a positive right
// outcome for an outcome that stands for no item: it has the live effect the
// same review of an unlabeled negative has, since a negative has no live
// effect to undo.
//
// The review then runs the labeling rule (labelNegativesTx) on the outcome's
// content, in the same transaction, so a review that gives a proposal's last
// unlabeled item its label labels that proposal's negative decisions, or
// records nothing at all.
//
// A review stating an open commitment may keep the obligation's current date:
// the commitment is kept, its judged date is not taken, and the verdict
// records that the judged outcome was wrong. A commitment the review adds,
// which the judgment did not hold, takes the timing of a commitment with no
// stated date in either direction: the contact's cadence watchdog days, else
// seven days.
func (s *Store) ReviewOutcome(ctx context.Context, outcomeID uuid.UUID, review ReviewRequest) (OutcomeView, error) {
	var view OutcomeView
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		q := db.New(tx)
		records, err := loadOutcomes(ctx, q, nil, []uuid.UUID{outcomeID})
		if err != nil {
			return err
		}
		if len(records) == 0 {
			return ErrNotFound
		}
		o := records[0]
		right, keep := review.RightOutcome, review.KeepCurrentDate
		if !isOutcomeValue(o.Question, right) {
			return ErrInvalidRightOutcome
		}
		if keep && !keepsDateApplies(o.Question, right) {
			return ErrInvalidKeepCurrentDate
		}
		live, err := takesEffect(ctx, q, o, right)
		if err != nil {
			return err
		}
		if err := s.recordReviewTx(ctx, tx, o, right, keep); err != nil {
			return err
		}
		if live && o.state() != OutcomeStateEvidence {
			if err := s.settleLandingTx(ctx, tx, o, o.value() == right && !keep); err != nil {
				return err
			}
			if err := s.applyReviewTx(ctx, tx, o, right, keep); err != nil {
				return err
			}
		}
		if _, err := s.labelNegativesTx(ctx, tx, o.Example, uuid.Nil); err != nil {
			return err
		}
		records, err = loadOutcomes(ctx, q, nil, []uuid.UUID{outcomeID})
		if err != nil {
			return err
		}
		view = records[0].view()
		return nil
	})
	return view, err
}

// takesEffect reports whether a review of the outcome stating right is one
// that may change live state: the first review of its decision, or an Add on
// a negative decision. Any other review is a re-review, which records the
// label only.
func takesEffect(ctx context.Context, q db.Querier, o *outcomeRecord, right string) (bool, error) {
	lb, err := loadLabels(ctx, q, []ExampleID{o.Example})
	if err != nil {
		return false, err
	}
	if lb.of(o) == nil {
		return true, nil
	}
	if isNoEffectValue(right) {
		return false, nil
	}
	siblings, err := loadOutcomes(ctx, q, []uuid.UUID{o.JudgmentID}, nil)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(sortOutcomes(siblings, lb).negatives, func(n *outcomeRecord) bool { return n.ID == o.ID }), nil
}

// settleLandingTx closes the outcome's landing as the review decides it: a
// claim is accepted when confirmed and rejected otherwise; an applied landing
// the user does not confirm is retracted.
func (s *Store) settleLandingTx(ctx context.Context, tx pgx.Tx, o *outcomeRecord, confirmed bool) error {
	var err error
	switch {
	case o.class.Status == repository.AssertionStatusProposed && confirmed:
		_, err = s.assert.AcceptTx(ctx, tx, o.class.ID)
	case o.class.Status == repository.AssertionStatusProposed:
		_, err = s.assert.RejectTx(ctx, tx, o.class.ID)
	case o.class.Status == repository.AssertionStatusAccepted && !confirmed:
		_, err = s.assert.RetractTx(ctx, tx, o.class.ID)
	}
	if err != nil {
		return fmt.Errorf("settle landing: %w", err)
	}
	return nil
}

// applyReviewTx gives the thread the right outcome's live effect, unless the
// outcome's own live effect no longer holds, and records the obligation that
// effect opened or found. The only obligation it may close is the one the
// outcome's live effect is. The judgment's other outcomes keep what they hold:
// a reviewed one its right outcome, an applied one its judged outcome.
func (s *Store) applyReviewTx(ctx context.Context, tx pgx.Tx, o *outcomeRecord, right string, keep bool) error {
	q := db.New(tx)
	rows, err := q.ListJudgmentRows(ctx, db.ListJudgmentRowsParams{Ids: []uuid.UUID{o.JudgmentID}, ExampleIds: []string{}})
	if err != nil || len(rows) != 1 {
		return fmt.Errorf("load judgment %s: %w", o.JudgmentID, err)
	}
	j := rows[0]
	live, err := loadLiveState(ctx, q, j.ThreadID)
	if err != nil {
		return err
	}
	gone, err := live.effectGone(ctx, o)
	if err != nil || gone {
		return err
	}
	if changeOf(o) == ChangeKindLaterDate {
		if err := q.SetJudgmentOutcomeKeptDueDate(ctx, db.SetJudgmentOutcomeKeptDueDateParams{
			NodeID: o.ID, KeptDueDate: live.obligationDue(o),
		}); err != nil {
			return fmt.Errorf("record kept due date: %w", err)
		}
	}
	due, err := s.reviewDueTx(ctx, tx, o, right, keep)
	if err != nil {
		return err
	}
	outcomes, err := loadOutcomes(ctx, q, []uuid.UUID{o.JudgmentID}, nil)
	if err != nil {
		return err
	}
	reviewed, err := loadLabels(ctx, q, []ExampleID{o.Example})
	if err != nil {
		return err
	}
	siblings := make([]sibling, 0, len(outcomes))
	for _, x := range outcomes {
		var value string
		switch l := reviewed.of(x); {
		case x.ID == o.ID:
			value = right
		case l != nil:
			value = l.right
		case x.state() == OutcomeStateApplied:
			value = x.value()
		default:
			continue
		}
		siblings = append(siblings, sibling{x.Question, contactOf(x.LiveContactID), value})
	}
	withdraw := ""
	if o.state() == OutcomeStateApplied {
		withdraw = o.value()
	}
	p := &planner{
		s: s, tx: tx, thread: j.ThreadID, cutOff: j.CutOff, wants: wantsOf(siblings),
		closable: onlyObligation(live.obligation(o)), openedNow: map[uuid.UUID]bool{},
	}
	pl, err := p.plan(ctx, o.Question, o.LiveContactID, right, due, withdraw)
	if err != nil {
		return err
	}
	if _, _, err := p.apply(ctx, pl, userLocator(), 100); err != nil {
		return err
	}
	return p.recordObligation(ctx, o.ID, o.Question, o.LiveContactID, right)
}

// reviewDueTx is the date a review gives a commitment: none when the review
// keeps the obligation's current date; the timing of a commitment with no
// stated date (the contact's cadence watchdog days, else seven days) when the
// review adds a commitment the judgment did not hold, in either direction;
// otherwise the judged date.
func (s *Store) reviewDueTx(ctx context.Context, tx pgx.Tx, o *outcomeRecord, right string, keep bool) (*time.Time, error) {
	commitment := o.Question == QuestionCommitmentMine || o.Question == QuestionCommitmentTheirs
	switch {
	case keep:
		return nil, nil
	case commitment && o.adds(right):
		due, err := s.nudgeDateTx(ctx, tx, *o.LiveContactID)
		return &due, err
	default:
		return o.DueDate, nil
	}
}
