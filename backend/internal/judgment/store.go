package judgment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	"personal-crm/backend/internal/threadctx"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Asserter is the assertion write path the store records claims, reviews and
// flags through. *service.AssertService is the one implementation.
type Asserter interface {
	AssertTx(ctx context.Context, tx pgx.Tx, req service.AssertRequest) (*repository.Assertion, error)
	AcceptTx(ctx context.Context, tx pgx.Tx, assertionID uuid.UUID) (*repository.Assertion, error)
	RejectTx(ctx context.Context, tx pgx.Tx, assertionID uuid.UUID) (*repository.Assertion, error)
	RetractTx(ctx context.Context, tx pgx.Tx, assertionID uuid.UUID) (*repository.Assertion, error)
}

// StoreDeps are the store's dependencies. Pool, Queries, Assert, Contexts,
// Cadence and Jobs are required; an empty ProductionMethod designates no
// production method, so no judgment changes live state.
type StoreDeps struct {
	Pool             *pgxpool.Pool
	Queries          *db.Queries
	Assert           Asserter
	Contexts         *threadctx.Builder
	Cadence          Cadence
	Jobs             repository.JobEnqueuer
	Watchdog         config.WatchdogConfig
	ProductionMethod string
}

// Store is the one writer of judgments, runs, claims, reviews and obligations.
type Store struct {
	pool       *pgxpool.Pool
	queries    *db.Queries
	assert     Asserter
	contexts   *threadctx.Builder
	cadence    Cadence
	jobs       repository.JobEnqueuer
	watchdog   config.WatchdogConfig
	production string
	tasks      *repository.ContactTaskRepository
	contacts   *repository.ContactRepository
}

// NewStore builds a Store, panicking with the name of each missing required
// dependency.
func NewStore(d StoreDeps) *Store {
	var missing []string
	for _, dep := range []struct {
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
		if !dep.ok {
			missing = append(missing, dep.name)
		}
	}
	if len(missing) > 0 {
		panic(fmt.Sprintf("judgment.NewStore: missing required deps: %v", missing))
	}
	return &Store{
		pool:       d.Pool,
		queries:    d.Queries,
		assert:     d.Assert,
		contexts:   d.Contexts,
		cadence:    d.Cadence,
		jobs:       d.Jobs,
		watchdog:   d.Watchdog,
		production: d.ProductionMethod,
		tasks:      repository.NewContactTaskRepository(d.Queries),
		contacts:   repository.NewContactRepository(d.Queries),
	}
}

// ProductionMethod returns the designated production method's key, and false
// when none is designated.
func (s *Store) ProductionMethod() (string, bool) {
	return s.production, s.production != ""
}

// StartRun starts a run of one method version.
func (s *Store) StartRun(ctx context.Context, spec RunSpec) (RunID, error) {
	switch spec.Kind {
	case RunKindScheduled, RunKindHistorical, RunKindRerun:
	default:
		return uuid.Nil, fmt.Errorf("start run: unknown run kind %q", spec.Kind)
	}
	if err := validateMethod(spec.Method); err != nil {
		return uuid.Nil, fmt.Errorf("start run: %w", err)
	}
	run, err := s.queries.CreateJudgmentRun(ctx, db.CreateJudgmentRunParams{
		Kind:           string(spec.Kind),
		MethodKey:      spec.Method.Key,
		MethodVersion:  spec.Method.Version,
		WordingVersion: spec.Method.WordingVersion,
		WindowFrom:     spec.From,
		WindowTo:       spec.To,
		StartedAt:      now(),
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("start run: %w", err)
	}
	return run.ID, nil
}

// FinishRun marks a run finished.
func (s *Store) FinishRun(ctx context.Context, run RunID) error {
	finishedAt := now()
	rows, err := s.queries.FinishJudgmentRun(ctx, db.FinishJudgmentRunParams{ID: run, FinishedAt: &finishedAt})
	if err != nil {
		return fmt.Errorf("finish run: %w", err)
	}
	if rows == 0 {
		if _, err := s.queries.GetJudgmentRun(ctx, run); err != nil {
			return fmt.Errorf("finish run %s: %w", run, err)
		}
	}
	return nil
}

// RecordIncomplete reports a thread the run could not judge at a cut-off.
func (s *Store) RecordIncomplete(ctx context.Context, run RunID, thread threadctx.VenueID, cutOff time.Time, reason IncompleteReason) error {
	switch reason {
	case IncompleteReasonOverLimit, IncompleteReasonCallFailed, IncompleteReasonInputChanged:
	default:
		return fmt.Errorf("record incomplete: unknown reason %q", reason)
	}
	if err := s.queries.CreateJudgmentIncomplete(ctx, db.CreateJudgmentIncompleteParams{
		RunID: run, ThreadID: thread, CutOff: cutOff, Reason: string(reason),
	}); err != nil {
		return fmt.Errorf("record incomplete: %w", err)
	}
	return nil
}

// RecordJudgment records a judgment in a run and gives it the live effects the
// run gives it. A judgment whose identity (method, thread, input hash) the
// store already holds is recorded in the run as ReuseJudgment records it.
func (s *Store) RecordJudgment(ctx context.Context, run RunID, j Judgment) (Effects, error) {
	if err := validateJudgment(j); err != nil {
		return Effects{}, fmt.Errorf("record judgment: %w", err)
	}
	var effects Effects
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		q := db.New(tx)
		runRow, err := q.GetJudgmentRun(ctx, run)
		if err != nil {
			return fmt.Errorf("get run %s: %w", run, err)
		}
		if runMethod(runRow) != j.Method {
			return fmt.Errorf("judgment method %+v is not run %s's method", j.Method, run)
		}
		judgmentID, found, err := findJudgment(ctx, q, j.Method, j.Thread, j.InputHash)
		if err != nil {
			return err
		}
		fresh := !found
		if fresh {
			if judgmentID, err = s.createJudgmentTx(ctx, tx, j); err != nil {
				return err
			}
		}
		effects, err = s.appearTx(ctx, tx, runRow, judgmentID, j.Thread, j.CutOff, fresh)
		return err
	})
	return effects, err
}

// ReuseJudgment records in the run, at cutOff, the judgment the run's method
// version holds for the thread and input hash, and gives it the live effects
// the run gives it. It returns false and records nothing when there is none.
func (s *Store) ReuseJudgment(ctx context.Context, run RunID, thread threadctx.VenueID, cutOff time.Time, inputHash string) (Effects, bool, error) {
	var (
		effects Effects
		found   bool
	)
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		q := db.New(tx)
		runRow, err := q.GetJudgmentRun(ctx, run)
		if err != nil {
			return fmt.Errorf("get run %s: %w", run, err)
		}
		var judgmentID uuid.UUID
		judgmentID, found, err = findJudgment(ctx, q, runMethod(runRow), thread, inputHash)
		if err != nil || !found {
			return err
		}
		effects, err = s.appearTx(ctx, tx, runRow, judgmentID, thread, cutOff, false)
		return err
	})
	return effects, found, err
}

// appearTx records the judgment's appearance in the run at cutOff and, when
// that appearance can change live state, lands the judgment's outcomes.
// Recording the same appearance twice changes nothing.
func (s *Store) appearTx(ctx context.Context, tx pgx.Tx, run *db.JudgmentRun, judgmentID uuid.UUID, thread threadctx.VenueID, cutOff time.Time, fresh bool) (Effects, error) {
	q := db.New(tx)
	added, err := q.CreateJudgmentAppearance(ctx, db.CreateJudgmentAppearanceParams{
		RunID: run.ID, JudgmentID: judgmentID, CutOff: cutOff, Fresh: fresh,
	})
	if err != nil {
		return Effects{}, fmt.Errorf("record appearance: %w", err)
	}
	if added == 0 {
		return Effects{}, nil
	}
	live, err := s.changesLiveStateTx(ctx, tx, run, thread, cutOff)
	if err != nil || !live {
		return Effects{}, err
	}
	effects, err := s.landTx(ctx, tx, judgmentID, thread, cutOff)
	if err != nil {
		return Effects{}, err
	}
	if err := q.SetJudgmentAppearanceEffects(ctx, db.SetJudgmentAppearanceEffectsParams{
		RunID:             run.ID,
		JudgmentID:        judgmentID,
		CutOff:            cutOff,
		ObligationsOpened: int32(effects.ObligationsOpened), //nolint:gosec // a handful per judgment
		FlagsRaised:       int32(effects.FlagsRaised),       //nolint:gosec // a handful per judgment
		ClaimsRaised:      int32(effects.ClaimsRaised),      //nolint:gosec // a handful per judgment
	}); err != nil {
		return Effects{}, fmt.Errorf("record appearance effects: %w", err)
	}
	return effects, nil
}

// changesLiveStateTx reports whether an appearance in run at cutOff can change
// live state: only a scheduled or historical run of the designated production
// method, at a cut-off at or after the thread's latest content (a current
// judgment), read through the transaction.
func (s *Store) changesLiveStateTx(ctx context.Context, tx pgx.Tx, run *db.JudgmentRun, thread threadctx.VenueID, cutOff time.Time) (bool, error) {
	production, ok := s.ProductionMethod()
	if !ok || run.MethodKey != production {
		return false, nil
	}
	if RunKind(run.Kind) != RunKindScheduled && RunKind(run.Kind) != RunKindHistorical {
		return false, nil
	}
	latest, hasContent, err := s.contexts.WithTx(tx).LatestContentAt(ctx, thread, now())
	if err != nil {
		return false, fmt.Errorf("latest content of thread %s: %w", thread, err)
	}
	return !hasContent || !cutOff.Before(latest), nil
}

// storedQuestion is an answered question as the judgment row stores it.
type storedQuestion struct {
	ID             string             `json:"id"`
	Probabilities  map[string]float64 `json:"probabilities"`
	OriginalAnswer string             `json:"original_answer"`
}

// createJudgmentTx stores a new judgment: its example, its record, and one
// outcome node per outcome carrying the method's answer.
func (s *Store) createJudgmentTx(ctx context.Context, tx pgx.Tx, j Judgment) (uuid.UUID, error) {
	q := db.New(tx)
	example, err := q.EnsureJudgmentExample(ctx, db.EnsureJudgmentExampleParams{ThreadID: j.Thread, InputHash: j.InputHash})
	if err != nil {
		return uuid.Nil, fmt.Errorf("ensure example: %w", err)
	}
	questions := make([]storedQuestion, len(j.Questions))
	for i, aq := range j.Questions {
		questions[i] = storedQuestion(aq)
	}
	questionsJSON, err := json.Marshal(questions)
	if err != nil {
		return uuid.Nil, fmt.Errorf("encode questions: %w", err)
	}
	var billed *int32
	if j.Usage.BilledTokens != nil {
		v := int32(*j.Usage.BilledTokens) //nolint:gosec // token counts fit int32
		billed = &v
	}
	id, err := q.CreateJudgment(ctx, db.CreateJudgmentParams{
		MethodKey:      j.Method.Key,
		MethodVersion:  j.Method.Version,
		WordingVersion: j.Method.WordingVersion,
		ExampleID:      example.ID,
		ContextKind:    string(j.Kind),
		CutOff:         j.CutOff,
		JudgedAt:       j.JudgedAt,
		ContextTokens:  int32(j.Usage.ContextTokens), //nolint:gosec // token counts fit int32
		QuestionCount:  int32(j.Usage.Questions),     //nolint:gosec // question counts fit int32
		BilledTokens:   billed,
		CostUsd:        j.Usage.CostUSD,
		LatencyNs:      int64(j.Usage.Latency),
		Questions:      questionsJSON,
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("create judgment: %w", err)
	}
	locator := judgmentLocator(id, j.Method, j.InputHash)
	for _, o := range flattenOutcomes(j.Outcomes) {
		outcome, err := q.CreateJudgmentOutcome(ctx, db.CreateJudgmentOutcomeParams{
			JudgmentID: id,
			Question:   string(o.question),
			QuestionID: &o.questionID,
			ContactID:  o.contact,
			DueDate:    o.due,
			Confidence: &o.confidence,
		})
		if err != nil {
			return uuid.Nil, fmt.Errorf("create outcome: %w", err)
		}
		value := o.value
		if _, err := s.assert.AssertTx(ctx, tx, service.AssertRequest{
			SubjectNodeID: outcome.NodeID,
			PredicateKey:  predicateAnswer,
			ValueText:     &value,
			Confidence:    percent(o.confidence),
			Locators:      []service.ProvenanceLocator{locator},
		}); err != nil {
			return uuid.Nil, fmt.Errorf("assert answer: %w", err)
		}
	}
	return id, nil
}

// flatOutcome is one typed outcome flattened to its question, contact and
// value.
type flatOutcome struct {
	question   Question
	questionID string
	contact    *uuid.UUID
	value      string
	confidence float64
	due        *time.Time
}

func flattenOutcomes(o Outcomes) []flatOutcome {
	var out []flatOutcome
	for _, b := range o.BallInCourt {
		contact := b.ContactID
		out = append(out, flatOutcome{QuestionBallInCourt, b.QuestionID, &contact, string(b.Outcome), b.Confidence, nil})
	}
	for _, c := range o.Commitments {
		contact := c.ContactID
		var due *time.Time
		if c.DueDate != nil {
			d := calendarDate(*c.DueDate)
			due = &d
		}
		out = append(out, flatOutcome{commitmentQuestion(c.Direction), c.QuestionID, &contact, string(c.Outcome), c.Confidence, due})
	}
	if o.BigMoment.Outcome != "" {
		out = append(out, flatOutcome{QuestionBigMoment, o.BigMoment.QuestionID, nil, string(o.BigMoment.Outcome), o.BigMoment.Confidence, nil})
	}
	return out
}

// judgmentLocator is a model answer's provenance: the judgment record, the
// method version and the input hash.
func judgmentLocator(judgmentID uuid.UUID, m MethodRef, inputHash string) service.ProvenanceLocator {
	return service.ProvenanceLocator{
		SourceKind:      repository.SourceKindJudgment,
		SourceID:        judgmentID.String(),
		ProducerKind:    repository.ProducerKindExtractor,
		ProducerVersion: m.Key + "@" + m.Version + "+" + m.WordingVersion,
		InputHash:       inputHash,
	}
}

// userLocator is the provenance of an act of the user's.
func userLocator() service.ProvenanceLocator {
	return service.ProvenanceLocator{
		SourceKind:   repository.SourceKindUser,
		SourceID:     uuid.NewString(),
		ProducerKind: repository.ProducerKindUser,
	}
}

func findJudgment(ctx context.Context, q *db.Queries, m MethodRef, thread threadctx.VenueID, inputHash string) (uuid.UUID, bool, error) {
	id, err := q.FindJudgment(ctx, db.FindJudgmentParams{
		MethodKey: m.Key, MethodVersion: m.Version, WordingVersion: m.WordingVersion,
		ThreadID: thread, InputHash: inputHash,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("find judgment: %w", err)
	}
	return id, true, nil
}

func runMethod(run *db.JudgmentRun) MethodRef {
	return MethodRef{Key: run.MethodKey, Version: run.MethodVersion, WordingVersion: run.WordingVersion}
}

func validateMethod(m MethodRef) error {
	if m.Key == "" || m.Version == "" {
		return errors.New("method key and version are required")
	}
	return nil
}

func validateJudgment(j Judgment) error {
	if err := validateMethod(j.Method); err != nil {
		return err
	}
	if j.Thread == uuid.Nil || j.InputHash == "" {
		return errors.New("thread and input hash are required")
	}
	if j.Kind != threadctx.KindConversation && j.Kind != threadctx.KindMeeting {
		return fmt.Errorf("unknown context kind %q", j.Kind)
	}
	for _, o := range flattenOutcomes(j.Outcomes) {
		if !isOutcomeValue(o.question, o.value) {
			return fmt.Errorf("%s outcome %q is not one of its values", o.question, o.value)
		}
		if o.question != QuestionBigMoment && *o.contact == uuid.Nil {
			return fmt.Errorf("%s outcome names no contact", o.question)
		}
		if o.confidence < 0 || o.confidence > 1 {
			return fmt.Errorf("%s outcome confidence %v is outside [0, 1]", o.question, o.confidence)
		}
	}
	for _, c := range j.Outcomes.Commitments {
		if c.Direction != CommitmentDirectionMine && c.Direction != CommitmentDirectionTheirs {
			return fmt.Errorf("unknown commitment direction %q", c.Direction)
		}
	}
	return nil
}

// inTx runs fn in one transaction and commits it.
func (s *Store) inTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	return pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{}, fn)
}

func now() time.Time { return accelerated.GetCurrentTime().UTC() }

// calendarDate is t's date in its own location, at UTC midnight, the way a
// DATE column decodes.
func calendarDate(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}
