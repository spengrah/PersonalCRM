package judgment

import (
	"math"
	"slices"
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

// isNoEffectValue reports whether an outcome value opens and raises nothing.
func isNoEffectValue(value string) bool {
	switch value {
	case string(BallInCourtNobodyOwes), string(BallInCourtNotApplicable), string(BigMomentNone):
		return true
	default:
		return false
	}
}

// deriveVerdict derives a review's verdict from the judged and right outcomes.
func deriveVerdict(judged, right string) Verdict {
	switch {
	case right == judged:
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

// commitmentQuestion is the question a commitment direction answers.
func commitmentQuestion(d CommitmentDirection) Question {
	if d == CommitmentDirectionMine {
		return QuestionCommitmentMine
	}
	return QuestionCommitmentTheirs
}
