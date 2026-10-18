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
// has an item, or that holds no negative decision the labeling rule may label.
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
// no user act states. The rule labels only a negative decision without a
// label that it does not spare: it spares a decision some proposal on the
// same content holds as an item without a label, which awaits the user's
// review of the item in every proposal on the content (G4), and one some
// proposal holds as a reply its own commitment absorbs, a positive decision
// (A1). With confirm set (Confirm Nones), the one judgment given must have no
// items and such a negative decision, and every such negative decision of its
// proposal is labeled with its judged value. Otherwise (inference, on a close
// of the row the judgments sit on), every such negative decision of each
// given proposal that holds a label is labeled with its judged value, until
// no given proposal changes, since a label shared across proposals on the
// same content can give another proposal its first label. A label that exists
// is never replaced, and a proposal holding no label gains none by inference.
// Each label is an ordinary review and changes nothing live. A review of an
// item never runs the rule. It returns how many labels it recorded, or
// ErrNotFound when a judgment is unknown.
func (s *Store) labelNegativesTx(ctx context.Context, tx pgx.Tx, judgments []uuid.UUID, confirm bool) (int, error) {
	// An empty ID list reads every judgment, so no judgment is no proposal.
	if len(judgments) == 0 {
		return 0, nil
	}
	q := db.New(tx)
	rows, err := q.ListJudgmentRows(ctx, db.ListJudgmentRowsParams{Ids: judgments, ExampleIds: []string{}})
	if err != nil {
		return 0, fmt.Errorf("list judgments: %w", err)
	}
	for _, id := range judgments {
		if !slices.ContainsFunc(rows, func(j *db.ListJudgmentRowsRow) bool { return j.ID == id }) {
			return 0, ErrNotFound
		}
	}
	byJudgment, lb, err := loadContent(ctx, q, rows)
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
	if confirm {
		if len(rows) != 1 {
			return 0, ErrConfirmNonesRefused
		}
		pending := sortOutcomes(byJudgment[rows[0].ID], lb).confirmNones(lb, spared(byJudgment, lb))
		if len(pending) == 0 {
			return 0, ErrConfirmNonesRefused
		}
		err := labelAll(pending)
		return recorded, err
	}
	for changed := true; changed; {
		changed = false
		sp := spared(byJudgment, lb)
		for _, j := range rows {
			sorted := sortOutcomes(byJudgment[j.ID], lb)
			if !sorted.holdsLabel(lb) {
				continue
			}
			pending := sorted.unlabeledNegatives(lb, sp.spares)
			if err := labelAll(pending); err != nil {
				return 0, err
			}
			changed = changed || len(pending) > 0
		}
	}
	return recorded, nil
}

// ConfirmNones labels every negative decision of the judgment's proposal that
// has no label from any proposal and that no proposal on the same content
// holds as an item without a label, or as a reply its own commitment absorbs,
// each with its judged value, in one transaction. It refuses a proposal with an item, or one holding no such
// negative decision, with ErrConfirmNonesRefused, and an unknown judgment
// with ErrNotFound.
func (s *Store) ConfirmNones(ctx context.Context, judgmentID uuid.UUID) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		_, err := s.labelNegativesTx(ctx, tx, []uuid.UUID{judgmentID}, true)
		return err
	})
}

// CloseRow records the user closing a row on the bench: every negative
// decision without a label of each of the given proposals (the row's) that
// holds a label is labeled with its judged value, but for one a proposal on
// the same content holds as an item without a label, or as a reply its own
// commitment absorbs, all in one transaction. It returns ErrNotFound,
// recording nothing, when a judgment is unknown.
func (s *Store) CloseRow(ctx context.Context, judgments []uuid.UUID) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		_, err := s.labelNegativesTx(ctx, tx, judgments, false)
		return err
	})
}

// InferNegativeLabels runs the labeling rule's inference once over every
// content the user has labeled, each content's proposals together and in a
// transaction of their own, as a close of their row would, so a proposal
// holding a label before the rule existed holds no unlabeled negative
// decision. Only content with a label can hold such a proposal. It never
// replaces a label and labels nothing in a proposal holding no label; a second
// run records nothing. It returns how many labels it recorded.
func (s *Store) InferNegativeLabels(ctx context.Context) (int, error) {
	rows, err := s.queries.ListLatestExampleReviews(ctx, db.ListLatestExampleReviewsParams{
		ReviewPredicate: predicateReview, KeepsDatePredicate: predicateReviewKeepsDate, ExampleIds: []string{},
	})
	if err != nil {
		return 0, fmt.Errorf("list labeled content: %w", err)
	}
	var examples []string
	for _, r := range rows {
		if len(examples) == 0 || examples[len(examples)-1] != r.ExampleID {
			examples = append(examples, r.ExampleID)
		}
	}
	total := 0
	for _, e := range examples {
		var n int
		if err := s.inTx(ctx, func(tx pgx.Tx) error {
			judgments, err := db.New(tx).ListJudgmentRows(ctx, db.ListJudgmentRowsParams{Ids: []uuid.UUID{}, ExampleIds: []string{e}})
			if err != nil {
				return fmt.Errorf("list content judgments: %w", err)
			}
			n, err = s.labelNegativesTx(ctx, tx, judgmentIDs(judgments), false)
			return err
		}); err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}
