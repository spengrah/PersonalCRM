// crm-admin is a one-shot operator utility for Pi-side maintenance
// tasks that don't fit the HTTP API surface. Operator-only; NOT
// deployed as part of the production service.
//
// Subcommands (exactly one required):
//
//	--messages-rematch-stranded   Retroactively match messages_message
//	                              rows whose matched_contact_id is NULL
//	                              against the current contact_method set,
//	                              update them, and enqueue
//	                              MessagingAggregateForContactArgs River
//	                              jobs per newly-matched contact.
//
//	--reconcile-address-book-methods
//	                              One-time catchup for the address-book
//	                              method leak: re-propagate any address
//	                              book (gcontacts / icloud_contacts)
//	                              method missing from an already-linked
//	                              CRM contact. Auto-applies for
//	                              auto-`matched` rows; records a pending
//	                              suggestion for user-`imported` rows.
//	                              Continue-on-error; exits non-zero iff
//	                              any row failed. Idempotent — safe to
//	                              re-run after fixing a failure cause.
//	                              NOTE: pre-existing rows that were modal-
//	                              linked-with-deselections BEFORE the
//	                              LinkContact curated-status change are
//	                              stored `matched`, so this one-time run
//	                              may auto-apply a previously-deselected
//	                              method (bounded; user-recoverable by
//	                              deleting the method).
//
//	--mint-pairing-token          Mint a single-use pairing token for
//	                              `crm-mac install --pair`. Optional
//	                              --hostname-label is operator-side
//	                              terminal context only; not persisted
//	                              server-side.
//
//	--list-hosts                  Print active paired Mac hosts
//	                              (id, hostname, created_at,
//	                              last_heartbeat_at) for ambiguous-pair
//	                              recovery.
//
//	--revoke-host <uuid>          Revoke a paired Mac host by id.
//
//	--rotate-host-key <uuid>      Validate the given host_id is an active
//	                              paired host, mint a fresh pairing token,
//	                              and print the templated
//	                              `crm-mac install --re-pair --pair <token>`
//	                              command. The rotation itself runs on the
//	                              Mac — this binary does not hold the
//	                              per-host pair-key.
//
// See backend/internal/messages/admin_rematch.go for the rematch
// business logic and backend/internal/service/mac_host.go for the
// pairing service. This binary is a thin CLI shim so the entire
// admin surface stays unit-testable independently of the binary.
//
// Build:  make crm-admin   (operator-only target; NOT wired into CI).
//
// Single-file pkg-main per .ai/rules/core.md "Adding types to
// cmd/crm-api/ in a companion file": all types referenced from this
// file must be DEFINED here or in their own internal packages.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/consumer/consumerjobs"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/messages"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"
)

// tokenMinter is the narrow interface --mint-pairing-token needs.
// Production wires this to *service.MacHostService.CreatePairingToken.
type tokenMinter interface {
	CreatePairingToken(ctx context.Context) (plaintext string, expiresAt time.Time, err error)
}

// hostLister is the narrow interface --list-hosts needs.
type hostLister interface {
	ListActiveHosts(ctx context.Context) ([]*repository.MacHost, error)
}

// hostRevoker is the narrow interface --revoke-host needs.
type hostRevoker interface {
	RevokeHost(ctx context.Context, id uuid.UUID) error
}

// rematchRunner is the narrow interface --messages-rematch-stranded
// needs. Defined for symmetry with the new flags so all four
// subcommands can be exercised by test doubles.
type rematchRunner interface {
	RematchStranded(ctx context.Context) (*messages.RematchStrandedResult, error)
}

// reconcileRunner is the narrow interface
// --reconcile-address-book-methods needs. Production wires this to
// *service.AddressBookReconcileService.ReconcileAllAddressBookMethods.
type reconcileRunner interface {
	ReconcileAllAddressBookMethods(ctx context.Context) (service.ReconcileAllResult, error)
}

// adminDeps groups the four subcommand dependencies. Tests inject
// fakes for each interface; the production wiring builds a single
// MacHostService and a small adapter for rematch.
type adminDeps struct {
	tokens    tokenMinter
	hosts     hostLister
	revoker   hostRevoker
	rematch   rematchRunner
	reconcile reconcileRunner
	stdout    io.Writer
	stderr    io.Writer
}

// runOptions captures parsed flags. Exposed as a struct so tests can
// drive run() directly without re-parsing argv.
type runOptions struct {
	rematchStranded      bool
	reconcileAddressBook bool
	mintPairingToken     bool
	hostnameLabel        string
	listHosts            bool
	revokeHostID         string
	rotateHostID         string
}

func main() {
	if err := runMain(os.Args[1:]); err != nil {
		log.Fatalf("crm-admin: %v", err)
	}
}

// runMain parses argv and dispatches. Split out so tests can drive
// the dispatch + mutual-exclusion logic against fake deps without
// touching flag.Parse's global state.
func runMain(args []string) error {
	opts, err := parseArgs(args)
	if err != nil {
		return err
	}
	// Validate subcommand selection BEFORE loading config / opening
	// the DB so a missing-flag or mutual-exclusion error surfaces as
	// a usage error rather than a config-not-found / migration error.
	if err := validateSubcommand(opts); err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	ctx := context.Background()
	database, err := db.NewDatabase(ctx, cfg.Database)
	if err != nil {
		return fmt.Errorf("connect db: %w", err)
	}
	defer database.Close()

	deps, cleanup, err := buildProductionDeps(ctx, cfg, database)
	if err != nil {
		return err
	}
	defer cleanup()
	deps.stdout = os.Stdout
	deps.stderr = os.Stderr

	return run(ctx, opts, deps)
}

// validateSubcommand returns nil iff exactly one subcommand flag is
// set. Run BEFORE config/DB setup so usage errors are surfaced
// immediately.
func validateSubcommand(opts runOptions) error {
	active := 0
	if opts.rematchStranded {
		active++
	}
	if opts.reconcileAddressBook {
		active++
	}
	if opts.mintPairingToken {
		active++
	}
	if opts.listHosts {
		active++
	}
	if opts.revokeHostID != "" {
		active++
	}
	if opts.rotateHostID != "" {
		active++
	}
	if active == 0 {
		return errors.New("no subcommand specified; pass exactly one of " +
			"--messages-rematch-stranded, --reconcile-address-book-methods, --mint-pairing-token, --list-hosts, --revoke-host <uuid>, --rotate-host-key <uuid>")
	}
	if active > 1 {
		return errors.New("subcommand flags are mutually exclusive; pass exactly one of " +
			"--messages-rematch-stranded, --reconcile-address-book-methods, --mint-pairing-token, --list-hosts, --revoke-host <uuid>, --rotate-host-key <uuid>")
	}
	return nil
}

// parseArgs uses a private FlagSet so tests can drive the parser
// without polluting flag's global state.
func parseArgs(args []string) (runOptions, error) {
	fs := flag.NewFlagSet("crm-admin", flag.ContinueOnError)
	var opts runOptions
	fs.BoolVar(&opts.rematchStranded, "messages-rematch-stranded", false,
		"Retroactively match stranded messages_message rows and enqueue aggregator jobs.")
	fs.BoolVar(&opts.reconcileAddressBook, "reconcile-address-book-methods", false,
		"One-time catchup: re-propagate address-book (gcontacts/icloud) methods "+
			"missing from already-linked contacts (auto-apply for matched, record "+
			"suggestion for imported). Continue-on-error; exits non-zero iff any row failed.")
	fs.BoolVar(&opts.mintPairingToken, "mint-pairing-token", false,
		"Mint a single-use pairing token for `crm-mac install --pair`.")
	fs.StringVar(&opts.hostnameLabel, "hostname-label", "",
		"Optional operator-side label for the mint output. NOT persisted server-side.")
	fs.BoolVar(&opts.listHosts, "list-hosts", false,
		"Print active paired Mac hosts for ambiguous-pair recovery.")
	fs.StringVar(&opts.revokeHostID, "revoke-host", "",
		"Revoke an existing paired Mac host by UUID.")
	fs.StringVar(&opts.rotateHostID, "rotate-host-key", "",
		"Validate the given host_id is active, mint a fresh pairing token, "+
			"and print the `crm-mac install --re-pair` command for rotating "+
			"that host's api-key. The rotation itself runs on the Mac.")
	if err := fs.Parse(args); err != nil {
		return runOptions{}, err
	}
	return opts, nil
}

// run dispatches to exactly one subcommand. Validates flag selection
// defensively even though runMain validates first — tests drive run()
// directly, so the same guard lives here.
func run(ctx context.Context, opts runOptions, deps adminDeps) error {
	if err := validateSubcommand(opts); err != nil {
		return err
	}

	switch {
	case opts.mintPairingToken:
		return runMintPairingToken(ctx, opts, deps)
	case opts.listHosts:
		return runListHosts(ctx, deps)
	case opts.revokeHostID != "":
		return runRevokeHost(ctx, opts, deps)
	case opts.rotateHostID != "":
		return runRotateHostKey(ctx, opts, deps)
	case opts.rematchStranded:
		return runRematchStranded(ctx, deps)
	case opts.reconcileAddressBook:
		return runReconcileAddressBookMethods(ctx, deps)
	}
	return errors.New("unreachable")
}

func runMintPairingToken(ctx context.Context, opts runOptions, deps adminDeps) error {
	token, expiresAt, err := deps.tokens.CreatePairingToken(ctx)
	if err != nil {
		return fmt.Errorf("mint pairing token: %w", err)
	}
	if _, err := fmt.Fprintf(deps.stdout,
		"token=%s\nexpires_at=%s\n", token, expiresAt.UTC().Format(time.RFC3339)); err != nil {
		return fmt.Errorf("write token: %w", err)
	}
	if opts.hostnameLabel != "" {
		if _, err := fmt.Fprintf(deps.stdout,
			"hostname_label=%s\n", opts.hostnameLabel); err != nil {
			return fmt.Errorf("write hostname label: %w", err)
		}
	}
	if _, err := fmt.Fprintf(deps.stdout,
		"note: paste into `crm-mac install --pair <token>` within 10 minutes\n"); err != nil {
		return fmt.Errorf("write note: %w", err)
	}
	return nil
}

func runListHosts(ctx context.Context, deps adminDeps) error {
	hosts, err := deps.hosts.ListActiveHosts(ctx)
	if err != nil {
		return fmt.Errorf("list active hosts: %w", err)
	}
	if len(hosts) == 0 {
		if _, err := fmt.Fprintln(deps.stdout, "no active paired hosts"); err != nil {
			return fmt.Errorf("write empty list: %w", err)
		}
		return nil
	}
	for _, h := range hosts {
		lastHeartbeat := "never"
		if h.LastHeartbeatAt != nil {
			lastHeartbeat = h.LastHeartbeatAt.UTC().Format(time.RFC3339)
		}
		if _, err := fmt.Fprintf(deps.stdout,
			"id=%s hostname=%s created_at=%s last_heartbeat_at=%s\n",
			h.ID, h.Hostname, h.CreatedAt.UTC().Format(time.RFC3339), lastHeartbeat); err != nil {
			return fmt.Errorf("write host row: %w", err)
		}
	}
	return nil
}

func runRevokeHost(ctx context.Context, opts runOptions, deps adminDeps) error {
	id, err := uuid.Parse(strings.TrimSpace(opts.revokeHostID))
	if err != nil {
		return fmt.Errorf("--revoke-host must be a valid UUID: %w", err)
	}
	if err := deps.revoker.RevokeHost(ctx, id); err != nil {
		return fmt.Errorf("revoke host %s: %w", id, err)
	}
	if _, err := fmt.Fprintf(deps.stdout, "revoked host_id=%s\n", id); err != nil {
		return fmt.Errorf("write revoke summary: %w", err)
	}
	return nil
}

// runRotateHostKey validates the given host_id is active, mints a
// fresh pairing token, and prints the templated `crm-mac install
// --re-pair --pair <token>` command. The rotation itself happens on
// the Mac (the daemon authenticates with its CURRENT pair-key, which
// the Pi-side CLI deliberately does not hold).
func runRotateHostKey(ctx context.Context, opts runOptions, deps adminDeps) error {
	id, err := uuid.Parse(strings.TrimSpace(opts.rotateHostID))
	if err != nil {
		return fmt.Errorf("--rotate-host-key must be a valid UUID: %w", err)
	}
	hosts, err := deps.hosts.ListActiveHosts(ctx)
	if err != nil {
		return fmt.Errorf("list active hosts: %w", err)
	}
	found := false
	for _, h := range hosts {
		if h.ID == id {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("--rotate-host-key %s: no active host with that id (already revoked?)", id)
	}
	token, expiresAt, err := deps.tokens.CreatePairingToken(ctx)
	if err != nil {
		return fmt.Errorf("mint pairing token: %w", err)
	}
	if _, err := fmt.Fprintf(deps.stdout,
		"token=%s\nexpires_at=%s\n\nRun on the Mac:\n  crm-mac install --re-pair --pair %s\n",
		token, expiresAt.UTC().Format(time.RFC3339), token); err != nil {
		return fmt.Errorf("write rotate-host-key output: %w", err)
	}
	return nil
}

func runRematchStranded(ctx context.Context, deps adminDeps) error {
	res, err := deps.rematch.RematchStranded(ctx)
	if err != nil {
		return fmt.Errorf("rematch stranded: %w", err)
	}
	if _, err := fmt.Fprintf(deps.stdout, "messages-rematch-stranded summary:\n"+
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

// runReconcileAddressBookMethods runs the one-time address-book method
// catchup and prints a counts-only summary (no PII). Returns a non-nil
// error iff any row failed so the process exits non-zero — the operator
// can fix the cause and re-run safely (idempotent).
func runReconcileAddressBookMethods(ctx context.Context, deps adminDeps) error {
	res, err := deps.reconcile.ReconcileAllAddressBookMethods(ctx)
	if err != nil {
		return fmt.Errorf("reconcile address-book methods: %w", err)
	}
	if _, err := fmt.Fprintf(deps.stdout, "reconcile-address-book-methods summary:\n"+
		"  scanned:               %d\n"+
		"  methods_auto_applied:  %d\n"+
		"  suggestions_recorded:  %d\n"+
		"  failed:                %d\n",
		res.Scanned, res.MethodsAutoApplied, res.SuggestionsRecorded, res.Failed); err != nil {
		return fmt.Errorf("write summary: %w", err)
	}
	if res.Failed > 0 {
		return fmt.Errorf("%d row(s) failed to reconcile (see logs); re-run after fixing the cause", res.Failed)
	}
	return nil
}

// buildProductionDeps wires up the production service stack. Returns
// a cleanup function to call before exit (releases the river client).
func buildProductionDeps(ctx context.Context, cfg *config.Config, database *db.Database) (adminDeps, func(), error) {
	// River client configured as an inserter only. We register a
	// noop worker for MessagingAggregateForContactArgs so River
	// accepts the kind at Insert time without actually running the
	// aggregation worker in this admin process — the main crm-api
	// service owns execution.
	workers := river.NewWorkers()
	river.AddWorker(workers, &noopMessagingAggregateWorker{})
	// The address-book reconcile auto-applies methods and publishes
	// contact_methods.added, which enqueues a RematchDispatcher job. The
	// admin binary is insert-only — register a noop worker so River
	// accepts the kind at Insert time; the always-running crm-api service
	// owns dispatcher execution (it re-fetches the event and runs the
	// real handlers).
	river.AddWorker(workers, &noopRematchDispatcherWorker{})
	riverClient, err := river.NewClient(riverpgxv5.New(database.Pool), &river.Config{
		JobTimeout: cfg.River.JobTimeout,
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: 1},
		},
		Workers: workers,
	})
	if err != nil {
		return adminDeps{}, nil, fmt.Errorf("build river client: %w", err)
	}

	identityRepo := repository.NewIdentityRepository(database.Queries)
	identityService := service.NewIdentityService(identityRepo)
	messagesRepo := repository.NewMessagesMessageRepository(database.Queries)

	// MacHostService wires through the same dependencies the API
	// handler uses. The bcryptCost passes 0 to indicate default;
	// see service.NewMacHostService.
	hostRepo := repository.NewMacHostRepository(database.Queries)
	pairingTokenRepo := repository.NewMacHostPairingTokenRepository(database.Queries)
	syncRepo := repository.NewSyncRepository(database.Queries)
	contactMethodLister := repository.NewContactMethodRepository(database.Queries)
	externalContactRepo := repository.NewExternalContactRepository(database.Queries)
	hostService := service.NewMacHostService(
		hostRepo,
		pairingTokenRepo,
		syncRepo,
		contactMethodLister,
		externalContactRepo, // /known-ids reader (external_contact)
		repository.NewMeetingNoteRepository(database.Queries), // /known-ids reader (anarlog_sessions)
		database.Pool,
		0)

	rematch := rematchAdapter{
		messagesRepo:    messagesRepo,
		identityService: identityService,
		riverClient:     riverClient,
	}

	// Address-book method reconcile catchup. Wires a real event bus +
	// EnrichmentService so the matched-row auto-propagation publishes
	// contact_methods.added (enqueuing a RematchDispatcher job the
	// running crm-api processes). Lightweight no-op rematch handlers for
	// email/phone make those method types "eligible" so the publish
	// fires; the admin process never runs the handler bodies — only the
	// crm-api dispatcher does, re-deriving from the event payload.
	rematchService := service.NewRematchService()
	rematchService.Register(noopRematchHandler{idType: "email"})
	rematchService.Register(noopRematchHandler{idType: "phone"})
	contactRepo := repository.NewContactRepository(database.Queries)
	contactMethodRepo := repository.NewContactMethodRepository(database.Queries)
	enrichmentRepo := repository.NewEnrichmentRepository(database.Queries)
	eventBus := events.NewBus(database.Pool, riverClient, repository.NewEventRepository(database.Queries))
	enrichmentService := service.NewEnrichmentService(
		database, contactRepo, contactMethodRepo, enrichmentRepo, eventBus, rematchService,
	)
	addressBookReconcile := service.NewAddressBookReconcileService(
		enrichmentService, contactRepo, contactMethodRepo, externalContactRepo,
	)

	cleanup := func() {}
	return adminDeps{
		tokens:    hostService,
		hosts:     hostService,
		revoker:   hostService,
		rematch:   rematch,
		reconcile: addressBookReconcile,
	}, cleanup, nil
}

// rematchAdapter wraps messages.RematchStranded so it conforms to
// rematchRunner without exposing the dependency struct through the
// admin surface.
type rematchAdapter struct {
	messagesRepo    *repository.MessagesMessageRepository
	identityService *service.IdentityService
	riverClient     *river.Client[pgx.Tx]
}

func (a rematchAdapter) RematchStranded(ctx context.Context) (*messages.RematchStrandedResult, error) {
	res, err := messages.RematchStranded(ctx, messages.RematchStrandedDeps{
		Messages:    a.messagesRepo,
		Identity:    a.identityService,
		RiverClient: adminRiverAdapter{client: a.riverClient},
	})
	if err != nil {
		return nil, err
	}
	return res, nil
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

// noopRematchDispatcherWorker satisfies River's "every enqueued kind
// must have a registered worker" rule for the RematchDispatcher jobs the
// address-book reconcile enqueues via the event bus. Execution lives in
// crm-api; the admin process never runs the worker — Start is never
// called on the client.
type noopRematchDispatcherWorker struct {
	river.WorkerDefaults[consumerjobs.RematchDispatcherJobArgs]
}

func (w *noopRematchDispatcherWorker) Work(_ context.Context, _ *river.Job[consumerjobs.RematchDispatcherJobArgs]) error {
	return nil
}

// noopRematchHandler is a type-only rematch handler registered for the
// email/phone method types so EnrichmentService.EligibleMethods reports
// auto-applied address-book methods as eligible — which is what makes
// the event bus publish contact_methods.added (enqueuing the
// RematchDispatcher job). Its Rematch body is never invoked in the admin
// process: the always-running crm-api owns dispatch and re-derives the
// rematch work from the persisted event with its own real handlers.
type noopRematchHandler struct {
	idType string
}

func (h noopRematchHandler) IdentifierType() string { return h.idType }

func (h noopRematchHandler) Rematch(_ context.Context, _ uuid.UUID, _ string) (int, error) {
	return 0, nil
}
