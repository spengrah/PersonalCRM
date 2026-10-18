package judgment

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/service"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrConfirmNonesRefused is returned for a Confirm Nones on a proposal that
// has an item, or that holds no negative decision without a label.
var ErrConfirmNonesRefused = errors.New("confirm nones applies only to a proposal with no items and an unlabeled negative decision")

// decisionKey names one decision: one question answered for one contact (nil
// for the big moment) on one judged content. Every method's outcome on that
// content answers the same decision.
type decisionKey struct {
	example  ExampleID
	question Question
	contact  uuid.UUID
}

// label is a decision's label: the user's latest review of it, from whichever
// outcome it was made on.
type label struct {
	right     string
	keepsDate bool
}

// labels are decisions' labels.
type labels map[decisionKey]label

// of returns the label of the decision an outcome answers, nil when it has
// none.
func (lb labels) of(o *outcomeRecord) *label {
	l, ok := lb[o.decision()]
	if !ok {
		return nil
	}
	return &l
}

// loadLabels reads the labels of the decisions on the given examples, through
// the one definition of a decision and its label the scorecard reads.
func loadLabels(ctx context.Context, q db.Querier, examples []ExampleID) (labels, error) {
	lb := labels{}
	if len(examples) == 0 {
		return lb, nil
	}
	ids := make([]string, len(examples))
	for i, e := range examples {
		ids[i] = string(e)
	}
	rows, err := q.ListLatestExampleReviews(ctx, db.ListLatestExampleReviewsParams{
		ReviewPredicate: predicateReview, KeepsDatePredicate: predicateReviewKeepsDate, ExampleIds: ids,
	})
	if err != nil {
		return nil, fmt.Errorf("list labels: %w", err)
	}
	for _, r := range rows {
		if r.RightOutcome != nil {
			lb[decisionKey{ExampleID(r.ExampleID), Question(r.Question), contactOf(r.ContactID)}] = label{*r.RightOutcome, r.KeepsDate}
		}
	}
	return lb, nil
}

// recordReviewTx records a review of the outcome's decision: the right
// outcome and whether it keeps an open commitment's current date, as
// assertions with user provenance. The outcome's earlier review, if any, is
// retracted first, so each review is a new assertion and the latest review of
// a decision is its label whichever outcomes earlier reviews were made on. It
// is the one writer of reviews.
func (s *Store) recordReviewTx(ctx context.Context, tx pgx.Tx, o *outcomeRecord, right string, keep bool) error {
	for _, earlier := range []*db.Assertion{o.review, o.keepsDate} {
		if earlier == nil {
			continue
		}
		if _, err := s.assert.RetractTx(ctx, tx, earlier.ID); err != nil {
			return fmt.Errorf("retract earlier review: %w", err)
		}
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
	return nil
}

// labelNegativesTx is the one rule by which a negative decision gains a label
// no user act states, and every path that records a review runs it before its
// transaction commits. In the proposal confirm names (Confirm Nones; uuid.Nil
// for none), which must have no items and a negative decision without a
// label, every negative decision without a label is labeled with its judged
// value. Then, in every proposal on the same content with at least one item
// whose items all have labels (inference), every negative decision without a
// label is labeled with its judged value, until no proposal on the content
// changes, since a label shared across proposals can give another proposal's
// last item its label. A label that exists is never replaced, and a proposal
// with no items gains no label by inference. Each label is an ordinary review
// and changes nothing live. It returns how many labels it recorded.
func (s *Store) labelNegativesTx(ctx context.Context, tx pgx.Tx, example ExampleID, confirm uuid.UUID) (int, error) {
	q := db.New(tx)
	rows, err := q.ListJudgmentRows(ctx, db.ListJudgmentRowsParams{Ids: []uuid.UUID{}, ExampleIds: []string{string(example)}})
	if err != nil {
		return 0, fmt.Errorf("list content judgments: %w", err)
	}
	outcomes, err := loadOutcomes(ctx, q, judgmentIDs(rows), nil)
	if err != nil {
		return 0, err
	}
	byJudgment := map[uuid.UUID][]*outcomeRecord{}
	for _, o := range outcomes {
		byJudgment[o.JudgmentID] = append(byJudgment[o.JudgmentID], o)
	}
	lb, err := loadLabels(ctx, q, []ExampleID{example})
	if err != nil {
		return 0, err
	}
	recorded := 0
	labelAll := func(negatives []*outcomeRecord) error {
		for _, o := range negatives {
			if err := s.recordReviewTx(ctx, tx, o, o.value(), false); err != nil {
				return err
			}
			lb[o.decision()] = label{right: o.value()}
			recorded++
		}
		return nil
	}
	if confirm != uuid.Nil {
		if !slices.ContainsFunc(rows, func(j *db.ListJudgmentRowsRow) bool { return j.ID == confirm }) {
			return 0, ErrNotFound
		}
		sorted := sortOutcomes(byJudgment[confirm], lb)
		pending := sorted.unlabeledNegatives(lb)
		if len(sorted.items) > 0 || len(pending) == 0 {
			return 0, ErrConfirmNonesRefused
		}
		if err := labelAll(pending); err != nil {
			return 0, err
		}
	}
	for changed := true; changed; {
		changed = false
		for _, j := range rows {
			sorted := sortOutcomes(byJudgment[j.ID], lb)
			if len(sorted.items) == 0 || slices.ContainsFunc(sorted.items, func(it placement) bool { return it.label == nil }) {
				continue
			}
			pending := sorted.unlabeledNegatives(lb)
			if err := labelAll(pending); err != nil {
				return 0, err
			}
			changed = changed || len(pending) > 0
		}
	}
	return recorded, nil
}

// ConfirmNones labels every negative decision of the judgment's proposal that
// has no label from any proposal, each with its judged value, and then
// whatever inference those labels complete on the same content, all in one
// transaction. It refuses a proposal with an item, or one holding no negative
// decision without a label, with ErrConfirmNonesRefused, and an unknown
// judgment with ErrNotFound.
func (s *Store) ConfirmNones(ctx context.Context, judgmentID uuid.UUID) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		rows, err := db.New(tx).ListJudgmentRows(ctx, db.ListJudgmentRowsParams{Ids: []uuid.UUID{judgmentID}, ExampleIds: []string{}})
		if err != nil {
			return fmt.Errorf("load judgment %s: %w", judgmentID, err)
		}
		if len(rows) == 0 {
			return ErrNotFound
		}
		_, err = s.labelNegativesTx(ctx, tx, ExampleID(rows[0].ExampleID), judgmentID)
		return err
	})
}

// InferNegativeLabels runs the labeling rule once over every content the user
// has labeled, each content in its own transaction, so a proposal whose items
// all had labels before the rule existed holds no unlabeled negative decision.
// Only content with a label can hold such a proposal. It never replaces a
// label and labels nothing in a proposal with no items; a second run records
// nothing. It returns how many labels it recorded.
func (s *Store) InferNegativeLabels(ctx context.Context) (int, error) {
	rows, err := s.queries.ListLatestExampleReviews(ctx, db.ListLatestExampleReviewsParams{
		ReviewPredicate: predicateReview, KeepsDatePredicate: predicateReviewKeepsDate, ExampleIds: []string{},
	})
	if err != nil {
		return 0, fmt.Errorf("list labeled content: %w", err)
	}
	var examples []ExampleID
	for _, r := range rows {
		if e := ExampleID(r.ExampleID); len(examples) == 0 || examples[len(examples)-1] != e {
			examples = append(examples, e)
		}
	}
	total := 0
	for _, e := range examples {
		var n int
		if err := s.inTx(ctx, func(tx pgx.Tx) (err error) {
			n, err = s.labelNegativesTx(ctx, tx, e, uuid.Nil)
			return err
		}); err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}
