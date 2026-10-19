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
	// IncompleteReasonMessageOverLimit: the newest message alone exceeds the
	// method's limits.
	IncompleteReasonMessageOverLimit IncompleteReason = "message_over_limit"
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

// ProposalList is a list of a proposal.
type ProposalList string

const (
	ProposalListIOweReply   ProposalList = "i_owe_reply"
	ProposalListOwesMeReply ProposalList = "owes_me_reply"
	ProposalListCommitments ProposalList = "commitments"
	ProposalListBigMoment   ProposalList = "big_moment"
)

// ItemState is where an item's decision sits: live (acting: its obligation
// open and its task created, or its flag raised), suggested (awaiting the
// user's OK), not acting (changing nothing live: evidence, or an item whose
// effect no longer holds), or live with a suggested change (a live item the
// judgment would remove or move later).
type ItemState string

const (
	ItemStateLive           ItemState = "live"
	ItemStateSuggested      ItemState = "suggested"
	ItemStateNotActing      ItemState = "not_acting"
	ItemStateLiveWithChange ItemState = "live_with_change"
)

// ChangeKind is a suggested change to a live item.
type ChangeKind string

const (
	ChangeKindRemove    ChangeKind = "remove"
	ChangeKindLaterDate ChangeKind = "later_date"
)

// ItemMark is how the user's review left an item: confirmed (confirmed, or a
// live item kept against a suggested change), removed, or added.
type ItemMark string

const (
	ItemMarkConfirmed ItemMark = "confirmed"
	ItemMarkRemoved   ItemMark = "removed"
	ItemMarkAdded     ItemMark = "added"
)

// ActKind names a review control of an item; on a reviewed item it is a
// re-review control.
type ActKind string

const (
	ActKindConfirm      ActKind = "confirm"
	ActKindRemove       ActKind = "remove"
	ActKindAccept       ActKind = "accept"
	ActKindKeep         ActKind = "keep"
	ActKindKeepOriginal ActKind = "keep_original"
)

// Debtor is who owes a commitment: the user or the participant.
type Debtor string

const (
	DebtorMe      Debtor = "me"
	DebtorContact Debtor = "contact"
)

// FlagKind is what a flag decision flags a message as holding. A flag carries
// no category and no extracted content.
type FlagKind string

const (
	// FlagKindCommitment: the message states that someone will do something for
	// another participant, and the content up to the cut-off does not show it
	// done.
	FlagKindCommitment FlagKind = "commitment"
	// FlagKindBigMoment: the message reveals a significant moment in a contact's
	// life.
	FlagKindBigMoment FlagKind = "big_moment"
	// FlagKindClaim: the message states a fact about a contact or someone in a
	// contact's life.
	FlagKindClaim FlagKind = "claim"
)

// UnjudgedReason is why a new message was answered by no judgment of a run.
type UnjudgedReason string

const (
	// UnjudgedReasonCallFailed: the cut-off's call failed after its retries.
	UnjudgedReasonCallFailed UnjudgedReason = "call_failed"
	// UnjudgedReasonMessageOverLimit: the cut-off was reported incomplete with
	// IncompleteReasonMessageOverLimit.
	UnjudgedReasonMessageOverLimit UnjudgedReason = "message_over_limit"
	// UnjudgedReasonBeforeWindow: the message lay before the window's start.
	UnjudgedReasonBeforeWindow UnjudgedReason = "before_window"
)

// ThreadVenue is the kind of place a thread lives in, as the Threads tab
// filters and names it. sourceVenues maps each venue source to one.
type ThreadVenue string

const (
	ThreadVenueWhatsApp ThreadVenue = "whatsapp"
	ThreadVenueTelegram ThreadVenue = "telegram"
	ThreadVenueIMessage ThreadVenue = "imessage"
	ThreadVenueEmail    ThreadVenue = "email"
	ThreadVenueCalendar ThreadVenue = "calendar"
)

// ChipState is how a flag chip stands under a night's method: to review (a
// shown flag with no label), confirmed or removed (a shown flag with a label
// that holds or does not), or added (the user's label that a message holds a
// kind the method did not show).
type ChipState string

const (
	ChipStateToReview  ChipState = "to_review"
	ChipStateConfirmed ChipState = "confirmed"
	ChipStateRemoved   ChipState = "removed"
	ChipStateAdded     ChipState = "added"
)

// ClearingLabelState is how the user's label stands against a clearing
// decision's answer: none (no label), confirmed (the label equals the answer)
// or removed (it differs).
type ClearingLabelState string

const (
	ClearingLabelStateNone      ClearingLabelState = "none"
	ClearingLabelStateConfirmed ClearingLabelState = "confirmed"
	ClearingLabelStateRemoved   ClearingLabelState = "removed"
)

// ThreadStatus is what a thread holds for the user to review in a run:
// to review (it shows an item without a label, or a night awaits Confirm
// Nones), nothing flagged (it shows nothing) or reviewed (every night holds
// one of the user's labels).
type ThreadStatus string

const (
	ThreadStatusToReview       ThreadStatus = "to_review"
	ThreadStatusNothingFlagged ThreadStatus = "nothing_flagged"
	ThreadStatusReviewed       ThreadStatus = "reviewed"
)
