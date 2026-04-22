package consumer

import (
	"context"
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
}

// RematchDispatcher is the event-bus consumer for contact_methods.added.
// Per spec §3.4.4 it runs the existing rematch pipeline (per-method
// handler iteration + per-contact mutex serialization) via
// RematchService.Run. Always-on — PR 10 plan Decision 1 removed the
// mode flag because a registered River worker returning nil in off
// mode would permanently ack queued jobs and make git revert the only
// real rollback path anyway.
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
		// Run has already marked the in-memory job Failed. Propagate so
		// River retries per MaxAttempts (3) from InsertOpts.
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

// Work implements river.Worker. Fetches the event envelope by id and
// invokes HandleEvent. On error river retries per MaxAttempts (3 from
// the InsertOpts set in events.consumerJobsForKind).
func (w *RematchDispatcherWorker) Work(ctx context.Context, j *river.Job[consumerjobs.RematchDispatcherJobArgs]) error {
	env, err := w.bus.GetEvent(ctx, j.Args.EventID)
	if err != nil {
		return fmt.Errorf("fetch event %s: %w", j.Args.EventID, err)
	}
	return w.dispatcher.HandleEvent(ctx, env)
}

// Timeout bounds a single rematch run. Multi-method rematch over large
// calendar + telegram histories can take tens of seconds; 5 minutes is
// ample headroom.
func (*RematchDispatcherWorker) Timeout(*river.Job[consumerjobs.RematchDispatcherJobArgs]) time.Duration {
	return 5 * time.Minute
}
