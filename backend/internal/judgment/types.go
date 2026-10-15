// Package judgment defines the decision-model record vocabulary.
package judgment

import (
	"errors"
	"fmt"
	"time"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	"personal-crm/backend/internal/threadctx"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Cadence exposes the deterministic correspondence state read by judgments.
type Cadence interface {
	WatchdogConfig() config.WatchdogConfig
}

type StoreDeps struct {
	Pool             *pgxpool.Pool
	Queries          *db.Queries
	Assert           *service.AssertService
	Contexts         *threadctx.Builder
	Cadence          Cadence
	Jobs             repository.JobEnqueuer
	Watchdog         config.WatchdogConfig
	ProductionMethod string
}

type Store struct {
	deps StoreDeps
}

// NewStore requires every dependency used by the persistence and apply paths.
func NewStore(d StoreDeps) *Store {
	for _, missing := range []struct {
		name string
		ok   bool
	}{
		{"Pool", d.Pool != nil},
		{"Queries", d.Queries != nil},
		{"Assert", d.Assert != nil},
		{"Contexts", d.Contexts != nil},
		{"Cadence", d.Cadence != nil},
		{"Jobs", d.Jobs != nil},
	} {
		if !missing.ok {
			panic(fmt.Sprintf("judgment.NewStore: missing %s", missing.name))
		}
	}
	return &Store{deps: d}
}

func (s *Store) ProductionMethod() (string, bool) {
	return s.deps.ProductionMethod, s.deps.ProductionMethod != ""
}

var (
	ErrNotFound            = errors.New("judgment outcome not found")
	ErrInvalidRightOutcome = errors.New("invalid judgment right outcome")
	ErrUnboundedQuery      = errors.New("judgment query must name a thread or run")
)

// Question identifiers and their allowed outcome values form the judgment catalog.
const (
	QuestionBallInCourt      = "ball_in_court"
	QuestionCommitmentMine   = "commitment_mine"
	QuestionCommitmentTheirs = "commitment_theirs"
	QuestionBigMoment        = "big_moment"
)

type BallInCourt string

const (
	IOweReply     BallInCourt = "i_owe_reply"
	TheyOweReply  BallInCourt = "they_owe_reply"
	NobodyOwes    BallInCourt = "nobody_owes"
	NotApplicable BallInCourt = "not_applicable"
)

type CommitmentState string

const (
	CommitmentOpen CommitmentState = "open"
	CommitmentNone CommitmentState = "none"
)

type CommitmentDirection string

const (
	CommitmentMine   CommitmentDirection = "mine"
	CommitmentTheirs CommitmentDirection = "theirs"
)

type BigMoment string

const (
	NoBigMoment       BigMoment = "none"
	ParticipantMoment BigMoment = "participant_moment"
	OtherPersonMoment BigMoment = "other_person_moment"
)

type RunKind string

const (
	RunScheduled  RunKind = "scheduled"
	RunHistorical RunKind = "historical"
	RunRerun      RunKind = "rerun"
)

type IncompleteReason string

const (
	IncompleteOverLimit    IncompleteReason = "over_limit"
	IncompleteCallFailed   IncompleteReason = "call_failed"
	IncompleteInputChanged IncompleteReason = "input_changed"
)

type OutcomeState string

const (
	OutcomeApplied  OutcomeState = "applied"
	OutcomeClaim    OutcomeState = "claim"
	OutcomeEvidence OutcomeState = "evidence"
)

type Verdict string

const (
	VerdictConfirmed Verdict = "confirmed"
	VerdictRejected  Verdict = "rejected"
	VerdictCorrected Verdict = "corrected"
)

type RunID = uuid.UUID

type ExampleID string

type MethodRef struct {
	Key            string
	Version        string
	WordingVersion string
}

type Usage struct {
	ContextTokens int
	Questions     int
	BilledTokens  *int
	CostUSD       *float64
	Latency       time.Duration
}

type AnsweredQuestion struct {
	ID             string
	Probabilities  map[string]float64
	OriginalAnswer string
}

type ParticipantOutcome[T comparable] struct {
	QuestionID string
	ContactID  uuid.UUID
	Outcome    T
	Confidence float64
}

type CommitmentOutcome struct {
	QuestionID string
	ContactID  uuid.UUID
	Direction  CommitmentDirection
	Outcome    CommitmentState
	Confidence float64
	DueDate    *time.Time
}

type Scored[T comparable] struct {
	QuestionID string
	Outcome    T
	Confidence float64
}

type Outcomes struct {
	BallInCourt []ParticipantOutcome[BallInCourt]
	Commitments []CommitmentOutcome
	BigMoment   Scored[BigMoment]
}

type Judgment struct {
	Method    MethodRef
	Thread    threadctx.VenueID
	Kind      threadctx.ContextKind
	CutOff    time.Time
	JudgedAt  time.Time
	InputHash string
	Questions []AnsweredQuestion
	Outcomes  Outcomes
	Usage     Usage
}

type RunSpec struct {
	Kind   RunKind
	Method MethodRef
	From   *time.Time
	To     *time.Time
}

type Effects struct {
	ObligationsOpened int
	FlagsRaised       int
	ClaimsRaised      int
}

type ThreadCutOff struct {
	Thread threadctx.VenueID
	CutOff time.Time
}

type Example struct {
	ID        ExampleID
	Thread    threadctx.VenueID
	CutOff    time.Time
	InputHash string
	Reviews   []Review
}

type Review struct {
	Question     string
	ContactID    *uuid.UUID
	RightOutcome string
}

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
	Effects    Effects
	CostUSD    *float64
}

type QuestionOutcome struct {
	QuestionID string
	Outcome    string
	Confidence float64
}

type JudgmentRecord struct {
	Example   ExampleID
	Method    MethodRef
	CutOff    time.Time
	JudgedAt  time.Time
	Questions []AnsweredQuestion
	Outcomes  []QuestionOutcome
	Usage     Usage
}

type ReviewRecord struct {
	Example      ExampleID
	Method       MethodRef
	QuestionID   string
	Verdict      Verdict
	RightOutcome string
	ReviewedAt   time.Time
}

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
}

type IncompleteThread struct {
	Thread threadctx.VenueID
	CutOff time.Time
	Reason IncompleteReason
}

type JudgmentQuery struct {
	Thread *threadctx.VenueID
	Run    *RunID
}

type JudgmentView struct {
	ID       uuid.UUID
	Thread   threadctx.VenueID
	CutOff   time.Time
	Runs     []RunAppearance
	Method   MethodRef
	Kind     threadctx.ContextKind
	Outcomes []OutcomeView
}

type RunAppearance struct {
	Run    RunID
	CutOff time.Time
}

type OutcomeView struct {
	ID         uuid.UUID
	QuestionID string
	ContactID  *uuid.UUID
	Outcome    string
	Confidence float64
	DueDate    *time.Time
	State      OutcomeState
	Review     *OutcomeReview
}

type OutcomeReview struct {
	Verdict      Verdict
	RightOutcome string
	ReviewedAt   time.Time
}
