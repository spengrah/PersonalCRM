package judgment

import (
	"context"
	"fmt"
	"time"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ReviewOutcome records the user's review of an outcome, whatever its state,
// as an assertion with user provenance holding the right outcome, and returns
// the outcome with the verdict derived from it. Reviewing an applied or claim
// outcome gives the thread the right outcome's live effect: confirming a claim
// applies it, and rejecting an applied addition withdraws it. That effect acts
// on the obligation the outcome's live effect is, followed through the tasks
// that continue it (a superseding commitment, the task a contact merge kept),
// and on the contact the judged contact now is after any merge; a review
// closes no other obligation, whatever opened it. Reviewing an evidence
// outcome records the label only, and so does any review of an outcome whose
// live effect no longer holds (not acting), so a review never reopens a closed
// obligation or touches one that replaced it. The review labels the outcome's
// input for every method: a later judgment of that input leaves its question
// to the user's verdict.
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
		locator := userLocator()
		if _, err := s.assert.AssertTx(ctx, tx, service.AssertRequest{
			SubjectNodeID: o.ID,
			PredicateKey:  predicateReview,
			ValueText:     &right,
			Confidence:    100,
			Locators:      []service.ProvenanceLocator{locator},
		}); err != nil {
			return fmt.Errorf("assert review: %w", err)
		}
		if _, err := s.assert.AssertTx(ctx, tx, service.AssertRequest{
			SubjectNodeID: o.ID,
			PredicateKey:  predicateReviewKeepsDate,
			ValueBool:     &keep,
			Confidence:    100,
			Locators:      []service.ProvenanceLocator{locator},
		}); err != nil {
			return fmt.Errorf("assert review keeps date: %w", err)
		}
		if o.state() != OutcomeStateEvidence {
			if err := s.settleLandingTx(ctx, tx, o, o.value() == right && !keep); err != nil {
				return err
			}
			if err := s.applyReviewTx(ctx, tx, o, right, keep); err != nil {
				return err
			}
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
	due, err := s.reviewDueTx(ctx, tx, o, right, keep)
	if err != nil {
		return err
	}
	outcomes, err := loadOutcomes(ctx, q, []uuid.UUID{o.JudgmentID}, nil)
	if err != nil {
		return err
	}
	reviewed, err := exampleReviews(ctx, q, j.ExampleID)
	if err != nil {
		return err
	}
	siblings := make([]sibling, 0, len(outcomes))
	for _, x := range outcomes {
		value, ok := reviewed[x.key()]
		switch {
		case x.ID == o.ID:
			value = right
		case ok:
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
