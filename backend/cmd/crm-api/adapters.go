package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"

	"personal-crm/backend/internal/consumer"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/scheduler"
	"personal-crm/backend/internal/todoist"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
)

// noopJobArgs is the args type for the placeholder worker below. It is
// never enqueued in production; its sole purpose is to satisfy river's
// "must have at least one registered worker" invariant when external
// sync is disabled.
type noopJobArgs struct{}

func (noopJobArgs) Kind() string { return "noop" }

// noopWorker exists so the river client always has at least one
// registered worker, even when cfg.Features.EnableExternalSync is false
// and the scheduler workers are not registered. river.NewClient rejects
// an empty Workers bundle (the constructor returns an error), so the
// API fails to boot in the default non-sync configuration without this
// placeholder.
type noopWorker struct {
	river.WorkerDefaults[noopJobArgs]
}

// Work implements river.Worker. Since no 'noop' jobs are enqueued
// anywhere in the codebase, this method is never called at runtime.
func (*noopWorker) Work(_ context.Context, _ *river.Job[noopJobArgs]) error {
	return nil
}

// followUpSettingsRef holds a deferred reference to the Todoist OAuth
// service + sync repo so the FollowUpManager settings func can be
// wired at construction time (before the external-sync branch decides
// whether Todoist is configured). The external-sync branch populates
// oauth+sync when Todoist is initialized; until then fn() returns
// consumer.ErrTodoistUnconfigured to keep the consumer's Todoist-dependent
// post-commit paths a best-effort no-op.
type followUpSettingsRef struct {
	oauth       *todoist.OAuthService
	sync        *repository.SyncRepository
	frontendURL string
}

// fn returns a TodoistSettingsFunc closure that resolves settings
// through the populated refs. Todoist-unconfigured states (no account,
// no sync state, missing label) collapse to consumer.ErrTodoistUnconfigured
// so the follow-up consumer can treat them as a non-fatal skip rather
// than rolling back the interaction write.
func (r *followUpSettingsRef) fn() consumer.TodoistSettingsFunc {
	return func(ctx context.Context) (*todoist.Settings, string, error) {
		if r.oauth == nil || r.sync == nil {
			return nil, "", consumer.ErrTodoistUnconfigured
		}
		accounts, err := r.oauth.ListAccounts(ctx)
		if err != nil {
			return nil, "", fmt.Errorf("list todoist accounts: %w", err)
		}
		if len(accounts) == 0 {
			return nil, "", consumer.ErrTodoistUnconfigured
		}
		accountID := accounts[0].AccountID
		accessToken, err := r.oauth.GetAccessToken(ctx, accountID)
		if err != nil {
			return nil, "", fmt.Errorf("get access token: %w", err)
		}
		state, err := r.sync.GetSyncStateBySource(ctx, todoist.SourceName, &accountID)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return nil, "", consumer.ErrTodoistUnconfigured
			}
			return nil, "", fmt.Errorf("get sync state: %w", err)
		}
		settings := &todoist.Settings{}
		if state.Metadata != nil {
			if v, ok := state.Metadata[todoist.MetadataKeyProjectID].(string); ok {
				settings.ProjectID = v
			}
			if v, ok := state.Metadata[todoist.MetadataKeyProjectName].(string); ok {
				settings.ProjectName = v
			}
			if v, ok := state.Metadata[todoist.MetadataKeyLabelID].(string); ok {
				settings.LabelID = v
			}
			if v, ok := state.Metadata[todoist.MetadataKeyLabelName].(string); ok {
				settings.LabelName = v
			}
			if v, ok := state.Metadata[todoist.MetadataKeyIntegrationInstance].(string); ok {
				settings.IntegrationInstanceID = v
			}
		}
		if settings.LabelID == "" {
			return nil, "", consumer.ErrTodoistUnconfigured
		}
		return settings, accessToken, nil
	}
}

// deferredAggregatorReenqueuer is a thread-safe holder that the
// InteractionRecorderWorker is constructed against before the
// Telegram aggregation engine exists. The cfg.Features.EnableTelegramSync
// branch calls .set once telegramManager is built; until then the
// holder dispatches every Reenqueue call to a logged-warn no-op.
//
// Satisfies consumer.AggregatorReenqueuer (via the holder pointer);
// safe to pass to the worker constructor before the inner registry
// is wired.
type deferredAggregatorReenqueuer struct {
	mu    sync.RWMutex
	inner consumer.AggregatorReenqueuer
}

// set installs the concrete reenqueuer. May be called once after the
// Telegram aggregation engine exists.
func (d *deferredAggregatorReenqueuer) set(inner consumer.AggregatorReenqueuer) {
	d.mu.Lock()
	d.inner = inner
	d.mu.Unlock()
}

// Reenqueue implements consumer.AggregatorReenqueuer. Falls back to a
// logged-warn no-op when the inner registry has not been wired yet
// (e.g. cfg.Features.EnableTelegramSync is false).
func (d *deferredAggregatorReenqueuer) Reenqueue(ctx context.Context, env *events.Envelope, contactID uuid.UUID) error {
	d.mu.RLock()
	inner := d.inner
	d.mu.RUnlock()
	if inner == nil {
		log.Printf("aggregator-reenqueuer: registry not yet wired; skipping (source=%s contact=%s)",
			env.Source, contactID.String())
		return nil
	}
	return inner.Reenqueue(ctx, env, contactID)
}

// commsSourceContactLister pins the multi-source comms_message repository to a
// single source so it satisfies the sweeper's single-source
// scheduler.UnprocessedContactLister interface (ListUnprocessedContactIDs(ctx)).
// It carries no persistence logic — only source-pinning delegation.
type commsSourceContactLister struct {
	repo   *repository.CommsMessageRepository
	source string
}

var _ scheduler.UnprocessedContactLister = (*commsSourceContactLister)(nil)

// newCommsSourceContactLister builds the source-bound sweeper lister adapter.
func newCommsSourceContactLister(repo *repository.CommsMessageRepository, source string) *commsSourceContactLister {
	return &commsSourceContactLister{repo: repo, source: source}
}

// ListUnprocessedContactIDs implements scheduler.UnprocessedContactLister.
func (l *commsSourceContactLister) ListUnprocessedContactIDs(ctx context.Context) ([]uuid.UUID, error) {
	return l.repo.ListUnprocessedContactIDsForSource(ctx, l.source)
}
