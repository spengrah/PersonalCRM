package main

import (
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/judgment"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/threadctx"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
)

// buildJudgmentStore constructs the one judgment store over the root's
// database, AssertService, CadenceUpdater and River client, with the
// production method DECISION_PRODUCTION_METHOD names, and makes it the
// CadenceUpdater's obligation closer. It is built before ContactService, so
// every recording path runs the closer.
func buildJudgmentStore(
	cfg *config.Config,
	database *db.Database,
	graph graphCore,
	consumers eventConsumers,
	messaging messagingFoundation,
	riverClient *river.Client[pgx.Tx],
) *judgment.Store {
	store := judgment.NewStore(judgment.StoreDeps{
		Pool:             database.Pool,
		Queries:          database.Queries,
		Assert:           graph.AssertService,
		Contexts:         threadctx.NewBuilder(repository.NewThreadContentRepository(database.Queries), messaging.VenueRepo),
		Cadence:          consumers.CadenceUpdater,
		Jobs:             riverClient,
		Watchdog:         cfg.Watchdog,
		ProductionMethod: cfg.Decision.ProductionMethod,
	})
	consumers.CadenceUpdater.SetObligationCloser(store)
	return store
}
