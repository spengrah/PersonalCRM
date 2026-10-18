package judgment

import (
	"math"
	"slices"
	"strings"
)

// Predicates of the judgment store (migration 093). The catalog rows hold the
// apply rule: the assertion write path lands each by its row alone.
const (
	// predicateAnswer records the outcome a method chose.
	predicateAnswer = "judgment_answer"
	// predicateAddition, predicateRemoval and predicateNoEffect record how a
	// live outcome landed: its class, by the effect it has on live state.
	predicateAddition = "judgment_addition"
	predicateRemoval  = "judgment_removal"
	predicateNoEffect = "judgment_no_effect"
	// predicateReview holds the right outcome the user stated.
	predicateReview = "judgment_review"
	// predicateReviewKeepsDate says whether a review keeps an open
	// commitment's current date against the judged later date; it is written
	// beside every review.
	predicateReviewKeepsDate = "judgment_review_keeps_date"
	// predicateRemovalTarget holds the outcome value whose live effect a
	// removal would take away; it is written beside every removal landing.
	predicateRemovalTarget = "judgment_removal_target"
	// predicateFlag is a big-moment flag on a thread.
	predicateFlag = "big_moment_flag"
)

// classPredicates are the landing classes, in the order the queries read them.
var classPredicates = []string{predicateAddition, predicateRemoval, predicateNoEffect}

// questionValues lists each question's outcome values: the one definition the
// store validates outcomes and right outcomes against.
var questionValues = map[Question][]string{
	QuestionBallInCourt:      {string(BallInCourtIOweReply), string(BallInCourtTheyOweReply), string(BallInCourtNobodyOwes), string(BallInCourtNotApplicable)},
	QuestionCommitmentMine:   {string(CommitmentStateOpen), string(CommitmentStateNone)},
	QuestionCommitmentTheirs: {string(CommitmentStateOpen), string(CommitmentStateNone)},
	QuestionBigMoment:        {string(BigMomentNone), string(BigMomentParticipant), string(BigMomentOtherPerson)},
}

// isOutcomeValue reports whether value is one of question's outcome values.
func isOutcomeValue(question Question, value string) bool {
	return slices.Contains(questionValues[question], value)
}

// OptionValue is the outcome value an answered option stands for: its key,
// or for a reasoned option keyed "<outcome>.<reason>", that outcome. The
// decision tree decides by it, and the bench sums a value's probability by
// it.
func OptionValue(key string) string {
	value, _, _ := strings.Cut(key, ".")
	return value
}

// isNoEffectValue reports whether an outcome value opens and raises nothing.
func isNoEffectValue(value string) bool {
	switch value {
	case string(BallInCourtNobodyOwes), string(BallInCourtNotApplicable), string(BigMomentNone):
		return true
	default:
		return false
	}
}

// deriveVerdict derives a review's verdict from the judged and right outcomes
// and whether the review kept the current date against the judged one, which
// makes the judged outcome wrong although its value is right.
func deriveVerdict(judged, right string, keepsDate bool) Verdict {
	switch {
	case right == judged && !keepsDate:
		return VerdictConfirmed
	case isNoEffectValue(right):
		return VerdictRejected
	default:
		return VerdictCorrected
	}
}

// percent converts a probability to the assertion store's 0-100 confidence,
// rounding down so a confidence below the threshold never reaches it.
func percent(confidence float64) int16 {
	p := math.Floor(confidence*100 + 1e-9)
	return int16(min(max(p, 0), 100))
}

// keepsDateApplies reports whether a review of the question stating right may
// keep the current date: only an open commitment has a date to keep.
func keepsDateApplies(question Question, right string) bool {
	return (question == QuestionCommitmentMine || question == QuestionCommitmentTheirs) && right == string(CommitmentStateOpen)
}

// commitmentQuestion is the question a commitment direction answers.
func commitmentQuestion(d CommitmentDirection) Question {
	if d == CommitmentDirectionMine {
		return QuestionCommitmentMine
	}
	return QuestionCommitmentTheirs
}
