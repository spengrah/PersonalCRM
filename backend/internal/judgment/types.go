// Package judgment stores decision-model judgments of thread contexts as SP1
// assertions and turns the production method's current judgments into live
// state: obligations with their tasks, big-moment flags, and claims the user
// reviews.
package judgment

import (
	"context"
	"errors"
	"maps"
	"slices"
	"time"

	"personal-crm/backend/internal/threadctx"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Cadence is the awaiting-reply writer a confirmed "nobody owes" claim ends a
// wait through. *consumer.CadenceUpdater satisfies it.
type Cadence interface {
	EndAwaitingReplyTx(ctx context.Context, tx pgx.Tx, contactID uuid.UUID) error
}

var (
	// ErrNotFound is returned for an unknown outcome.
	ErrNotFound = errors.New("judgment outcome not found")
	// ErrInvalidRightOutcome is returned for an empty right outcome, or one not
	// among the reviewed question's outcome values.
	ErrInvalidRightOutcome = errors.New("invalid judgment right outcome")
	// ErrInvalidKeepCurrentDate is returned for a review that keeps the current
	// date of anything but an open commitment.
	ErrInvalidKeepCurrentDate = errors.New("only a review stating an open commitment can keep its current date")
	// ErrUnboundedQuery is returned by ListJudgments with neither thread nor run.
	ErrUnboundedQuery = errors.New("judgment query must name a thread or run")
)

// RunID identifies a run.
type RunID = uuid.UUID

// ExampleID is the opaque ID the store issues for one thread and input hash.
type ExampleID string

// MethodRef names a method version and its question wording.
type MethodRef struct {
	Key            string
	Version        string
	WordingVersion string
}

// Usage is what one call cost. BilledTokens and CostUSD are nil where the
// source reported none.
type Usage struct {
	ContextTokens int
	Questions     int
	BilledTokens  *int
	CostUSD       *float64
	Latency       time.Duration
}

// AnsweredQuestion is one question as the method answered it: every option's
// probability (option key to probability) and the model's original answer.
type AnsweredQuestion struct {
	ID             string
	Probabilities  map[string]float64
	OriginalAnswer string
}

// Chosen is the question's selected option: the model's original answer,
// else its most probable option, ties to the first key.
func (q AnsweredQuestion) Chosen() string {
	if q.OriginalAnswer != "" {
		return q.OriginalAnswer
	}
	best, bestP := "", -1.0
	for _, k := range slices.Sorted(maps.Keys(q.Probabilities)) {
		if q.Probabilities[k] > bestP {
			best, bestP = k, q.Probabilities[k]
		}
	}
	return best
}

// ParticipantOutcome is a per-participant outcome.
type ParticipantOutcome[T comparable] struct {
	QuestionID string
	ContactID  uuid.UUID
	Outcome    T
	Confidence float64
}

// CommitmentOutcome is a commitment question's outcome for one participant.
// DueDate is the commitment's selected calendar date, when known.
type CommitmentOutcome struct {
	QuestionID string
	ContactID  uuid.UUID
	Direction  CommitmentDirection
	Outcome    CommitmentState
	Confidence float64
	DueDate    *time.Time
}

// Scored is a thread-level outcome.
type Scored[T comparable] struct {
	QuestionID string
	Outcome    T
	Confidence float64
}

// Outcomes are a judgment's typed answers.
type Outcomes struct {
	BallInCourt []ParticipantOutcome[BallInCourt]
	Commitments []CommitmentOutcome
	BigMoment   Scored[BigMoment]
}

// Judgment is one method's answers to one question tree for one thread at one
// cut-off. JudgedAt is the execution time of the call that produced it.
// InputHash identifies the whole context; WindowStart, NewMessages, Flags and
// Clearings record what the method read of it and answered about messages. A
// judgment of a version before migration 099 sets none of them; one that sets
// WindowStart records flag and clearing decisions and holds ball-in-court
// outcomes only. Chunks are the reads of a night the method judged in more
// than one chunk (Read). ID is the judgment's identity, which a clearing
// decision names when an earlier chunk of the same night flagged its
// commitment; the store issues one when it is zero.
type Judgment struct {
	ID          uuid.UUID
	Method      MethodRef
	Thread      threadctx.VenueID
	Kind        threadctx.ContextKind
	CutOff      time.Time
	JudgedAt    time.Time
	InputHash   string
	WindowStart string
	NewMessages []string
	Chunks      []Read
	Flags       []FlagDecision
	Clearings   []ClearingDecision
	Questions   []AnsweredQuestion
	Outcomes    Outcomes
	Usage       Usage
}

// RunSpec describes a run to start.
type RunSpec struct {
	Kind   RunKind
	Method MethodRef
	From   *time.Time
	To     *time.Time
}

// Effects counts what one call changed.
type Effects struct {
	ObligationsOpened int
	FlagsRaised       int
	ClaimsRaised      int
}

// ThreadCutOff is a thread a run judged, at the run's cut-off.
type ThreadCutOff struct {
	Thread threadctx.VenueID
	CutOff time.Time
}

// Example is one reviewed input: a thread at an input hash, with the user's
// reviews of it. A review labels the input for every method. Question is the
// reviewed outcome's question; ContactID is nil for big_moment.
type Example struct {
	ID        ExampleID
	Thread    threadctx.VenueID
	CutOff    time.Time
	InputHash string
	Reviews   []Review
}

// Review is the user's right outcome for one question and contact.
type Review struct {
	Question     string
	ContactID    *uuid.UUID
	RightOutcome string
}

// RunRecord is a finished run's content-free record.
type RunRecord struct {
	Run        RunID
	Kind       RunKind
	Method     MethodRef
	From       *time.Time
	To         *time.Time
	StartedAt  time.Time
	FinishedAt time.Time
	Judged     int
	Incomplete map[IncompleteReason]int
	// Unjudged counts the run's unjudged messages by reason.
	Unjudged map[UnjudgedReason]int
	Effects  Effects
	// CostUSD is summed over the calls the run made; nil when none reported a
	// cost.
	CostUSD *float64
}

// QuestionOutcome is one outcome keyed by the method's question ID.
type QuestionOutcome struct {
	QuestionID string
	Outcome    string
	Confidence float64
}

// JudgmentRecord is one judgment appearing in a run, content-free.
type JudgmentRecord struct {
	Example ExampleID
	Method  MethodRef
	// ReadDigest tells apart a method version's judgments of one example that
	// read different contexts; empty for a judgment before migration 099.
	ReadDigest string
	CutOff     time.Time
	JudgedAt   time.Time
	Questions  []AnsweredQuestion
	Outcomes   []QuestionOutcome
	Usage      Usage
	// WindowStart, NewMessages, Flags and Clearings are as JudgmentView holds
	// them. They name messages by key and carry no text.
	WindowStart string
	NewMessages []string
	Flags       []FlagDecision
	Clearings   []ClearingDecision
}

// ReviewRecord is one review, content-free.
type ReviewRecord struct {
	Example ExampleID
	Method  MethodRef
	// ReadDigest is the reviewed judgment's, as JudgmentRecord holds it.
	ReadDigest   string
	QuestionID   string
	Verdict      Verdict
	RightOutcome string
	ReviewedAt   time.Time
}

// RunSummary is a run as the bench lists it.
type RunSummary struct {
	ID          RunID
	Kind        RunKind
	Method      MethodRef
	From        *time.Time
	To          *time.Time
	StartedAt   time.Time
	FinishedAt  *time.Time
	JudgedCount int
	Incomplete  []IncompleteThread
	// Unjudged lists the new messages no judgment of the run answered, and
	// UnjudgedCounts counts them by reason.
	Unjudged       []UnjudgedMessage
	UnjudgedCounts map[UnjudgedReason]int
}

// IncompleteThread is a thread a run reported incomplete. ListRuns names it
// by its venue's kind and title (nil when the venue has none).
type IncompleteThread struct {
	Thread    threadctx.VenueID
	CutOff    time.Time
	Reason    IncompleteReason
	VenueKind string
	Title     *string
}

// JudgmentQuery selects judgments by thread, run or both.
type JudgmentQuery struct {
	Thread *threadctx.VenueID
	Run    *RunID
}

// JudgmentView is a judgment as the bench reads it. CutOff is its first
// appearance's, as of which its content is shown. WindowStart is the message
// key of the first message of the window it read: the context's first message
// for a judgment of a version before migration 099, which read the whole
// context (empty for a context with no message). NewMessages are the keys of
// the messages new at its cut-off inside its window, in thread order; Flags
// hold one decision per new message and flag kind, and Clearings one per
// commitment it re-checked. A judgment of a version before migration 099
// records none of them.
type JudgmentView struct {
	ID          uuid.UUID
	Thread      threadctx.VenueID
	CutOff      time.Time
	Runs        []RunAppearance
	Method      MethodRef
	Kind        threadctx.ContextKind
	InputHash   string
	JudgedAt    time.Time
	WindowStart string
	NewMessages []string
	Flags       []FlagDecision
	Clearings   []ClearingDecision
	Outcomes    []OutcomeView
}

// RunAppearance is a run a judgment appears in, at that run's cut-off.
type RunAppearance struct {
	Run    RunID
	CutOff time.Time
}

// OutcomeView is one outcome with its state and review. JudgmentID is the
// judgment holding it. QuestionID is the method's question ID, empty for an
// omission the store raised; Question is the question it answers.
type OutcomeView struct {
	ID         uuid.UUID
	JudgmentID uuid.UUID
	QuestionID string
	Question   Question
	ContactID  *uuid.UUID
	Outcome    string
	Confidence float64
	DueDate    *time.Time
	State      OutcomeState
	Review     *OutcomeReview
}

// ReviewRequest is the user's review of one outcome: the right outcome, and,
// for an open commitment, whether its obligation keeps its current date
// against the judged later one.
type ReviewRequest struct {
	RightOutcome    string
	KeepCurrentDate bool
}

// OutcomeReview is the user's review of an outcome. KeepsDate is set when the
// review kept an open commitment's current date.
type OutcomeReview struct {
	Verdict      Verdict
	RightOutcome string
	KeepsDate    bool
	ReviewedAt   time.Time
}

// Obligation is one obligation on a thread, open or closed: who owes (the user
// or the task's contact) what, and its one task.
type Obligation struct {
	TaskID    uuid.UUID
	ContactID uuid.UUID
	Debtor    string
	Basis     string
	Kind      string
	DueDate   time.Time
	Open      bool
}
