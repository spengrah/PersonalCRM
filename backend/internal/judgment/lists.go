package judgment

import (
	"math"
	"slices"

	"github.com/google/uuid"
)

// ProposalListView is one list a proposal shows, beside its items. Closest is
// the decision the judgment came closest to answering with the list's value
// among the decisions no item of the list represents; nil when the list has
// no candidate. NonesReviewed is true exactly when at least one negative
// decision sits behind the list and none is a None to review.
type ProposalListView struct {
	List          ProposalList
	Closest       *ClosestDecision
	NonesReviewed bool
}

// ClosestDecision names a list's closest decision as the list's items name
// their decisions (Contact for a reply list, Contact and Debtor for a
// commitment, neither for the big moment) with the probability of the list's
// value as a whole percentage.
type ClosestDecision struct {
	Contact *uuid.UUID
	Debtor  Debtor
	Percent int
}

// listSpec is what a list is made of: the questions whose decisions it
// shows, the outcome values that put a decision in it (the first is the one
// an Add to the list states), and whether it names each decision by its
// contact.
type listSpec struct {
	list      ProposalList
	questions []Question
	values    []string
	byContact bool
}

// listSpecs are the lists, in display order: the one mapping from a
// question's outcome value to the list it puts an item in, which the items,
// the pickers, the closest decisions and the nones all read.
var listSpecs = []listSpec{
	{ProposalListIOweReply, []Question{QuestionBallInCourt}, []string{string(BallInCourtIOweReply)}, true},
	{ProposalListOwesMeReply, []Question{QuestionBallInCourt}, []string{string(BallInCourtTheyOweReply)}, true},
	{ProposalListCommitments, []Question{QuestionCommitmentMine, QuestionCommitmentTheirs}, []string{string(CommitmentStateOpen)}, true},
	{ProposalListBigMoment, []Question{QuestionBigMoment}, []string{string(BigMomentParticipant), string(BigMomentOtherPerson)}, false},
}

// specOf is the list's spec.
func specOf(list ProposalList) listSpec {
	i := slices.IndexFunc(listSpecs, func(spec listSpec) bool { return spec.list == list })
	return listSpecs[i]
}

// listOf is the list an outcome value puts an item in; empty for a value with
// no effect.
func listOf(question Question, value string) ProposalList {
	for _, spec := range listSpecs {
		if slices.Contains(spec.questions, question) && slices.Contains(spec.values, value) {
			return spec.list
		}
	}
	return ""
}

// party is the contact the list names the outcome's decision by; nil for a
// list that names none.
func (spec listSpec) party(o *outcomeRecord) *uuid.UUID {
	if !spec.byContact {
		return nil
	}
	return o.ContactID
}

// listViews compiles each list a proposal shows, in display order: the reply
// lists only where ball-in-court was asked.
func listViews(replyLists bool, questions []AnsweredQuestion, outcomes []*outcomeRecord, sorted sorting, lb labels) []ProposalListView {
	views := []ProposalListView{}
	for _, spec := range listSpecs {
		if !replyLists && slices.Contains(spec.questions, QuestionBallInCourt) {
			continue
		}
		views = append(views, ProposalListView{
			List:          spec.list,
			Closest:       spec.closest(questions, outcomes, sorted),
			NonesReviewed: spec.nonesReviewed(sorted, lb),
		})
	}
	return views
}

// closest is the candidate with the highest probability of the list's value:
// an answered outcome of one of the list's questions that no item of the list
// stands for and whose question stored option probabilities. A positive
// decision of another list is a candidate. Ties go to the earlier outcome.
func (spec listSpec) closest(questions []AnsweredQuestion, outcomes []*outcomeRecord, sorted sorting) *ClosestDecision {
	var best *outcomeRecord
	bestP := -1.0
	for _, o := range outcomes {
		if o.QuestionID == "" || !slices.Contains(spec.questions, o.Question) {
			continue
		}
		if slices.ContainsFunc(sorted.items, func(pl placement) bool { return pl.o == o && pl.list == spec.list }) {
			continue
		}
		i := slices.IndexFunc(questions, func(q AnsweredQuestion) bool { return q.ID == o.QuestionID })
		if i < 0 || len(questions[i].Probabilities) == 0 {
			continue
		}
		if p := spec.probability(questions[i]); p > bestP {
			best, bestP = o, p
		}
	}
	if best == nil {
		return nil
	}
	return &ClosestDecision{Contact: spec.party(best), Debtor: debtorOf(best.Question), Percent: int(min(max(math.Round(bestP*100), 0), 100))}
}

// probability is the probability an answered question gives the list's
// value: the sum of the option probabilities that stand for one of its values.
func (spec listSpec) probability(q AnsweredQuestion) float64 {
	sum := 0.0
	for key, p := range q.Probabilities {
		if slices.Contains(spec.values, OptionValue(key)) {
			sum += p
		}
	}
	return sum
}

// nonesReviewed reports whether at least one negative decision of the list's
// questions sits behind the list and none is a None to review.
func (spec listSpec) nonesReviewed(sorted sorting, lb labels) bool {
	behind := func(o *outcomeRecord) bool { return slices.Contains(spec.questions, o.Question) }
	return slices.ContainsFunc(sorted.negatives, behind) && !slices.ContainsFunc(sorted.nonesToReview(lb), behind)
}
