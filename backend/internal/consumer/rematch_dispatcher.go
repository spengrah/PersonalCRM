package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"personal-crm/backend/internal/consumer/consumerjobs"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/service"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
)

// rematchRunner is the subset of *service.RematchService the dispatcher
// needs. Interface defined here (not on the service) so unit tests can
// stub Run without instantiating the full service graph.
type rematchRunner interface {
	Run(ctx context.Context, jobID, contactID uuid.UUID, methods []service.Method) error
	// FailJob forces the in-memory job terminal. Needed because Run leaves a
	// budget-exhausted job Running across snoozes; when the worker stops
	// rescheduling, someone must terminate it or the frontend watcher polls
	// forever.
	FailJob(jobID uuid.UUID, reason error)
}

// RematchDispatcher is the event-bus consumer for contact_methods.added.
// It runs the rematch pipeline (per-method handler iteration +
// per-contact mutex serialization) via RematchService.Run. Always-on:
// a registered River worker returning nil on a kill-switch would
// permanently ack queued jobs, so rollback is `git revert` only.
type RematchDispatcher struct {
	runner rematchRunner
}

// NewRematchDispatcher constructs a dispatcher wired to the given
// runner (production: *service.RematchService).
func NewRematchDispatcher(runner rematchRunner) *RematchDispatcher {
	return &RematchDispatcher{runner: runner}
}

// HandleEvent decodes the contact_methods.added envelope and invokes
// the rematch runner. Does NOT accept a tx — unlike the other event
// consumers, the rematch pipeline reads/writes across multiple
// repositories (calendar_event, telegram_message, aggregation tables)
// and each handler owns its own tx scoping. The publisher's tx has
// already committed by the time River dequeues this worker.
func (d *RematchDispatcher) HandleEvent(ctx context.Context, env *events.Envelope) error {
	if env == nil {
		return errors.New("rematch_dispatcher: nil envelope")
	}
	if env.Kind != events.KindContactMethodsAdded {
		return fmt.Errorf("rematch_dispatcher: unexpected kind %q", env.Kind)
	}

	var p events.ContactMethodsAddedPayload
	if err := events.Unmarshal(env, &p); err != nil {
		return fmt.Errorf("unmarshal contact_methods.added: %w", err)
	}
	if p.RematchJobID == uuid.Nil {
		return errors.New("rematch_dispatcher: empty rematch_job_id in payload")
	}
	if p.ContactID == uuid.Nil {
		return errors.New("rematch_dispatcher: empty contact_id in payload")
	}
	if len(p.Methods) == 0 {
		// Empty methods is a publisher bug (the publisher is supposed to
		// skip publish when no methods diff). Retrying won't help, so
		// log and return nil so River marks the job done.
		logger.Warn().
			Str("event_id", env.ID.String()).
			Str("rematch_job_id", p.RematchJobID.String()).
			Msg("rematch_dispatcher: empty methods slice; no-op")
		return nil
	}

	methods := make([]service.Method, len(p.Methods))
	for i, m := range p.Methods {
		methods[i] = service.Method{Type: m.Type, Value: m.Value}
	}

	if err := d.runner.Run(ctx, p.RematchJobID, p.ContactID, methods); err != nil {
		// Run has marked the in-memory job Failed for a genuine error, but
		// deliberately leaves it Running for the continue-later sentinel
		// (service.ErrRematchBudgetExhausted). Propagate either way: Work
		// inspects the error and converts the sentinel into a snooze, while a
		// genuine error is retried per MaxAttempts (3) from InsertOpts.
		return fmt.Errorf("rematch run: %w", err)
	}
	return nil
}

// --------------------------------------------------------------------------
// River worker wrapper — matches interaction_recorder.go pattern.
// --------------------------------------------------------------------------

// RematchDispatcherWorker is the river worker that fetches the event
// by id and invokes RematchDispatcher.HandleEvent.
type RematchDispatcherWorker struct {
	river.WorkerDefaults[consumerjobs.RematchDispatcherJobArgs]
	bus        eventBusTx // reuse interface declared in interaction_recorder.go
	pool       *pgxpool.Pool
	dispatcher *RematchDispatcher
}

// NewRematchDispatcherWorker wires the worker to the concrete bus, the
// application pgxpool (currently unused — reserved for future tx
// scoping), and the consumer instance.
func NewRematchDispatcherWorker(bus eventBusTx, pool *pgxpool.Pool, dispatcher *RematchDispatcher) *RematchDispatcherWorker {
	return &RematchDispatcherWorker{bus: bus, pool: pool, dispatcher: dispatcher}
}

// rematchBudgetSnoozeDelay is how long a rematch job waits before resuming when
// a gchat rematch aborts on budget exhaustion. It is comfortably above the
// "within minutes" rapid-fire cadence that was discarding these jobs.
const rematchBudgetSnoozeDelay = 5 * time.Minute

// rematchBudgetSnoozeCeiling bounds how many times one rematch job may snooze on
// budget exhaustion before the worker gives up.
//
// A ceiling is required because a snoozed re-run is NOT guaranteed to make
// progress: the scan restarts from the account's static onboarding floor every
// attempt (gchat_rematch.go passes gchatBackfillFloor, which nothing advances)
// and ScanIdentifier persists no continuation cursor, so an account whose
// history exceeds the per-run page budget re-scans the same pages and exhausts
// again — deterministically, forever. Snoozing is only self-resolving for an
// account that is transiently over budget. Without this ceiling an over-budget
// account burns a scan every rematchBudgetSnoozeDelay indefinitely while its
// job shows as running.
//
// 12 snoozes ≈ 1 hour of retrying before giving up loudly. Persisting real
// per-rematch scan progress is the actual fix; until then this converts an
// infinite loop into a bounded one with a terminal, visible outcome.
const rematchBudgetSnoozeCeiling = 12

// Work implements river.Worker. Fetches the event envelope by id and
// invokes HandleEvent. Budget-exhaustion (service.ErrRematchBudgetExhausted) is
// a continue-later signal, not a terminal failure, so it is rescheduled via
// JobSnooze (which rewinds the attempt, so it never counts against MaxAttempts)
// rather than discarded — up to rematchBudgetSnoozeCeiling times, after which
// the job is cancelled terminally. Genuine errors propagate so river retries per
// MaxAttempts (3 from the InsertOpts set in events.consumerJobsForKind) and
// eventually discards.
func (w *RematchDispatcherWorker) Work(ctx context.Context, j *river.Job[consumerjobs.RematchDispatcherJobArgs]) error {
	env, err := w.bus.GetEvent(ctx, j.Args.EventID)
	if err != nil {
		return fmt.Errorf("fetch event %s: %w", j.Args.EventID, err)
	}
	if err := w.dispatcher.HandleEvent(ctx, env); err != nil {
		if errors.Is(err, service.ErrRematchBudgetExhausted) {
			if snoozes := riverSnoozeCount(j); snoozes >= rematchBudgetSnoozeCeiling {
				// Out of patience: this backfill has re-scanned from the same
				// floor rematchBudgetSnoozeCeiling times without completing, so
				// further snoozes would just repeat it. Push the in-memory job
				// terminal (Run leaves it Running across snoozes, so the
				// frontend watcher would otherwise poll forever) and cancel the
				// River job — JobCancel is immediately terminal, unlike a
				// returned error which would burn MaxAttempts on more doomed
				// scans.
				logger.Error().
					Str("event_id", env.ID.String()).
					Str("rematch_job_id", j.Args.RematchJobID.String()).
					Int("snoozes", snoozes).
					Msg("rematch_dispatcher: gchat rematch still budget-exhausted after ceiling; giving up (history likely exceeds per-run page budget)")
				w.dispatcher.runner.FailJob(j.Args.RematchJobID, err)
				return river.JobCancel(err)
			}
			// Snooze instead of returning a terminal error: three rapid retries
			// against the same still-exhausted budget would discard the job and
			// strand the contact's backfill. Snoozing never counts against
			// MaxAttempts, so a transiently-over-budget backfill resumes until
			// it completes. A genuine failure co-occurring with budget
			// exhaustion keeps snoozing until the budget stops exhausting, then
			// discards normally once it is the sole remaining error. Log at Warn
			// (river logs snoozes only at Debug) so a backfill stuck snoozing
			// stays visible; rematch_job_id + event_id attribute it without
			// logging contact PII.
			logger.Warn().
				Str("event_id", env.ID.String()).
				Str("rematch_job_id", j.Args.RematchJobID.String()).
				Dur("snooze", rematchBudgetSnoozeDelay).
				Msg("rematch_dispatcher: gchat rematch budget exhausted; snoozing to continue backfill")
			return river.JobSnooze(rematchBudgetSnoozeDelay)
		}
		return err
	}
	return nil
}

// riverSnoozeCount reads the snooze tally river maintains in job metadata
// ("snoozes", incremented on every JobSnooze). Returns 0 when the job row or
// metadata is absent or unparseable — a missing count must not be read as
// "ceiling reached", since that would cancel a first-attempt backfill.
func riverSnoozeCount(j *river.Job[consumerjobs.RematchDispatcherJobArgs]) int {
	if j.JobRow == nil || len(j.Metadata) == 0 {
		return 0
	}
	var meta struct {
		Snoozes int `json:"snoozes"`
	}
	if err := json.Unmarshal(j.Metadata, &meta); err != nil {
		return 0
	}
	return meta.Snoozes
}

// Timeout bounds a single rematch run. Multi-method rematch over large
// calendar + telegram histories can take tens of seconds; 5 minutes is
// ample headroom.
func (*RematchDispatcherWorker) Timeout(*river.Job[consumerjobs.RematchDispatcherJobArgs]) time.Duration {
	return 5 * time.Minute
}
