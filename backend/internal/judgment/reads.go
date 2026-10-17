package judgment

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/threadctx"

	"github.com/google/uuid"
)

// ListRuns returns every run, oldest first, with its judged count (reused
// appearances included) and the threads it reported incomplete.
func (s *Store) ListRuns(ctx context.Context) ([]RunSummary, error) {
	rows, err := s.queries.ListJudgmentRunSummaries(ctx, false)
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	incomplete, err := s.incompleteByRun(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.nameIncomplete(ctx, incomplete); err != nil {
		return nil, err
	}
	runs := make([]RunSummary, len(rows))
	for i, r := range rows {
		runs[i] = RunSummary{
			ID:          r.ID,
			Kind:        RunKind(r.Kind),
			Method:      MethodRef{r.MethodKey, r.MethodVersion, r.WordingVersion},
			From:        r.WindowFrom,
			To:          r.WindowTo,
			StartedAt:   r.StartedAt,
			FinishedAt:  r.FinishedAt,
			JudgedCount: int(r.JudgedCount),
			Incomplete:  emptyIfNil(incomplete[r.ID]),
		}
	}
	return runs, nil
}

// FinishedRuns returns each finished run's content-free record.
func (s *Store) FinishedRuns(ctx context.Context) ([]RunRecord, error) {
	rows, err := s.queries.ListJudgmentRunSummaries(ctx, true)
	if err != nil {
		return nil, fmt.Errorf("list finished runs: %w", err)
	}
	incomplete, err := s.incompleteByRun(ctx)
	if err != nil {
		return nil, err
	}
	records := make([]RunRecord, len(rows))
	for i, r := range rows {
		records[i] = runRecord(r, incomplete[r.ID])
	}
	return records, nil
}

// RunExport is one run's content-free records: the judgments its calls
// produced, in the order it recorded them (not those it reused, which another
// run produced), and its record once it finished (nil until then).
type RunExport struct {
	Run       RunID
	Judgments []JudgmentRecord
	Finished  *RunRecord
}

// RunExports returns every run's content-free records, oldest run first, so
// every judgment is in exactly one run's export, finished or not. Runs are
// read before judgments, so a run read as finished has all its judgments
// listed.
func (s *Store) RunExports(ctx context.Context) ([]RunExport, error) {
	rows, err := s.queries.ListJudgmentRunSummaries(ctx, false)
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	incomplete, err := s.incompleteByRun(ctx)
	if err != nil {
		return nil, err
	}
	appearances, err := s.queries.ListJudgmentAppearances(ctx, db.ListJudgmentAppearancesParams{JudgmentIds: []uuid.UUID{}, FreshOnly: true})
	if err != nil {
		return nil, fmt.Errorf("list fresh appearances: %w", err)
	}
	records, err := s.judgmentRecords(ctx, appearances)
	if err != nil {
		return nil, err
	}
	byRun := map[RunID][]JudgmentRecord{}
	for i, a := range appearances {
		byRun[a.RunID] = append(byRun[a.RunID], records[i])
	}
	exports := make([]RunExport, len(rows))
	for i, r := range rows {
		exports[i] = RunExport{Run: r.ID, Judgments: byRun[r.ID]}
		if r.FinishedAt != nil {
			record := runRecord(r, incomplete[r.ID])
			exports[i].Finished = &record
		}
	}
	return exports, nil
}

// runRecord is a finished run's content-free record.
func runRecord(r *db.ListJudgmentRunSummariesRow, incomplete []IncompleteThread) RunRecord {
	counts := map[IncompleteReason]int{}
	for _, t := range incomplete {
		counts[t.Reason]++
	}
	var cost *float64
	if r.CostReported {
		c := r.CostUsd
		cost = &c
	}
	return RunRecord{
		Run:        r.ID,
		Kind:       RunKind(r.Kind),
		Method:     MethodRef{r.MethodKey, r.MethodVersion, r.WordingVersion},
		From:       r.WindowFrom,
		To:         r.WindowTo,
		StartedAt:  r.StartedAt,
		FinishedAt: *r.FinishedAt,
		Judged:     int(r.JudgedCount),
		Incomplete: counts,
		Effects: Effects{
			ObligationsOpened: int(r.ObligationsOpened),
			FlagsRaised:       int(r.FlagsRaised),
			ClaimsRaised:      int(r.ClaimsRaised),
		},
		CostUSD: cost,
	}
}

func (s *Store) incompleteByRun(ctx context.Context) (map[RunID][]IncompleteThread, error) {
	rows, err := s.queries.ListJudgmentIncomplete(ctx)
	if err != nil {
		return nil, fmt.Errorf("list incomplete threads: %w", err)
	}
	byRun := map[RunID][]IncompleteThread{}
	for _, r := range rows {
		byRun[r.RunID] = append(byRun[r.RunID], IncompleteThread{Thread: r.ThreadID, CutOff: r.CutOff, Reason: IncompleteReason(r.Reason)})
	}
	return byRun, nil
}

// nameIncomplete sets each incomplete thread's venue kind and title.
func (s *Store) nameIncomplete(ctx context.Context, byRun map[RunID][]IncompleteThread) error {
	venues := repository.NewVenueRepository(s.queries)
	named := map[threadctx.VenueID]*repository.Venue{}
	for _, threads := range byRun {
		for i := range threads {
			t := &threads[i]
			venue, ok := named[t.Thread]
			if !ok {
				v, err := venues.GetVenue(ctx, t.Thread)
				if err != nil {
					return fmt.Errorf("get incomplete thread %s: %w", t.Thread, err)
				}
				venue, named[t.Thread] = v, v
			}
			t.VenueKind, t.Title = venue.Kind, venue.Title
		}
	}
	return nil
}

// ListJudgments returns the judgments of a thread, of a run (every judgment
// appearing in it, reused ones included) or both, each with every run it
// appears in and its outcomes' states and reviews.
func (s *Store) ListJudgments(ctx context.Context, query JudgmentQuery) ([]JudgmentView, error) {
	if query.Thread == nil && query.Run == nil {
		return nil, ErrUnboundedQuery
	}
	rows, err := s.queries.ListJudgmentRows(ctx, db.ListJudgmentRowsParams{
		Ids: []uuid.UUID{}, ThreadID: query.Thread, RunID: query.Run, ExampleIds: []string{},
	})
	if err != nil {
		return nil, fmt.Errorf("list judgments: %w", err)
	}
	ids := judgmentIDs(rows)
	appearances, err := s.appearances(ctx, ids, nil)
	if err != nil {
		return nil, err
	}
	outcomes, err := s.outcomesByJudgment(ctx, ids)
	if err != nil {
		return nil, err
	}
	views := make([]JudgmentView, len(rows))
	for i, j := range rows {
		var runs []RunAppearance
		for _, a := range appearances[j.ID] {
			runs = append(runs, RunAppearance{Run: a.RunID, CutOff: a.CutOff})
		}
		var outcomeViews []OutcomeView
		for _, o := range outcomes[j.ID] {
			outcomeViews = append(outcomeViews, o.view())
		}
		views[i] = JudgmentView{
			ID:        j.ID,
			Thread:    j.ThreadID,
			CutOff:    j.CutOff,
			Runs:      emptyIfNil(runs),
			Method:    MethodRef{j.MethodKey, j.MethodVersion, j.WordingVersion},
			Kind:      threadctx.ContextKind(j.ContextKind),
			InputHash: j.InputHash,
			JudgedAt:  j.JudgedAt,
			Outcomes:  emptyIfNil(outcomeViews),
		}
	}
	return views, nil
}

// RunJudgmentCutOffs returns, for each judgment appearing in a run (a reused
// one included), its thread and the judgment's own cut-off: the cut-off its
// proposal carries and is placed at, which for a reused judgment precedes the
// run's.
func (s *Store) RunJudgmentCutOffs(ctx context.Context, run RunID) ([]ThreadCutOff, error) {
	rows, err := s.queries.ListJudgmentRows(ctx, db.ListJudgmentRowsParams{Ids: []uuid.UUID{}, RunID: &run, ExampleIds: []string{}})
	if err != nil {
		return nil, fmt.Errorf("list run judgments: %w", err)
	}
	cutOffs := make([]ThreadCutOff, len(rows))
	for i, r := range rows {
		cutOffs[i] = ThreadCutOff{Thread: r.ThreadID, CutOff: r.CutOff}
	}
	return cutOffs, nil
}

// ThreadsJudgedInRun returns each thread a run judged, at the run's cut-off,
// reused appearances included.
func (s *Store) ThreadsJudgedInRun(ctx context.Context, run RunID) ([]ThreadCutOff, error) {
	rows, err := s.queries.ListJudgmentAppearances(ctx, db.ListJudgmentAppearancesParams{JudgmentIds: []uuid.UUID{}, RunID: &run})
	if err != nil {
		return nil, fmt.Errorf("list run appearances: %w", err)
	}
	threads := make([]ThreadCutOff, len(rows))
	for i, r := range rows {
		threads[i] = ThreadCutOff{Thread: r.ThreadID, CutOff: r.CutOff}
	}
	return threads, nil
}

// ReviewedExamples returns each input the user reviewed, with the user's
// latest right outcome per question and contact.
func (s *Store) ReviewedExamples(ctx context.Context) ([]Example, error) {
	rows, err := s.queries.ListLatestExampleReviews(ctx, db.ListLatestExampleReviewsParams{
		ReviewPredicate: predicateReview, ExampleIds: []string{},
	})
	if err != nil {
		return nil, fmt.Errorf("list reviewed examples: %w", err)
	}
	var examples []Example
	for _, r := range rows {
		if r.RightOutcome == nil {
			continue
		}
		if len(examples) == 0 || examples[len(examples)-1].ID != ExampleID(r.ExampleID) {
			examples = append(examples, Example{ID: ExampleID(r.ExampleID), Thread: r.ThreadID, CutOff: r.CutOff, InputHash: r.InputHash})
		}
		e := &examples[len(examples)-1]
		e.Reviews = append(e.Reviews, Review{Question: r.Question, ContactID: r.ContactID, RightOutcome: *r.RightOutcome})
	}
	return emptyIfNil(examples), nil
}

// OutcomesFor returns one method version's outcomes on the given examples; an
// example the method did not judge is absent.
func (s *Store) OutcomesFor(ctx context.Context, method MethodRef, examples []ExampleID) (map[ExampleID]Outcomes, error) {
	ids := make([]string, len(examples))
	for i, e := range examples {
		ids[i] = string(e)
	}
	result := map[ExampleID]Outcomes{}
	if len(ids) == 0 {
		return result, nil
	}
	rows, err := s.queries.ListJudgmentRows(ctx, db.ListJudgmentRowsParams{
		Ids: []uuid.UUID{}, ExampleIds: ids,
		MethodKey: &method.Key, MethodVersion: &method.Version, WordingVersion: &method.WordingVersion,
	})
	if err != nil {
		return nil, fmt.Errorf("list method judgments: %w", err)
	}
	outcomes, err := s.outcomesByJudgment(ctx, judgmentIDs(rows))
	if err != nil {
		return nil, err
	}
	for _, j := range rows {
		result[ExampleID(j.ExampleID)] = typedOutcomes(outcomes[j.ID])
	}
	return result, nil
}

// JudgmentRecords returns, for each judgment appearing in a run, its
// content-free record at the run's cut-off, keyed by example ID and question
// ID.
func (s *Store) JudgmentRecords(ctx context.Context, run RunID) ([]JudgmentRecord, error) {
	appearances, err := s.queries.ListJudgmentAppearances(ctx, db.ListJudgmentAppearancesParams{JudgmentIds: []uuid.UUID{}, RunID: &run})
	if err != nil {
		return nil, fmt.Errorf("list run appearances: %w", err)
	}
	return s.judgmentRecords(ctx, appearances)
}

// judgmentRecords returns each appearance's judgment as a content-free record
// at the appearance's cut-off, in the appearances' order.
func (s *Store) judgmentRecords(ctx context.Context, appearances []*db.ListJudgmentAppearancesRow) ([]JudgmentRecord, error) {
	ids := make([]uuid.UUID, len(appearances))
	for i, a := range appearances {
		ids[i] = a.JudgmentID
	}
	byID := map[uuid.UUID]*db.ListJudgmentRowsRow{}
	if len(ids) > 0 {
		rows, err := s.queries.ListJudgmentRows(ctx, db.ListJudgmentRowsParams{Ids: ids, ExampleIds: []string{}})
		if err != nil {
			return nil, fmt.Errorf("list judgments: %w", err)
		}
		for _, r := range rows {
			byID[r.ID] = r
		}
	}
	outcomes, err := s.outcomesByJudgment(ctx, ids)
	if err != nil {
		return nil, err
	}
	records := make([]JudgmentRecord, len(appearances))
	for i, a := range appearances {
		j := byID[a.JudgmentID]
		questions, err := decodeQuestions(j.Questions)
		if err != nil {
			return nil, err
		}
		var questionOutcomes []QuestionOutcome
		for _, o := range outcomes[j.ID] {
			if o.QuestionID == "" {
				continue
			}
			questionOutcomes = append(questionOutcomes, QuestionOutcome{QuestionID: o.QuestionID, Outcome: o.value(), Confidence: o.confidence()})
		}
		records[i] = JudgmentRecord{
			Example:   ExampleID(j.ExampleID),
			Method:    MethodRef{j.MethodKey, j.MethodVersion, j.WordingVersion},
			CutOff:    a.CutOff,
			JudgedAt:  j.JudgedAt,
			Questions: questions,
			Outcomes:  emptyIfNil(questionOutcomes),
			Usage:     usageOf(j),
		}
	}
	return records, nil
}

// ReviewRecords returns each live review, content-free, with the reviewed
// outcome's judgment method, example and question.
func (s *Store) ReviewRecords(ctx context.Context) ([]ReviewRecord, error) {
	rows, err := s.queries.ListReviewRecords(ctx, db.ListReviewRecordsParams{
		AnswerPredicate: predicateAnswer, ClassPredicates: classPredicates, ReviewPredicate: predicateReview,
		KeepsDatePredicate: predicateReviewKeepsDate,
	})
	if err != nil {
		return nil, fmt.Errorf("list reviews: %w", err)
	}
	records := make([]ReviewRecord, 0, len(rows))
	for _, r := range rows {
		if r.RightOutcome == nil {
			continue
		}
		questionID := r.Question
		if r.QuestionID != nil {
			questionID = *r.QuestionID
		}
		records = append(records, ReviewRecord{
			Example:      ExampleID(r.ExampleID),
			Method:       MethodRef{r.MethodKey, r.MethodVersion, r.WordingVersion},
			QuestionID:   questionID,
			Verdict:      deriveVerdict(r.JudgedValue, *r.RightOutcome, r.KeepsDate),
			RightOutcome: *r.RightOutcome,
			ReviewedAt:   r.ReviewedAt,
		})
	}
	return records, nil
}

// Obligations returns every obligation on a thread, open or closed, oldest
// first.
func (s *Store) Obligations(ctx context.Context, thread threadctx.VenueID) ([]Obligation, error) {
	tasks, err := s.tasks.ListObligationTasksByThread(ctx, thread)
	if err != nil {
		return nil, fmt.Errorf("list obligations: %w", err)
	}
	obligations := make([]Obligation, len(tasks))
	for i := range tasks {
		t := &tasks[i]
		due, _ := time.Parse(dateFormat, dueOf(t))
		obligations[i] = Obligation{
			TaskID:    t.ID,
			ContactID: t.ContactID,
			Debtor:    t.Obligation.Debtor,
			Basis:     t.Obligation.Basis,
			Kind:      t.Kind,
			DueDate:   due,
			Open:      t.IsLive(),
		}
	}
	return obligations, nil
}

func (s *Store) appearances(ctx context.Context, judgmentIDs []uuid.UUID, run *RunID) (map[uuid.UUID][]*db.ListJudgmentAppearancesRow, error) {
	byJudgment := map[uuid.UUID][]*db.ListJudgmentAppearancesRow{}
	if len(judgmentIDs) == 0 {
		return byJudgment, nil
	}
	rows, err := s.queries.ListJudgmentAppearances(ctx, db.ListJudgmentAppearancesParams{JudgmentIds: judgmentIDs, RunID: run})
	if err != nil {
		return nil, fmt.Errorf("list appearances: %w", err)
	}
	for _, r := range rows {
		byJudgment[r.JudgmentID] = append(byJudgment[r.JudgmentID], r)
	}
	return byJudgment, nil
}

func (s *Store) outcomesByJudgment(ctx context.Context, judgmentIDs []uuid.UUID) (map[uuid.UUID][]*outcomeRecord, error) {
	byJudgment := map[uuid.UUID][]*outcomeRecord{}
	if len(judgmentIDs) == 0 {
		return byJudgment, nil
	}
	records, err := loadOutcomes(ctx, s.queries, judgmentIDs, nil)
	if err != nil {
		return nil, err
	}
	for _, r := range records {
		byJudgment[r.JudgmentID] = append(byJudgment[r.JudgmentID], r)
	}
	return byJudgment, nil
}

// typedOutcomes rebuilds a judgment's typed outcomes from its method's answers;
// omissions the store raised are not the method's and are left out.
func typedOutcomes(records []*outcomeRecord) Outcomes {
	var out Outcomes
	for _, o := range records {
		if o.QuestionID == "" {
			continue
		}
		switch o.Question {
		case QuestionBallInCourt:
			out.BallInCourt = append(out.BallInCourt, ParticipantOutcome[BallInCourt]{
				QuestionID: o.QuestionID, ContactID: contactOf(o.ContactID), Outcome: BallInCourt(o.value()), Confidence: o.confidence(),
			})
		case QuestionCommitmentMine, QuestionCommitmentTheirs:
			direction := CommitmentDirectionTheirs
			if o.Question == QuestionCommitmentMine {
				direction = CommitmentDirectionMine
			}
			out.Commitments = append(out.Commitments, CommitmentOutcome{
				QuestionID: o.QuestionID, ContactID: contactOf(o.ContactID), Direction: direction,
				Outcome: CommitmentState(o.value()), Confidence: o.confidence(), DueDate: o.DueDate,
			})
		case QuestionBigMoment:
			out.BigMoment = Scored[BigMoment]{QuestionID: o.QuestionID, Outcome: BigMoment(o.value()), Confidence: o.confidence()}
		}
	}
	return out
}

func decodeQuestions(raw []byte) ([]AnsweredQuestion, error) {
	var stored []storedQuestion
	if err := json.Unmarshal(raw, &stored); err != nil {
		return nil, fmt.Errorf("decode questions: %w", err)
	}
	questions := make([]AnsweredQuestion, len(stored))
	for i, q := range stored {
		questions[i] = AnsweredQuestion(q)
	}
	return questions, nil
}

func usageOf(j *db.ListJudgmentRowsRow) Usage {
	var billed *int
	if j.BilledTokens != nil {
		b := int(*j.BilledTokens)
		billed = &b
	}
	return Usage{
		ContextTokens: int(j.ContextTokens),
		Questions:     int(j.QuestionCount),
		BilledTokens:  billed,
		CostUSD:       j.CostUsd,
		Latency:       time.Duration(j.LatencyNs),
	}
}

func judgmentIDs(rows []*db.ListJudgmentRowsRow) []uuid.UUID {
	ids := make([]uuid.UUID, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	return ids
}
