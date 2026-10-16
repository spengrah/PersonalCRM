package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/consumer"
	"personal-crm/backend/internal/consumer/consumerjobs"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/decision"
	"personal-crm/backend/internal/judgerun"
	"personal-crm/backend/internal/judgment"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	"personal-crm/backend/internal/threadctx"

	"github.com/riverqueue/river"
)

// judgeMonthRunner is the narrow interface --judge-month needs. Production
// wires it to a *judgerun.Runner.
type judgeMonthRunner interface {
	Historical(ctx context.Context, methodKey string, month judgerun.Month) (judgerun.Summary, error)
	Measure(ctx context.Context, methodKey string, month judgerun.Month) (judgerun.Measurement, error)
}

// buildJudgeMonthRunner wires the historical-run stack: the thread-context
// builder, the masker loaded from the contact dictionary, the method registry
// and the one judgment store, with the production method
// DECISION_PRODUCTION_METHOD names. It fails when that setting names a key the
// registry lacks, before any call.
func buildJudgeMonthRunner(
	ctx context.Context,
	cfg *config.Config,
	database *db.Database,
	assert *service.AssertService,
	contacts *repository.ContactRepository,
	jobs repository.JobEnqueuer,
) (*judgerun.Runner, error) {
	content := repository.NewThreadContentRepository(database.Queries)
	contexts := threadctx.NewBuilder(content, repository.NewVenueRepository(database.Queries))
	masker, err := threadctx.LoadMasker(ctx, content)
	if err != nil {
		return nil, fmt.Errorf("load masker: %w", err)
	}
	cadence := consumer.NewCadenceUpdater(
		repository.NewEventConsumerClaimRepository(database.Queries),
		contacts,
		database.Queries,
		consumer.CadenceModeFromConfig(cfg.EventBus.CadenceMode),
		cfg.EventBus.UnsafeAllowOffMode,
		cfg.Watchdog,
	)
	store := judgment.NewStore(judgment.StoreDeps{
		Pool:             database.Pool,
		Queries:          database.Queries,
		Assert:           assert,
		Contexts:         contexts,
		Cadence:          cadence,
		Jobs:             jobs,
		Watchdog:         cfg.Watchdog,
		ProductionMethod: cfg.Decision.ProductionMethod,
	})
	return judgerun.New(contexts, store, decision.NewRegistry(cfg.Decision, masker))
}

// validateJudgeMonth checks the flags --judge-month reads.
func validateJudgeMonth(opts runOptions) error {
	if opts.judgeMonth == "" {
		if opts.judgeMethod != "" || opts.judgeMeasure {
			return errors.New("--method and --measure apply only to --judge-month")
		}
		return nil
	}
	if opts.judgeMethod == "" {
		return errors.New("--judge-month requires --method <key>")
	}
	_, err := judgerun.ParseMonth(opts.judgeMonth)
	return err
}

// runJudgeMonth runs a historical run over the month, or with --measure a
// measuring run that sends and writes nothing, and prints its content-free
// report: IDs, counts and reasons only.
func runJudgeMonth(ctx context.Context, opts runOptions, deps adminDeps) error {
	month, err := judgerun.ParseMonth(opts.judgeMonth)
	if err != nil {
		return err
	}
	if opts.judgeMeasure {
		m, err := deps.judgeMonth.Measure(ctx, opts.judgeMethod, month)
		if err != nil {
			return fmt.Errorf("measure %s: %w", month, err)
		}
		return writeMeasurement(deps.stdout, m)
	}
	s, err := deps.judgeMonth.Historical(ctx, opts.judgeMethod, month)
	if err != nil {
		return fmt.Errorf("judge %s: %w", month, err)
	}
	return writeRunSummary(deps.stdout, month, s)
}

func writeRunSummary(w io.Writer, month judgerun.Month, s judgerun.Summary) error {
	incomplete := 0
	for _, n := range s.Incomplete {
		incomplete += n
	}
	if _, err := fmt.Fprintf(w,
		"judge-month: month=%s run=%s kind=%s method=%s version=%s production=%t\n"+
			"  cut_offs=%d eligible=%d judged=%d reused=%d incomplete=%d\n",
		month, s.Run, s.Kind, s.Method.Key, s.Method.Version, s.Production,
		s.CutOffs, s.Eligible, s.Judged, s.Reused, incomplete); err != nil {
		return fmt.Errorf("write run summary: %w", err)
	}
	reasons := make([]string, 0, len(s.Incomplete))
	for reason := range s.Incomplete {
		reasons = append(reasons, string(reason))
	}
	sort.Strings(reasons)
	for _, reason := range reasons {
		if _, err := fmt.Fprintf(w, "  incomplete reason=%s n=%d\n", reason, s.Incomplete[judgment.IncompleteReason(reason)]); err != nil {
			return fmt.Errorf("write run summary: %w", err)
		}
	}
	if _, err := fmt.Fprintf(w, "  live_effects obligations_opened=%d flags_raised=%d claims_raised=%d\n",
		s.Effects.ObligationsOpened, s.Effects.FlagsRaised, s.Effects.ClaimsRaised); err != nil {
		return fmt.Errorf("write run summary: %w", err)
	}
	return nil
}

func writeMeasurement(w io.Writer, m judgerun.Measurement) error {
	if _, err := fmt.Fprintf(w,
		"judge-month --measure: month=%s method=%s version=%s (nothing sent, nothing written)\n"+
			"  cut_offs=%d contexts=%d\n"+
			"  tokens min=%d p50=%d p90=%d p99=%d max=%d limit=%d over_limit=%d\n"+
			"  questions min=%d p50=%d p90=%d p99=%d max=%d limit=%d over_limit=%d\n"+
			"  over_any_limit=%d\n",
		m.Month, m.Method.Key, m.Method.Version,
		m.CutOffs, m.Contexts,
		m.Tokens.Min, m.Tokens.P50, m.Tokens.P90, m.Tokens.P99, m.Tokens.Max, m.TokenLimit, m.OverTokenLimit,
		m.Questions.Min, m.Questions.P50, m.Questions.P90, m.Questions.P99, m.Questions.Max, m.QuestionLimit, m.OverQuestionLimit,
		m.OverLimit); err != nil {
		return fmt.Errorf("write measurement: %w", err)
	}
	return nil
}

// noopTodoistTaskOpWorker registers the todoist_task_op kind so the
// insert-only admin River client accepts the Todoist operations a historical
// run's live effects enqueue; the crm-api worker runs them.
type noopTodoistTaskOpWorker struct {
	river.WorkerDefaults[consumerjobs.TodoistTaskOpArgs]
}

func (w *noopTodoistTaskOpWorker) Work(_ context.Context, _ *river.Job[consumerjobs.TodoistTaskOpArgs]) error {
	return nil
}
