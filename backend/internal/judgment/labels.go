package judgment

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/service"
	"personal-crm/backend/internal/threadctx"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrConfirmNonesRefused is returned for a Confirm Nones on a night that shows a
// chip, a clearing decision or a reply item, or that holds a label.
var ErrConfirmNonesRefused = errors.New("confirm nones applies only to a night that shows nothing and holds no label")

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

// review is one label the user states, for the review writer to record: a
// review of a reply-proposal outcome, or a label of a flag decision or of a
// clearing decision.
type review interface{ isReview() }

// outcomeReview is the user's review of an outcome's decision: the right
// outcome, and whether it keeps an open commitment's current date.
type outcomeReview struct {
	outcome   *outcomeRecord
	right     string
	keepsDate bool
}

// flagReview is the user's label of a flag decision: whether the message holds
// the kind.
type flagReview struct {
	thread     uuid.UUID
	messageKey string
	kind       FlagKind
	holds      bool
}

// clearingReview is the user's label of a clearing decision: whether the
// commitment's message is done as of the cut-off.
type clearingReview struct {
	thread        uuid.UUID
	commitmentKey string
	cutOff        time.Time
	done          bool
}

func (outcomeReview) isReview()  {}
func (flagReview) isReview()     {}
func (clearingReview) isReview() {}

// recordReviewTx records the user's label of a decision, whichever kind of
// decision it is. A review of an outcome is recorded as assertions with user
// provenance: the outcome's earlier review, if any, is retracted first, so each
// review is a new assertion and the latest review of a decision is its label
// whichever outcomes earlier reviews were made on. A label of a flag decision
// or of a clearing decision replaces the decision's one row, so a decision has
// one current label. It is the one writer of reviews: every label, whether
// made by a control, by Confirm Nones or by a close, is recorded here.
func (s *Store) recordReviewTx(ctx context.Context, tx pgx.Tx, r review) error {
	switch r := r.(type) {
	case outcomeReview:
		return s.recordOutcomeReviewTx(ctx, tx, r)
	case flagReview:
		if err := db.New(tx).UpsertJudgmentFlagLabel(ctx, db.UpsertJudgmentFlagLabelParams{
			ThreadID: r.thread, MessageKey: r.messageKey, Kind: string(r.kind), Holds: r.holds, LabeledAt: now(),
		}); err != nil {
			return fmt.Errorf("record flag label: %w", err)
		}
		return nil
	case clearingReview:
		if err := db.New(tx).UpsertJudgmentClearingLabel(ctx, db.UpsertJudgmentClearingLabelParams{
			ThreadID: r.thread, CommitmentMessageKey: r.commitmentKey, CutOff: r.cutOff, Done: r.done, LabeledAt: now(),
		}); err != nil {
			return fmt.Errorf("record clearing label: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("record review: unknown review %T", r)
	}
}

func (s *Store) recordOutcomeReviewTx(ctx context.Context, tx pgx.Tx, r outcomeReview) error {
	o := r.outcome
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
		ValueText:     &r.right,
		Confidence:    100,
		Locators:      []service.ProvenanceLocator{locator},
	}); err != nil {
		return fmt.Errorf("assert review: %w", err)
	}
	if _, err := s.assert.AssertTx(ctx, tx, service.AssertRequest{
		SubjectNodeID: o.ID,
		PredicateKey:  predicateReviewKeepsDate,
		ValueBool:     &r.keepsDate,
		Confidence:    100,
		Locators:      []service.ProvenanceLocator{locator},
	}); err != nil {
		return fmt.Errorf("assert review keeps date: %w", err)
	}
	return nil
}

// FlagLabel is the user's label of a flag decision: whether the message holds
// the kind. A label belongs to the decision, so every method, run and cut-off
// that judged the message and kind reads it.
type FlagLabel struct {
	Thread     threadctx.VenueID
	MessageKey string
	Kind       FlagKind
	Holds      bool
}

// ClearingLabel is the user's label of a clearing decision: whether the
// commitment's message is done as of the cut-off of the judgment that
// re-checked it.
type ClearingLabel struct {
	Thread               threadctx.VenueID
	CommitmentMessageKey string
	CutOff               time.Time
	Done                 bool
}

// ThreadLabels are a thread's current labels of flag and clearing decisions:
// one per labeled decision, the latest review's, whoever or whatever wrote it.
type ThreadLabels struct {
	Flags     []FlagLabel
	Clearings []ClearingLabel
}

// LabelsByThread returns the thread's current flag and clearing labels, each
// decision once. A label a close or Confirm Nones wrote reads as any other.
func (s *Store) LabelsByThread(ctx context.Context, thread threadctx.VenueID) (ThreadLabels, error) {
	byThread, err := loadDecisionLabels(ctx, s.queries, []uuid.UUID{thread})
	if err != nil {
		return ThreadLabels{}, err
	}
	return byThread[thread], nil
}

// loadDecisionLabels reads the flag and clearing labels of the given threads.
func loadDecisionLabels(ctx context.Context, q db.Querier, threads []uuid.UUID) (map[uuid.UUID]ThreadLabels, error) {
	out := make(map[uuid.UUID]ThreadLabels, len(threads))
	for _, t := range threads {
		out[t] = ThreadLabels{Flags: []FlagLabel{}, Clearings: []ClearingLabel{}}
	}
	flags, err := q.ListJudgmentFlagLabels(ctx, threads)
	if err != nil {
		return nil, fmt.Errorf("list flag labels: %w", err)
	}
	for _, f := range flags {
		l := out[f.ThreadID]
		l.Flags = append(l.Flags, FlagLabel{Thread: f.ThreadID, MessageKey: f.MessageKey, Kind: FlagKind(f.Kind), Holds: f.Holds})
		out[f.ThreadID] = l
	}
	clearings, err := q.ListJudgmentClearingLabels(ctx, threads)
	if err != nil {
		return nil, fmt.Errorf("list clearing labels: %w", err)
	}
	for _, c := range clearings {
		l := out[c.ThreadID]
		l.Clearings = append(l.Clearings, ClearingLabel{Thread: c.ThreadID, CommitmentMessageKey: c.CommitmentMessageKey, CutOff: c.CutOff, Done: c.Done})
		out[c.ThreadID] = l
	}
	return out, nil
}

// labelNegativesTx is the one rule by which a negative decision gains a label
// no user act states. The rule labels only a negative decision without a
// label that it does not spare: it spares a decision some proposal on the
// same content holds as an item without a label, which awaits the user's
// review of the item in every proposal on the content (G4). A night's close
// or Confirm Nones runs it on the night's judgment (labelNightNegativesTx);
// inference runs it on each given proposal that holds a label, until no given
// proposal changes, since a label shared across proposals on the same content
// can give another proposal its first label. A label that exists is never
// replaced, and a proposal holding no label gains none by inference. Each
// label is an ordinary review and changes nothing live. A review of an item
// never runs the rule. It returns how many labels it recorded, or ErrNotFound
// when a judgment is unknown.
func (s *Store) labelNegativesTx(ctx context.Context, tx pgx.Tx, judgments []uuid.UUID, infer bool) (int, error) {
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
			if err := s.recordReviewTx(ctx, tx, outcomeReview{outcome: o, right: o.value()}); err != nil {
				return err
			}
			lb[o.decision()] = label{right: o.value()}
			recorded++
		}
		return nil
	}
	for changed := true; changed; {
		changed = false
		sp := spared(byJudgment, lb)
		for _, j := range rows {
			sorted := sortOutcomes(byJudgment[j.ID], lb)
			if infer && !sorted.holdsLabel(lb) {
				continue
			}
			pending := sorted.unlabeledNegatives(lb, sp.spares)
			if err := labelAll(pending); err != nil {
				return 0, err
			}
			changed = changed || len(pending) > 0
		}
		if !infer {
			break
		}
	}
	return recorded, nil
}

// InferNegativeLabels runs the labeling rule's inference once over every
// content the user has labeled, each content's proposals together and in a
// transaction of their own, as a close of their thread would, so a proposal
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
			n, err = s.labelNegativesTx(ctx, tx, judgmentIDs(judgments), true)
			return err
		}); err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}
