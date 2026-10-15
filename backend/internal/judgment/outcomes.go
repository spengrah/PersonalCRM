package judgment

import (
	"context"
	"fmt"
	"time"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
)

// outcomeRecord is one outcome node with its assertions: the method's answer
// (absent for an omission), its latest landing under a class predicate
// (absent while the outcome is evidence), and the user's live review.
// Confidence is the method's, at the precision it reported (absent for an
// omission).
type outcomeRecord struct {
	ID         uuid.UUID
	JudgmentID uuid.UUID
	Question   Question
	QuestionID string
	ContactID  *uuid.UUID
	DueDate    *time.Time
	Confidence *float64
	answer     *db.Assertion
	class      *db.Assertion
	review     *db.Assertion
}

// value is the judged outcome: the method's answer, or the omission's value.
func (o *outcomeRecord) value() string {
	switch {
	case o.answer != nil && o.answer.ValueText != nil:
		return *o.answer.ValueText
	case o.class != nil && o.class.ValueText != nil:
		return *o.class.ValueText
	default:
		return ""
	}
}

// confidence is the method's confidence in the outcome; an omission, which no
// method answered, has none.
func (o *outcomeRecord) confidence() float64 {
	if o.Confidence == nil {
		return 0
	}
	return *o.Confidence
}

// state is how the outcome landed: evidence when it never landed; a claim
// when its landing awaits or failed the user's confirmation; applied when it
// took effect (or had none to take), including a confirmed claim.
func (o *outcomeRecord) state() OutcomeState {
	if o.class == nil {
		return OutcomeStateEvidence
	}
	switch o.class.Status {
	case repository.AssertionStatusProposed, repository.AssertionStatusRejected:
		return OutcomeStateClaim
	default:
		return OutcomeStateApplied
	}
}

func (o *outcomeRecord) reviewView() *OutcomeReview {
	if o.review == nil || o.review.ValueText == nil {
		return nil
	}
	right := *o.review.ValueText
	return &OutcomeReview{
		Verdict:      deriveVerdict(o.value(), right),
		RightOutcome: right,
		ReviewedAt:   o.review.KnowledgeFrom,
	}
}

func (o *outcomeRecord) view() OutcomeView {
	return OutcomeView{
		ID:         o.ID,
		QuestionID: o.QuestionID,
		Question:   o.Question,
		ContactID:  o.ContactID,
		Outcome:    o.value(),
		Confidence: o.confidence(),
		DueDate:    o.DueDate,
		State:      o.state(),
		Review:     o.reviewView(),
	}
}

// key names the question and contact an outcome answers, the unit a review
// labels and the user's verdict governs.
func (o *outcomeRecord) key() outcomeKey {
	return outcomeKey{question: o.Question, contact: contactOf(o.ContactID)}
}

type outcomeKey struct {
	question Question
	contact  uuid.UUID
}

func contactOf(id *uuid.UUID) uuid.UUID {
	if id == nil {
		return uuid.Nil
	}
	return *id
}

// loadOutcomes reads the outcomes of the given judgments, or the given
// outcomes, with their assertions.
func loadOutcomes(ctx context.Context, q db.Querier, judgmentIDs, outcomeIDs []uuid.UUID) ([]*outcomeRecord, error) {
	rows, err := q.ListJudgmentOutcomes(ctx, db.ListJudgmentOutcomesParams{
		JudgmentIds: emptyIfNil(judgmentIDs), OutcomeIds: emptyIfNil(outcomeIDs),
	})
	if err != nil {
		return nil, fmt.Errorf("list outcomes: %w", err)
	}
	records := make([]*outcomeRecord, len(rows))
	byID := make(map[uuid.UUID]*outcomeRecord, len(rows))
	ids := make([]uuid.UUID, len(rows))
	for i, row := range rows {
		r := &outcomeRecord{
			ID:         row.NodeID,
			JudgmentID: row.JudgmentID,
			Question:   Question(row.Question),
			ContactID:  row.ContactID,
			DueDate:    row.DueDate,
			Confidence: row.Confidence,
		}
		if row.QuestionID != nil {
			r.QuestionID = *row.QuestionID
		}
		records[i], byID[r.ID], ids[i] = r, r, r.ID
	}
	if len(ids) == 0 {
		return records, nil
	}
	assertions, err := q.ListOutcomeAssertions(ctx, db.ListOutcomeAssertionsParams{
		OutcomeIds: ids,
		Predicates: append([]string{predicateAnswer, predicateReview}, classPredicates...),
	})
	if err != nil {
		return nil, fmt.Errorf("list outcome assertions: %w", err)
	}
	// Assertions arrive oldest first, so the last of each role wins.
	for _, a := range assertions {
		r := byID[a.SubjectNodeID]
		switch a.PredicateKey {
		case predicateAnswer:
			if a.Status == repository.AssertionStatusAccepted {
				r.answer = a
			}
		case predicateReview:
			if a.Status == repository.AssertionStatusAccepted {
				r.review = a
			}
		default:
			r.class = a
		}
	}
	return records, nil
}

func emptyIfNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
