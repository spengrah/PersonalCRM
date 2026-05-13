// crm-admin is a one-shot operator utility for Pi-side maintenance
// tasks that don't fit the HTTP API surface. Operator-only; NOT
// deployed as part of the production service.
//
// Subcommands:
//
//	--messages-rematch-stranded   Retroactively match messages_message
//	                              rows whose matched_contact_id is NULL
//	                              against the current contact_method set,
//	                              update them, and enqueue
//	                              MessagingAggregateForContactArgs River
//	                              jobs per newly-matched contact.
//
// See backend/internal/messages/admin_rematch.go for the business
// logic — this binary is a thin CLI shim so the entire admin handler
// stays unit-testable independently of the binary.
//
// Build:  make crm-admin   (operator-only target; NOT wired into CI).
//
// Single-file pkg-main per .ai/rules/core.md "Adding types to
// cmd/crm-api/ in a companion file": all types referenced from this
// file must be DEFINED here or in their own internal packages.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/consumer/consumerjobs"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/messages"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("crm-admin: %v", err)
	}
}

func run() error {
	rematchStranded := flag.Bool("messages-rematch-stranded", false,
		"Retroactively match stranded messages_message rows against contact_method and enqueue aggregator jobs")
	flag.Parse()

	if !*rematchStranded {
		flag.Usage()
		return fmt.Errorf("no subcommand specified; pass --messages-rematch-stranded")
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	ctx := context.Background()
	if err := db.RunMigrations(ctx, cfg.Database.URL, cfg.Database.MigrationsPath); err != nil {
		return fmt.Errorf("run migrations: %w", err)
	}
	database, err := db.NewDatabase(ctx, cfg.Database)
	if err != nil {
		return fmt.Errorf("connect db: %w", err)
	}
	defer database.Close()

	// River client configured as an inserter only. We register a
	// noop worker for MessagingAggregateForContactArgs so River
	// accepts the kind at Insert time without actually running the
	// aggregation worker in this admin process — the main crm-api
	// service owns execution.
	workers := river.NewWorkers()
	river.AddWorker(workers, &noopMessagingAggregateWorker{})
	riverClient, err := river.NewClient(riverpgxv5.New(database.Pool), &river.Config{
		JobTimeout: cfg.River.JobTimeout,
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: 1},
		},
		Workers: workers,
	})
	if err != nil {
		return fmt.Errorf("build river client: %w", err)
	}

	// Repositories + identity service. Constructed locally; identical
	// shape to the crm-api wiring.
	identityRepo := repository.NewIdentityRepository(database.Queries)
	identityService := service.NewIdentityService(identityRepo)
	messagesRepo := repository.NewMessagesMessageRepository(database.Queries)

	res, err := messages.RematchStranded(ctx, messages.RematchStrandedDeps{
		Messages:    messagesRepo,
		Identity:    identityService,
		RiverClient: adminRiverAdapter{client: riverClient},
	})
	if err != nil {
		return fmt.Errorf("rematch stranded: %w", err)
	}

	if _, err := fmt.Fprintf(os.Stdout, "messages-rematch-stranded summary:\n"+
		"  scanned:        %d\n"+
		"  matched:        %d\n"+
		"  still_stranded: %d\n"+
		"  enqueued:       %d\n"+
		"  errors:         %d\n",
		res.Scanned, res.Matched, res.StillStranded, res.Enqueued, res.Errors); err != nil {
		return fmt.Errorf("write summary: %w", err)
	}
	return nil
}

// adminRiverAdapter adapts *river.Client[pgx.Tx] to the
// messages.AdminRiverInserter interface. Trivial pass-through.
type adminRiverAdapter struct {
	client *river.Client[pgx.Tx]
}

func (a adminRiverAdapter) Insert(ctx context.Context, args river.JobArgs, opts *river.InsertOpts) (*rivertype.JobInsertResult, error) {
	return a.client.Insert(ctx, args, opts)
}

// noopMessagingAggregateWorker satisfies River's "every enqueued kind
// must have a registered worker" rule for the admin binary's insert-
// only path. Execution lives in crm-api; the admin process never
// runs the worker — Start is never called on the client.
type noopMessagingAggregateWorker struct {
	river.WorkerDefaults[consumerjobs.MessagingAggregateForContactArgs]
}

func (w *noopMessagingAggregateWorker) Work(_ context.Context, _ *river.Job[consumerjobs.MessagingAggregateForContactArgs]) error {
	return nil
}
