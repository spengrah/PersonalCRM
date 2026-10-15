package judgment

// Question names one question of the tree a method asks of a thread context.
// Ball-in-court and the two commitment questions are asked per participant.
type Question string

const (
	QuestionBallInCourt      Question = "ball_in_court"
	QuestionCommitmentMine   Question = "commitment_mine"
	QuestionCommitmentTheirs Question = "commitment_theirs"
	QuestionBigMoment        Question = "big_moment"
)

// BallInCourt is the ball-in-court outcome for one participant.
type BallInCourt string

const (
	BallInCourtIOweReply     BallInCourt = "i_owe_reply"
	BallInCourtTheyOweReply  BallInCourt = "they_owe_reply"
	BallInCourtNobodyOwes    BallInCourt = "nobody_owes"
	BallInCourtNotApplicable BallInCourt = "not_applicable"
)

// CommitmentState is a commitment question's outcome.
type CommitmentState string

const (
	CommitmentStateOpen CommitmentState = "open"
	CommitmentStateNone CommitmentState = "none"
)

// CommitmentDirection says who committed: the user (mine) or the participant
// (theirs).
type CommitmentDirection string

const (
	CommitmentDirectionMine   CommitmentDirection = "mine"
	CommitmentDirectionTheirs CommitmentDirection = "theirs"
)

// BigMoment is the big-moment outcome: none, a participant's own significant
// moment, or another person's mentioned in passing.
type BigMoment string

const (
	BigMomentNone        BigMoment = "none"
	BigMomentParticipant BigMoment = "participant_moment"
	BigMomentOtherPerson BigMoment = "other_person_moment"
)

// RunKind is how a run was started. Only scheduled and historical runs of the
// production method change live state.
type RunKind string

const (
	RunKindScheduled  RunKind = "scheduled"
	RunKindHistorical RunKind = "historical"
	RunKindRerun      RunKind = "rerun"
)

// IncompleteReason is why a run could not judge a thread at a cut-off.
type IncompleteReason string

const (
	IncompleteReasonOverLimit    IncompleteReason = "over_limit"
	IncompleteReasonCallFailed   IncompleteReason = "call_failed"
	IncompleteReasonInputChanged IncompleteReason = "input_changed"
)

// OutcomeState is how an outcome landed: applied (it took effect, or had no
// effect to take), claim (it awaits the user's confirmation) or evidence (its
// judgment cannot change live state).
type OutcomeState string

const (
	OutcomeStateApplied  OutcomeState = "applied"
	OutcomeStateClaim    OutcomeState = "claim"
	OutcomeStateEvidence OutcomeState = "evidence"
)

// Verdict is derived from a review's right outcome: confirmed when it equals
// the judged outcome, rejected when it differs and is a no-effect value,
// corrected when it differs and has an effect.
type Verdict string

const (
	VerdictConfirmed Verdict = "confirmed"
	VerdictRejected  Verdict = "rejected"
	VerdictCorrected Verdict = "corrected"
)
