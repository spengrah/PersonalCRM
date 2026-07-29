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
//	--rederive-correspondence-names
//	                              One-time catchup for the gmail_correspondence
//	                              source: re-fetch already-ingested email
//	                              messages from Gmail and additively populate
//	                              participant display names in comms_message
//	                              metadata (preserving all existing content +
//	                              provenance). Continue-on-error; exits
//	                              non-zero iff any row FAILED (Skipped* do not
//	                              fail). Idempotent — safe to re-run. Requires
//	                              Google OAuth credentials; built lazily so
//	                              other subcommands work on Google-less hosts.
//	                              NOTE: candidate discovery now runs in-sync on
//	                              the Gmail fetch loop, not here; this subcommand
//	                              only backfills display names.
//
//	--reset-gmail-backfill-cursors
//	                              Rewind enabled Gmail sync cursors to each
//	                              row's backfill floor, mark them due, and
//	                              clear error state. Prints counts only.
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
//	--seed [--profile P]          Seed the selected synthetic world (P =
//	      [--namespace N]         minimal-scoped|dev|prod-shaped|standard; default dev)
//	      [--prng-seed S] --yes   into the DB (ADDITIVE). REFUSED in production
//	                              (CRM_ENV gate) and when the river_job queue is
//	                              not drained. The crm-api service MUST be
//	                              STOPPED first (use `make dev-seed`). Requires
//	                              --yes (or CRM_SEED_RESET_CONFIRM=1). Prints a
//	                              counts-only summary. Re-running WITHOUT a reset
//	                              accumulates contacts — use --reset-and-seed for
//	                              a reproducible baseline.
//
//	--reset-and-seed [--profile P]  HARD-wipe every live data table (incl.
//	      [--namespace N]           oauth_credential + sync-state + telegram
//	      [--prng-seed S] --yes     session, preserving schema_migrations + River
//	                                internals + the migration-seeded curated
//	                                catalog [predicate/entity_type, provisional
//	                                rows cleared]), then reseed (default profile
//	                                prod-shaped). REFUSED in production.
//	                                The crm-api service MUST be STOPPED (use
//	                                `make staging-reset`). Requires --yes. The
//	                                stable namespace makes the reseed reproducible.
//
//	--migrate                     Apply pending application + River migrations
//	                              idempotently (the exact call crm-api boot
//	                              makes). Exit 0 on success, 1 on error. The
//	                              deploy script runs this from the NEW image,
//	                              after a snapshot, before swapping the running
//	                              container.
//
//	--migrate-check               Report whether migrations are pending WITHOUT
//	                              mutating the DB. THREE distinct exit codes:
//	                              0 = up-to-date, 2 = pending (app and/or River),
//	                              1 = operational error (cannot connect, dirty DB,
//	                              version read failed). The deploy script branches
//	                              on exit 2 to snapshot-then-migrate; exit 1 aborts
//	                              before touching anything. Prints a counts-only
//	                              summary (no PII).
//
//	--list-jobs                   List River jobs (one key=value row per job) for
//	      [--job-state S]         dead-letter inspection. --job-state is a
//	      [--job-limit N]         comma-separated state filter (default
//	                              "discarded,retryable"; each token validated
//	                              against River's job states). --job-limit caps the
//	                              row count (default 100; 1..10000). Newest job
//	                              first. SAFE with crm-api running (read-only).
//
//	--retry-job <id>              Make a single River job available to run again
//	                              (by numeric id from --list-jobs). On an exhausted
//	                              job River bumps max_attempts so it can run once
//	                              more. SAFE with crm-api running: the live worker
//	                              pool picks the job up — that is the intended flow.
//	                              Consumers are idempotent, so retrying an
//	                              already-side-effected job re-runs into a no-op.
//
//	--migrate-tags                Mirror the legacy tag / contact_tag tables into
//	                              the graph: each tag becomes a `tag` entity node
//	                              (color carried in the entity's detail JSONB) and
//	                              each contact_tag of a NON-deleted contact becomes
//	                              an accepted `tagged_as` assertion authored by the
//	                              user (knowledge time = contact_tag.created_at).
//	                              FAILS LOUDLY on a case-insensitive tag-name
//	                              collision (e.g. "Friend" vs "friend") so the
//	                              operator dedups the legacy tags first. Idempotent
//	                              (routes through AssertService proposition
//	                              identity + find-or-create entity nodes) — safe to
//	                              re-run. Prints a counts-only summary. The legacy
//	                              tag / contact_tag tables are NOT dropped (rollback
//	                              anchor, removed in a later migration).
//	--migrate-contact-knowledge-columns
//	                              Mirror the legacy contact.location / birthday /
//	                              how_met cache columns into the graph: location
//	                              becomes a `lives_in` edge to a place entity node,
//	                              birthday/how_met become facts — for each NON-deleted
//	                              contact, authored by the user (knowledge time =
//	                              contact.created_at). Idempotent (routes through
//	                              AssertService proposition identity + find-or-create
//	                              place nodes) — safe to re-run. Counts-only summary.
//	                              The cache columns are retained (the consumer keeps
//	                              them in sync from the assertions).
//
//	--river-tier0 [--window-hours N]
//	                              One-shot wait/run-by-kind read over live river_job
//	                              rows finalized in the last N hours (default 24).
//	                              An APPROXIMATE first signal before job_exec_sample
//	                              accrues — its wait is attempted_at - scheduled_at
//	                              over the last attempt only and River mutates
//	                              scheduled_at on retry, so do NOT compare it
//	                              numerically to the Tier-1 queue_wait_ms metric.
//	                              Read-only; SAFE with crm-api running.
//
// The seed/reset subcommands inherit the process env (CRM_ENV / TIME_BASE /
// TIME_ACCELERATION / DATABASE_URL / MIGRATIONS_PATH) — on the Pi via
// `set -a; . /srv/personalcrm/.env; set +a` — so the seeded world's cadence /
// overdue / time semantics track the running app's clock. They run migrations
// before seeding (db.NewDatabase does NOT migrate). The service-stopped
// requirement is asserted operationally by --yes + the wrapper scripts; River
// 0.34 exposes no sound in-Go live-client signal (river_client is unpopulated),
// so there is no in-Go liveness probe.
//
// See backend/internal/messages/admin_rematch.go for the rematch
// business logic and backend/internal/service/mac_host.go for the
// pairing service. This binary is a thin CLI shim so the entire
// admin surface stays unit-testable independently of the binary.
//
// Build:  make crm-admin   (operator-only target). CI cross-compiles the binary
// (a compile guard) but does not ship it; in prod the binary is baked into the
// backend image and invoked by the runner deploy (scripts/deploy-artifact.sh).
//
// This binary is built as a package (`go build ./cmd/crm-admin`), so new
// types it needs may live in sibling `package main` files here or in their
// own internal packages.
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

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/consumer"
	"personal-crm/backend/internal/consumer/consumerjobs"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/google"
	"personal-crm/backend/internal/messages"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	"personal-crm/backend/internal/synthetic"

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
// needs. Defined for symmetry with the other flags so subcommands can be
// exercised by test doubles.
type rematchRunner interface {
	RematchStranded(ctx context.Context) (*messages.RematchStrandedResult, error)
}

// reconcileRunner is the narrow interface
// --reconcile-address-book-methods needs. Production wires this to
// *service.AddressBookReconcileService.ReconcileAllAddressBookMethods.
type reconcileRunner interface {
	ReconcileAllAddressBookMethods(ctx context.Context) (service.ReconcileAllResult, error)
}

// rederiveRunner is the narrow interface --rederive-correspondence-names needs
// for step 1 (re-fetch + additive name backfill). Production wires this to
// *google.CorrespondenceNameRederiveService.RederiveNames.
type rederiveRunner interface {
	RederiveNames(ctx context.Context, since time.Time) (google.CorrespondenceRederiveResult, error)
}

// gmailBackfillCursorResetter is the narrow interface
// --reset-gmail-backfill-cursors needs. Production wires this to
// *service.EmailBackfillCursorResetService.ResetGmailBackfillCursors.
type gmailBackfillCursorResetter interface {
	ResetGmailBackfillCursors(ctx context.Context) (service.EmailBackfillCursorResetResult, error)
}

// tagMigrator is the narrow interface --migrate-tags needs. Production wires this
// to *service.TagMigrationService.MigrateTags.
type tagMigrator interface {
	MigrateTags(ctx context.Context) (service.TagMigrationResult, error)
}

// contactKnowledgeMigrator is the narrow interface
// --migrate-contact-knowledge-columns needs. Production wires this to
// *service.ContactKnowledgeMigrationService.MigrateContactKnowledgeColumns.
type contactKnowledgeMigrator interface {
	MigrateContactKnowledgeColumns(ctx context.Context) (service.ContactKnowledgeMigrationResult, error)
}

// seedRunner is the narrow interface --seed / --reset-and-seed need. Production
// wires this to a seedAdapter over the synthetic toolkit. Seed is additive (it
// runs the non-final-river_job drain preflight); ResetAndSeed hard-wipes the live
// data tables first (no drain check — it wipes river_job). Both build a harness
// for the stable per-profile namespace, run the profile, and on SUCCESS Quiesce
// (seed-and-leave) — on ERROR they run the full teardown so a failed seed cleans
// its partial world and never leaks the harness's River client. The service must
// be STOPPED (asserted operationally by --yes + the wrapper scripts; River 0.34
// exposes no sound in-Go live-client signal).
type seedRunner interface {
	Seed(ctx context.Context, params synthetic.SeedParams) (synthetic.ProfileResult, error)
	ResetAndSeed(ctx context.Context, params synthetic.SeedParams) (synthetic.ProfileResult, error)
}

// riverJobAdmin is the narrow interface --list-jobs / --retry-job need.
// *river.Client[pgx.Tx] satisfies it structurally, so the existing insert-only
// admin client serves both calls (JobList / JobRetry go straight through the
// driver — neither needs Start() nor registered workers).
type riverJobAdmin interface {
	JobList(ctx context.Context, params *river.JobListParams) (*river.JobListResult, error)
	JobRetry(ctx context.Context, id int64) (*rivertype.JobRow, error)
}

// tier0Reader is the narrow interface --river-tier0 needs. Production wires
// this to *repository.JobSampleRepository.
type tier0Reader interface {
	Tier0StatsByKind(ctx context.Context, cutoff time.Time) ([]repository.Tier0Row, error)
}

// adminDeps groups the per-subcommand dependencies. Tests inject
// fakes for each interface; the production wiring builds a single
// MacHostService and a small adapter for rematch.
type adminDeps struct {
	tokens    tokenMinter
	hosts     hostLister
	revoker   hostRevoker
	rematch   rematchRunner
	reconcile reconcileRunner
	// rederive is built LAZILY (only when --rederive-correspondence-names is
	// set) because it requires Google OAuth credentials; building it
	// unconditionally would break every other subcommand on a Google-less host.
	// Nil for all other subcommands.
	rederive   rederiveRunner
	gmailReset gmailBackfillCursorResetter
	seed       seedRunner
	// jobs serves --list-jobs / --retry-job. Wired to the insert-only river
	// client in buildProductionDeps (JobList/JobRetry go through the driver,
	// no worker pool needed).
	jobs riverJobAdmin
	// migrateTags serves --migrate-tags (mirror legacy tag/contact_tag into the
	// graph via AssertService). Wired to a TagMigrationService.
	migrateTags tagMigrator
	// migrateContactKnowledge serves --migrate-contact-knowledge-columns (mirror
	// legacy contact.location/birthday/how_met into the graph via AssertService).
	// Wired to a ContactKnowledgeMigrationService.
	migrateContactKnowledge contactKnowledgeMigrator
	// tier0 serves --river-tier0 (one-shot wait/run-by-kind read over live
	// river_job). Wired to a JobSampleRepository.
	tier0  tier0Reader
	stdout io.Writer
	stderr io.Writer
}

// runOptions captures parsed flags. Exposed as a struct so tests can
// drive run() directly without re-parsing argv.
type runOptions struct {
	rematchStranded        bool
	reconcileAddressBook   bool
	rederiveCorrespondence bool
	resetGmailBackfill     bool
	mintPairingToken       bool
	hostnameLabel          string
	listHosts              bool
	revokeHostID           string
	rotateHostID           string
	// Synthetic seed. doSeed ← --seed (the BOOL subcommand selector);
	// resetAndSeed ← --reset-and-seed; prngSeed ← --prng-seed (the uint64 PRNG
	// seed, a DISTINCT flag from --seed — Go's flag cannot bind one name to both
	// a Bool and a Uint64). seedYes ← --yes (mandatory confirm for BOTH commands).
	doSeed        bool
	resetAndSeed  bool
	seedProfile   string
	seedNamespace string
	prngSeed      uint64
	seedYes       bool
	// Migration subcommands. migrate ← --migrate (idempotent apply);
	// migrateCheck ← --migrate-check (report pending, non-mutating, distinct
	// exit codes). Both run PRE-DB (they need only cfg.Database URL +
	// MigrationsPath, not buildProductionDeps).
	migrate      bool
	migrateCheck bool
	// River dead-letter subcommands. listJobs ← --list-jobs (the bool
	// subcommand selector); jobState / jobLimit are auxiliary filters used only
	// by --list-jobs. retryJobID ← --retry-job (0 = unset; non-zero selects the
	// retry subcommand).
	listJobs   bool
	jobState   string
	jobLimit   int
	retryJobID int64
	// migrateTags ← --migrate-tags (mirror legacy tag/contact_tag into the graph
	// via the validated assertion write path). Idempotent; counts-only output.
	migrateTags bool
	// migrateContactKnowledge ← --migrate-contact-knowledge-columns (mirror legacy
	// contact.location/birthday/how_met into the graph via the validated assertion
	// write path). Idempotent; counts-only output.
	migrateContactKnowledge bool
	// riverTier0 ← --river-tier0 (one-shot wait/run-by-kind read over live
	// river_job for the last --window-hours). windowHours is auxiliary, used only
	// by --river-tier0.
	riverTier0  bool
	windowHours int
}

// exitErr carries an explicit process exit code so a subcommand can drive a
// distinct exit status (e.g. --migrate-check's exit 2 = pending). main() maps it
// via errors.As; every other error keeps the existing exit-1 (log.Fatalf)
// behavior. run()/runMain keep returning error so the dispatch + exit-code
// contract stay unit-testable without spawning a subprocess.
type exitErr struct {
	code int
	msg  string
}

func (e exitErr) Error() string { return e.msg }

func main() {
	if err := runMain(os.Args[1:]); err != nil {
		var ee exitErr
		if errors.As(err, &ee) {
			fmt.Fprintln(os.Stderr, "crm-admin:", ee.msg)
			os.Exit(ee.code)
		}
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

	// Migration subcommands run PRE-DB: they need only the database URL +
	// migrations path, NOT the full buildProductionDeps service stack (no River
	// client, no Google, no MacHostService). Dispatching here keeps them runnable
	// on a host that lacks Google creds etc., and avoids opening the application
	// pool just to check/apply migrations. Mirrors the seed gate short-circuit.
	if opts.migrate {
		return runMigrate(ctx, cfg.Database.URL, cfg.Database.MigrationsPath, os.Stdout)
	}
	if opts.migrateCheck {
		return runMigrateCheck(ctx, cfg.Database.URL, cfg.Database.MigrationsPath, os.Stdout)
	}

	// The seed/reset entrypoints invoke shippable fake-fetcher scaffolding and
	// (for reset) destroy data, so BOTH gates run PRE-DB AND PRE-MIGRATION — a
	// production env or a missing confirmation must be refused BEFORE the DB is
	// opened, before migrations run, and before a reset could begin. The other
	// subcommands assume an already-migrated live DB.
	if opts.doSeed || opts.resetAndSeed {
		if err := synthetic.SeedAllowed(cfg); err != nil {
			return err
		}
		// The mandatory --yes / CRM_SEED_RESET_CONFIRM gate runs here, BEFORE
		// migrations, so a mistaken reset never even applies migrations before
		// being rejected (the run-dispatch confirm check below is a defensive
		// backstop for tests that drive run() directly).
		if err := requireSeedConfirm(opts); err != nil {
			return err
		}
		// db.NewDatabase only connects + pings; it does NOT migrate. The seed
		// path may hit a fresh dev DB, and the reset TRUNCATE needs the schema to
		// exist — so migrate here (the same call crm-api makes), after the gates
		// and before opening the pool.
		if err := db.RunMigrations(ctx, cfg.Database.URL, cfg.Database.MigrationsPath); err != nil {
			return fmt.Errorf("run migrations: %w", err)
		}
	}

	database, err := db.NewDatabase(ctx, cfg.Database)
	if err != nil {
		return fmt.Errorf("connect db: %w", err)
	}
	defer database.Close()

	deps, cleanup, err := buildProductionDeps(ctx, cfg, database, opts)
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
	if opts.rederiveCorrespondence {
		active++
	}
	if opts.resetGmailBackfill {
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
	if opts.doSeed {
		active++
	}
	if opts.resetAndSeed {
		active++
	}
	if opts.migrate {
		active++
	}
	if opts.migrateCheck {
		active++
	}
	if opts.listJobs {
		active++
	}
	if opts.retryJobID != 0 {
		active++
	}
	if opts.migrateTags {
		active++
	}
	if opts.migrateContactKnowledge {
		active++
	}
	if opts.riverTier0 {
		active++
	}
	if active == 0 {
		return errors.New("no subcommand specified; pass exactly one of " +
			subcommandList)
	}
	if active > 1 {
		return errors.New("subcommand flags are mutually exclusive; pass exactly one of " +
			subcommandList)
	}
	return nil
}

// subcommandList is the canonical subcommand enumeration shared by the
// no-subcommand and mutual-exclusion usage errors.
const subcommandList = "--messages-rematch-stranded, --reconcile-address-book-methods, " +
	"--rederive-correspondence-names, --reset-gmail-backfill-cursors, --mint-pairing-token, " +
	"--list-hosts, --revoke-host <uuid>, --rotate-host-key <uuid>, --seed, --reset-and-seed, " +
	"--migrate, --migrate-check, --list-jobs, --retry-job <id>, --migrate-tags, " +
	"--migrate-contact-knowledge-columns, --river-tier0"

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
	fs.BoolVar(&opts.rederiveCorrespondence, "rederive-correspondence-names", false,
		"One-time catchup: re-fetch already-ingested email messages from Gmail and "+
			"additively populate participant display names in comms_message metadata "+
			"(candidate discovery now runs in-sync on the Gmail fetch loop, not here). "+
			"Continue-on-error; exits non-zero iff any row failed. Idempotent — safe to "+
			"re-run. Requires Google OAuth creds.")
	fs.BoolVar(&opts.resetGmailBackfill, "reset-gmail-backfill-cursors", false,
		"Rewind enabled Gmail sync cursors to each row's backfill floor, mark them due, "+
			"and clear error state. Prints counts only.")
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
	fs.BoolVar(&opts.doSeed, "seed", false,
		"Seed the selected synthetic --profile world into the DB (additive). REFUSED in "+
			"production (CRM_ENV gate) and when the river_job queue is not drained. The crm-api "+
			"service MUST be stopped first (use `make dev-seed` / `make staging-reset`). Requires --yes.")
	fs.BoolVar(&opts.resetAndSeed, "reset-and-seed", false,
		"HARD-wipe every live data table (incl. oauth_credential + sync-state, preserving only "+
			"schema_migrations + River internals), then reseed the selected --profile world. "+
			"REFUSED in production. The crm-api service MUST be stopped (use `make staging-reset`). "+
			"Requires --yes.")
	fs.StringVar(&opts.seedProfile, "profile", "",
		"Synthetic profile to seed: minimal-scoped | dev | prod-shaped | standard. Defaults to "+
			"`dev` for --seed and `prod-shaped` for --reset-and-seed.")
	fs.StringVar(&opts.seedNamespace, "namespace", "",
		"Override the stable per-profile namespace (default: the profile's stable token, so a "+
			"reseed reproduces the same world). Rarely needed.")
	fs.Uint64Var(&opts.prngSeed, "prng-seed", 0,
		"Override the deterministic PRNG seed (default: the toolkit's DefaultSeed). DISTINCT "+
			"from the --seed subcommand bool.")
	fs.BoolVar(&opts.seedYes, "yes", false,
		"Confirm a seed/reset against this DB. MANDATORY for --seed and --reset-and-seed (or set "+
			"CRM_SEED_RESET_CONFIRM=1). Asserts the operator stopped the crm-api service first.")
	fs.BoolVar(&opts.migrate, "migrate", false,
		"Apply pending application + River migrations (idempotent; the same call crm-api boot makes). "+
			"Exit 0 on success, 1 on error. Used by the deploy script before swapping the running image.")
	fs.BoolVar(&opts.migrateCheck, "migrate-check", false,
		"Report whether migrations are pending WITHOUT mutating the DB. Exit 0 = up-to-date, "+
			"2 = pending (app and/or River), 1 = operational error (cannot connect / dirty DB / read failed). "+
			"The deploy script branches on exit 2 to snapshot-then-migrate.")
	fs.BoolVar(&opts.listJobs, "list-jobs", false,
		"List River jobs (one key=value row per job) for dead-letter inspection. Use with "+
			"--job-state / --job-limit. Read-only; safe with crm-api running.")
	fs.StringVar(&opts.jobState, "job-state", "discarded,retryable",
		"Comma-separated River job states to filter --list-jobs (default discarded,retryable). "+
			"Each token is validated against River's job states.")
	fs.IntVar(&opts.jobLimit, "job-limit", 100,
		"Max rows for --list-jobs (default 100; 1..10000). Newest job first.")
	fs.Int64Var(&opts.retryJobID, "retry-job", 0,
		"Make a single River job (by numeric id from --list-jobs) available to run again. "+
			"On an exhausted job River bumps max_attempts. Safe with crm-api running.")
	fs.BoolVar(&opts.migrateTags, "migrate-tags", false,
		"Mirror the legacy tag/contact_tag tables into the graph (tag entity nodes + accepted "+
			"tagged_as assertions for non-deleted contacts). FAILS LOUDLY on a case-insensitive "+
			"tag-name collision. Idempotent; counts-only output. Legacy tables retained.")
	fs.BoolVar(&opts.migrateContactKnowledge, "migrate-contact-knowledge-columns", false,
		"Mirror the legacy contact.location/birthday/how_met cache columns into the graph "+
			"(lives_in edges to place nodes + birthday/how_met facts, for non-deleted contacts) "+
			"via the validated assertion write path with each contact's created_at as the "+
			"knowledge time. Idempotent; counts-only output. Cache columns are retained.")
	fs.BoolVar(&opts.riverTier0, "river-tier0", false,
		"One-shot wait/run-by-kind read over live river_job rows finalized in the last "+
			"--window-hours. An APPROXIMATE first signal before job_exec_sample accrues; do NOT "+
			"compare it numerically to the Tier-1 queue_wait_ms metric. Read-only; safe with crm-api running.")
	fs.IntVar(&opts.windowHours, "window-hours", 24,
		"Look-back window in hours for --river-tier0 (default 24; must be >= 1).")
	if err := fs.Parse(args); err != nil {
		return runOptions{}, err
	}
	return opts, nil
}

// migrateExitPending is the exit code --migrate-check returns when app and/or
// River migrations are pending. It is the contract deploy-artifact.sh branches
// on (exit 2 ⇒ snapshot-then-migrate); distinct from operational errors (exit 1)
// so the deploy script never confuses "pending" with "abort".
const migrateExitPending = 2

// migrationStatusFn is the read-only migration-status reporter --migrate-check
// depends on. Production passes db.MigrationStatus; tests pass a stub so the
// exit-code mapping is unit-testable without a real database.
type migrationStatusFn func(ctx context.Context, databaseURL, migrationsPath string) (appPending, riverPending bool, err error)

// runMigrate applies pending application + River migrations idempotently (the
// same call crm-api boot makes). Returns nil on success (exit 0) or a plain
// error (exit 1). db.RunMigrations swallows ErrNoChange, so re-running on an
// up-to-date DB is a clean no-op.
func runMigrate(ctx context.Context, databaseURL, migrationsPath string, stdout io.Writer) error {
	if err := db.RunMigrations(ctx, databaseURL, migrationsPath); err != nil {
		return fmt.Errorf("run migrations: %w", err)
	}
	if _, err := fmt.Fprintln(stdout, "migrations applied (or already up-to-date)"); err != nil {
		return fmt.Errorf("write migrate summary: %w", err)
	}
	return nil
}

// runMigrateCheck reports whether migrations are pending WITHOUT mutating the
// DB. It returns:
//   - nil (exit 0) when up-to-date,
//   - exitErr{code:2} (exit 2) when app and/or River migrations are pending,
//   - a plain error (exit 1) on any operational failure (cannot connect, dirty
//     DB, version read failed).
//
// The status reporter is injected so the exit-code mapping is unit-testable;
// production passes db.MigrationStatus.
func runMigrateCheck(ctx context.Context, databaseURL, migrationsPath string, stdout io.Writer) error {
	return runMigrateCheckWith(ctx, databaseURL, migrationsPath, stdout, db.MigrationStatus)
}

// runMigrateCheckWith is the injectable core of runMigrateCheck. It prints a
// counts-only summary (no PII) and maps the boolean status to the exit-code
// contract.
func runMigrateCheckWith(ctx context.Context, databaseURL, migrationsPath string, stdout io.Writer, status migrationStatusFn) error {
	appPending, riverPending, err := status(ctx, databaseURL, migrationsPath)
	if err != nil {
		return fmt.Errorf("migration check: %w", err)
	}

	appCount, riverCount := 0, 0
	if appPending {
		appCount = 1
	}
	if riverPending {
		riverCount = 1
	}
	if _, werr := fmt.Fprintf(stdout, "migrate-check: app_pending=%d river_pending=%d\n", appCount, riverCount); werr != nil {
		return fmt.Errorf("write migrate-check summary: %w", werr)
	}

	if appPending || riverPending {
		return exitErr{code: migrateExitPending, msg: "migrations pending"}
	}
	return nil
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
	case opts.rederiveCorrespondence:
		return runRederiveCorrespondenceNames(ctx, deps)
	case opts.resetGmailBackfill:
		return runResetGmailBackfillCursors(ctx, deps)
	case opts.doSeed:
		return runSeed(ctx, opts, deps)
	case opts.resetAndSeed:
		return runResetAndSeed(ctx, opts, deps)
	case opts.listJobs:
		return runListJobs(ctx, opts, deps)
	case opts.retryJobID != 0:
		return runRetryJob(ctx, opts, deps)
	case opts.migrateTags:
		return runMigrateTags(ctx, deps)
	case opts.migrateContactKnowledge:
		return runMigrateContactKnowledge(ctx, deps)
	case opts.riverTier0:
		return runRiverTier0(ctx, opts, deps)
	case opts.migrate, opts.migrateCheck:
		// Migration subcommands are dispatched PRE-DB in runMain (they need
		// only the database URL + migrations path, not deps), so they never
		// reach this DB-backed dispatcher. Guard against a future caller that
		// drives run() directly with these set rather than falling through to
		// the misleading "unreachable".
		return errors.New("migration subcommands are dispatched pre-DB in runMain, not run()")
	}
	return errors.New("unreachable")
}

// seedConfirmEnv is the env-var alternative to --yes for the mandatory
// seed/reset confirmation gate.
const seedConfirmEnv = "CRM_SEED_RESET_CONFIRM"

// seedConfirmed reports whether the mandatory seed/reset confirmation is present
// (the --yes flag OR CRM_SEED_RESET_CONFIRM=1). Both --seed and --reset-and-seed
// require it — the seed Starts a competing River worker pool, so neither is a
// no-confirm operation. The operator/script asserts the crm-api service is
// stopped by passing it.
func seedConfirmed(opts runOptions) bool {
	return opts.seedYes || os.Getenv(seedConfirmEnv) == "1"
}

// requireSeedConfirm returns a command-specific error when the mandatory
// confirmation is absent. Called PRE-migration in runMain (so a mistaken reset
// never applies migrations) and again as a backstop in the run-dispatch path
// (which tests drive directly).
func requireSeedConfirm(opts runOptions) error {
	if seedConfirmed(opts) {
		return nil
	}
	if opts.resetAndSeed {
		return fmt.Errorf("--reset-and-seed requires confirmation: pass --yes (or set %s=1) AND ensure the crm-api service is stopped — this HARD-wipes all data", seedConfirmEnv)
	}
	return fmt.Errorf("--seed requires confirmation: pass --yes (or set %s=1) AND ensure the crm-api service is stopped", seedConfirmEnv)
}

// resolveSeedParams builds the SeedParams for a seed/reset run: the profile
// (defaulting per command), the stable namespace (overridable), and the PRNG
// seed (overridable). defaultProfile is `dev` for --seed, `prod-shaped` for
// --reset-and-seed.
func resolveSeedParams(opts runOptions, defaultProfile synthetic.Profile) (synthetic.SeedParams, error) {
	profile := defaultProfile
	if opts.seedProfile != "" {
		profile = synthetic.Profile(opts.seedProfile)
	}
	params, err := synthetic.ProfileParams(profile)
	if err != nil {
		return synthetic.SeedParams{}, err
	}
	if opts.seedNamespace != "" {
		params.Namespace = opts.seedNamespace
	}
	if opts.prngSeed != 0 {
		params.Seed = opts.prngSeed
	}
	// Validate the namespace HERE — before any caller does destructive work.
	// NewHarness* validates at construction, but --reset-and-seed truncates every
	// live data table BEFORE the harness is built, so a late rejection would leave
	// the DB wiped and unseeded. Failing fast here keeps the truncate from running
	// on an invalid namespace.
	if err := synthetic.ValidateNamespace(params.Namespace); err != nil {
		return synthetic.SeedParams{}, err
	}
	return params, nil
}

// runSeed runs the additive synthetic seed. The CRM_ENV gate + migrations run
// PRE-this in runMain; the additive queue-drain preflight + harness lifecycle are
// inside deps.seed.Seed (the production seedAdapter).
func runSeed(ctx context.Context, opts runOptions, deps adminDeps) error {
	if err := requireSeedConfirm(opts); err != nil {
		return err
	}
	params, err := resolveSeedParams(opts, synthetic.ProfileDev)
	if err != nil {
		return fmt.Errorf("--seed: %w", err)
	}
	res, err := deps.seed.Seed(ctx, params)
	if err != nil {
		writeSeedSummaryOnError(deps.stdout, res, err)
		return fmt.Errorf("seed: %w", err)
	}
	return writeSeedSummary(deps.stdout, res, false)
}

// runResetAndSeed hard-wipes the live data tables then reseeds. The CRM_ENV gate
// + migrations run PRE-this in runMain; the wipe + harness lifecycle are inside
// deps.seed.ResetAndSeed.
func runResetAndSeed(ctx context.Context, opts runOptions, deps adminDeps) error {
	if err := requireSeedConfirm(opts); err != nil {
		return err
	}
	params, err := resolveSeedParams(opts, synthetic.ProfileProdShaped)
	if err != nil {
		return fmt.Errorf("--reset-and-seed: %w", err)
	}
	res, err := deps.seed.ResetAndSeed(ctx, params)
	if err != nil {
		writeSeedSummaryOnError(deps.stdout, res, err)
		return fmt.Errorf("reset-and-seed: %w", err)
	}
	return writeSeedSummary(deps.stdout, res, false)
}

// errSeedWorldIntact marks a failure that happened AFTER the profile finished
// seeding — the world is complete and was NOT torn down. Such a run must not be
// labelled PARTIAL: an operator who reads "run failed" against a good staging
// world re-runs --reset-and-seed, which wipes it. That is the inverse of the
// mistake the marker exists to prevent, and the more expensive one.
var errSeedWorldIntact = errors.New("seed completed; a post-seed step failed")

// writeSeedSummaryOnError prints the summary for a seed run that returned an
// error. It marks the summary PARTIAL only when the PROFILE failed — a
// post-seed failure (errSeedWorldIntact) leaves a complete world, so it prints
// as an ordinary summary and the returned error carries the bad news.
//
// It prints nothing when the run never reached the profile (a harness-build or
// preflight failure leaves a zero result, and a summary of nothing is noise),
// and it deliberately swallows its own write error: the caller is already
// returning the seed failure, which is the error that matters.
func writeSeedSummaryOnError(w io.Writer, res synthetic.ProfileResult, err error) {
	if res.Timings.Total == 0 {
		return
	}
	_ = writeSeedSummary(w, res, !errors.Is(err, errSeedWorldIntact))
}

// writeSeedSummary prints the counts-only seed summary (no PII). The bare
// namespace is printed (NOT the synth-<ns>- prefix — an internal detail).
// partial marks a run that FAILED: the counts and timings are whatever had
// accumulated, and the header says so, so a partial summary is never mistaken
// for a successful run.
func writeSeedSummary(w io.Writer, res synthetic.ProfileResult, partial bool) error {
	marker := ""
	if partial {
		marker = " (PARTIAL — run failed)"
		if res.Timings.Current != "" {
			marker = fmt.Sprintf(" (PARTIAL — run failed during phase %q)", res.Timings.Current)
		}
	}
	if _, err := fmt.Fprintf(w, "seed summary (profile=%s namespace=%s prng_seed=%d)%s:\n"+
		"  contacts:             %d\n"+
		"  gmail_settled:        %d\n"+
		"  telegram_settled:     %d\n"+
		"  gcal_settled:         %d\n"+
		"  gchat_settled:        %d\n"+
		"  imessage_settled:     %d\n"+
		"  unmatched_external:   %d\n"+
		"  stranded_telegram:    %d\n"+
		"  stranded_messages:    %d\n"+
		"  unmatched_calendar:   %d\n"+
		"  orphan_meeting_notes: %d\n"+
		// Three archetype figures in three different UNITS, which is why they are
		// reported separately rather than reconciled: payloads driven, interaction
		// rows landed (smaller by design — aggregation collapses a promotion pair
		// into one mutual and a burst into one session), and settle gates waited on
		// (one per dependency generation, not per payload — the phase's real cost).
		"  archetype_payloads:   %d\n"+
		"  archetype_rows:       %d\n"+
		"  archetype_settles:    %d\n",
		res.Profile, res.Namespace, res.Seed, marker,
		res.Contacts, res.GmailSettled, res.TelegramSettled, res.GCalSettled, res.GChatSettled,
		res.IMessageSettled, res.UnmatchedExternal, res.StrandedTelegram, res.StrandedMessages,
		res.UnmatchedCalendar, res.OrphanMeetingNote,
		res.ArchetypePayloads, res.ArchetypeInteractions, res.ArchetypeSettleCalls); err != nil {
		return fmt.Errorf("write seed summary: %w", err)
	}
	if err := writeSeedTimings(w, res.Timings); err != nil {
		return err
	}
	if !partial {
		return nil
	}
	// A failed profile run is torn down before this prints, so the counts above
	// describe what HAD been seeded, not rows an operator can go look at. Without
	// this they read as current DB state.
	if _, err := fmt.Fprintf(w,
		"  note: the partial world these counts describe has been torn down "+
			"(unless the error reports that teardown also failed).\n"); err != nil {
		return fmt.Errorf("write seed summary note: %w", err)
	}
	return nil
}

// writeSeedTimings prints the wall-clock block: total duration, the settle
// accounting, and every phase with its payload volume.
//
// The attribution model is NESTED, and the summary says so rather than inviting
// a wrong reading: a phase timer ENCOMPASSES the Settle calls made inside it,
// and the gate timers nest inside those. So `outside_gates` (duration minus
// total gate wait) is close to an accounting identity — PostgreSQL and River
// work performed WHILE a gate waits is already counted as gate wait — and it is
// labelled as bookkeeping, not evidence. What is actually falsifiable is
// gate_polls: waitGateA evaluates its predicate BEFORE the first sleep, so a
// high inline-hit rate with low poll counts means gate wait is genuine worker/DB
// throughput, while a low rate with many polls is latency that batching removes.
func writeSeedTimings(w io.Writer, t synthetic.SeedTimings) error {
	s := t.Settle
	gateTotal := s.GateAWait + s.GateBWait + s.CaptureWait
	avgGateBPolls := 0.0
	if s.Calls > 0 {
		avgGateBPolls = float64(s.GateBPolls) / float64(s.Calls)
	}
	if _, err := fmt.Fprintf(w,
		"  duration:             %s\n"+
			"  settle_calls:         %d\n"+
			"  gate_wait:            gate_a=%s gate_b=%s capture=%s   (nested inside phases)\n"+
			"  gate_polls:           gate_a=inline-hits %d/%d  gate_b=polls %d (avg %.1f/call)\n"+
			"  outside_gates:        %s   (bookkeeping: duration − gate wait; NOT a hypothesis test)\n"+
			"  phases (%d):\n",
		formatSeedDuration(t.Total),
		s.Calls,
		formatSeedDuration(s.GateAWait), formatSeedDuration(s.GateBWait), formatSeedDuration(s.CaptureWait),
		s.GateAInlineHits, s.GateACalls, s.GateBPolls, avgGateBPolls,
		formatSeedDuration(t.Total-gateTotal),
		len(t.Phases)); err != nil {
		return fmt.Errorf("write seed timings: %w", err)
	}
	for _, p := range t.Phases {
		// A phase that seeds no source payloads prints `-`, so "none by design"
		// reads differently from "expected payloads, got none".
		payloads := "-"
		switch {
		case p.Payloads == 1:
			payloads = "1 payload"
		case p.Payloads > 1:
			payloads = fmt.Sprintf("%d payloads", p.Payloads)
		}
		if _, err := fmt.Fprintf(w, "    %-26s %9s   %s\n",
			p.Name, formatSeedDuration(p.Duration), payloads); err != nil {
			return fmt.Errorf("write seed phase timing: %w", err)
		}
	}
	return nil
}

// formatSeedDuration renders a duration as plain seconds. Duration.String would
// switch units across the ranges these numbers span ("1m12.97s" vs "412ms"),
// which makes a phase table impossible to scan or diff.
func formatSeedDuration(d time.Duration) string {
	return fmt.Sprintf("%.2fs", d.Seconds())
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

// runMigrateTags mirrors the legacy tag / contact_tag tables into the graph and
// prints a counts-only summary. Returns a non-nil error on a case-insensitive
// tag-name collision (nothing is written) so the operator dedups the legacy tags
// and re-runs; otherwise the run is idempotent.
func runMigrateTags(ctx context.Context, deps adminDeps) error {
	res, err := deps.migrateTags.MigrateTags(ctx)
	if err != nil {
		return fmt.Errorf("migrate tags: %w", err)
	}
	if _, err := fmt.Fprintf(deps.stdout, "migrate-tags summary:\n"+
		"  tags:                          %d\n"+
		"  tag_nodes_created:             %d\n"+
		"  tag_nodes_existing:            %d\n"+
		"  contact_tags_migrated:         %d\n"+
		"  contact_tags_skipped_deleted:  %d\n"+
		"  assertions_asserted:           %d\n",
		res.Tags, res.TagNodesCreated, res.TagNodesExisting, res.ContactTags, res.SkippedDeletedContacts, res.AssertionsAsserted); err != nil {
		return fmt.Errorf("write summary: %w", err)
	}
	return nil
}

// runMigrateContactKnowledge mirrors the legacy contact.location/birthday/how_met
// cache columns into the graph (via AssertService) and prints a counts-only
// summary. Idempotent: a re-run corroborates rather than duplicates.
func runMigrateContactKnowledge(ctx context.Context, deps adminDeps) error {
	res, err := deps.migrateContactKnowledge.MigrateContactKnowledgeColumns(ctx)
	if err != nil {
		return fmt.Errorf("migrate contact knowledge columns: %w", err)
	}
	if _, err := fmt.Fprintf(deps.stdout, "migrate-contact-knowledge-columns summary:\n"+
		"  contacts_scanned:    %d\n"+
		"  locations_migrated:  %d\n"+
		"  birthdays_migrated:  %d\n"+
		"  how_met_migrated:    %d\n",
		res.Contacts, res.LocationsMigrated, res.BirthdaysMigrated, res.HowMetMigrated); err != nil {
		return fmt.Errorf("write summary: %w", err)
	}
	return nil
}

// maxTier0WindowHours bounds --window-hours (10 years) so an extreme value
// can't overflow the int64 duration arithmetic that computes the cutoff.
const maxTier0WindowHours = 24 * 365 * 10

// runRiverTier0 prints a one-shot wait/run-by-kind read over live river_job
// rows finalized in the last --window-hours. The cutoff is computed in Go from
// accelerated time (NOT SQL NOW()) so the window tracks the running app's clock.
// The output is an APPROXIMATE first signal (its wait is attempted_at -
// scheduled_at over the last attempt only, and River mutates scheduled_at on
// retry), so it must NOT be compared numerically to the Tier-1 queue_wait_ms
// metric — the header says so.
func runRiverTier0(ctx context.Context, opts runOptions, deps adminDeps) error {
	if opts.windowHours < 1 || opts.windowHours > maxTier0WindowHours {
		return fmt.Errorf("--window-hours must be between 1 and %d (got %d)", maxTier0WindowHours, opts.windowHours)
	}
	cutoff := accelerated.GetCurrentTime().Add(-time.Duration(opts.windowHours) * time.Hour)

	rows, err := deps.tier0.Tier0StatsByKind(ctx, cutoff)
	if err != nil {
		return fmt.Errorf("river tier0 stats: %w", err)
	}

	if _, err := fmt.Fprintf(deps.stdout,
		"river-tier0 (last %dh; APPROXIMATE — do NOT compare to Tier-1 queue_wait_ms):\n",
		opts.windowHours); err != nil {
		return fmt.Errorf("write tier0 header: %w", err)
	}
	if len(rows) == 0 {
		if _, err := fmt.Fprintln(deps.stdout, "  no finished jobs in window"); err != nil {
			return fmt.Errorf("write tier0 empty: %w", err)
		}
		return nil
	}
	for _, row := range rows {
		if _, err := fmt.Fprintf(deps.stdout,
			"  kind=%s n=%d p50_wait_s=%.3f p50_run_s=%.3f\n",
			row.Kind, row.N, row.P50WaitS, row.P50RunS); err != nil {
			return fmt.Errorf("write tier0 row: %w", err)
		}
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

func runResetGmailBackfillCursors(ctx context.Context, deps adminDeps) error {
	res, err := deps.gmailReset.ResetGmailBackfillCursors(ctx)
	if err != nil {
		return fmt.Errorf("reset gmail backfill cursors: %w", err)
	}
	if _, err := fmt.Fprintf(deps.stdout, "reset-gmail-backfill-cursors summary:\n"+
		"  scanned: %d\n"+
		"  reset:   %d\n",
		res.Scanned, res.Reset); err != nil {
		return fmt.Errorf("write summary: %w", err)
	}
	return nil
}

// jobListMaxLimit is the upper bound River's JobListParams.First enforces (it
// panics outside 1..10000). We validate --job-limit against it BEFORE calling
// First so the operator gets a friendly usage error instead of a panic.
const jobListMaxLimit = 10_000

// parseJobStates splits a comma-separated state filter into rivertype.JobState
// values, validating each token against River's known states. Whitespace around
// tokens is trimmed; an empty filter is rejected. An unknown token returns a
// usage error listing the valid states.
func parseJobStates(filter string) ([]rivertype.JobState, error) {
	valid := rivertype.JobStates()
	validSet := make(map[string]rivertype.JobState, len(valid))
	for _, s := range valid {
		validSet[string(s)] = s
	}
	var states []rivertype.JobState
	for _, tok := range strings.Split(filter, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		st, ok := validSet[tok]
		if !ok {
			validNames := make([]string, len(valid))
			for i, s := range valid {
				validNames[i] = string(s)
			}
			return nil, fmt.Errorf("--job-state: unknown state %q (valid: %s)", tok, strings.Join(validNames, ", "))
		}
		states = append(states, st)
	}
	if len(states) == 0 {
		return nil, errors.New("--job-state: at least one state required")
	}
	return states, nil
}

// runListJobs lists River jobs (one key=value row per job) for dead-letter
// inspection. Read-only; safe with crm-api running. Newest job first on the
// never-null, deterministic id key (finalized_at is NULL for retryable rows, so
// it can't sort the mixed default view).
func runListJobs(ctx context.Context, opts runOptions, deps adminDeps) error {
	states, err := parseJobStates(opts.jobState)
	if err != nil {
		return err
	}
	if opts.jobLimit < 1 || opts.jobLimit > jobListMaxLimit {
		return fmt.Errorf("--job-limit must be between 1 and %d (got %d)", jobListMaxLimit, opts.jobLimit)
	}

	params := river.NewJobListParams().
		States(states...).
		First(opts.jobLimit).
		OrderBy(river.JobListOrderByID, river.SortOrderDesc)

	res, err := deps.jobs.JobList(ctx, params)
	if err != nil {
		return fmt.Errorf("list jobs: %w", err)
	}

	if len(res.Jobs) == 0 {
		if _, err := fmt.Fprintf(deps.stdout, "no jobs found (states=%s)\n", opts.jobState); err != nil {
			return fmt.Errorf("write empty list: %w", err)
		}
		return nil
	}

	for _, job := range res.Jobs {
		if err := writeJobRow(deps.stdout, job); err != nil {
			return err
		}
	}

	// A full page may mean older rows exist beyond the limit.
	if len(res.Jobs) == opts.jobLimit {
		if _, err := fmt.Fprintf(deps.stdout,
			"note: limit reached (%d rows shown); older rows may exist — raise --job-limit\n",
			opts.jobLimit); err != nil {
			return fmt.Errorf("write limit note: %w", err)
		}
	}
	return nil
}

// writeJobRow prints a single River job as a key=value row (runListHosts style).
// last_error is the Error of the most recent attempt (empty if none), printed
// with %q so a multi-line worker error stays on one line. args is the raw
// EncodedArgs (compact JSON). finalized_at is "none" when nil.
func writeJobRow(w io.Writer, job *rivertype.JobRow) error {
	lastError := ""
	if n := len(job.Errors); n > 0 {
		lastError = job.Errors[n-1].Error
	}
	finalizedAt := "none"
	if job.FinalizedAt != nil {
		finalizedAt = job.FinalizedAt.UTC().Format(time.RFC3339)
	}
	args := string(job.EncodedArgs)
	if args == "" {
		args = "{}"
	}
	if _, err := fmt.Fprintf(w,
		"id=%d kind=%s state=%s attempt=%d/%d queue=%s created_at=%s finalized_at=%s last_error=%q args=%s\n",
		job.ID, job.Kind, job.State, job.Attempt, job.MaxAttempts, job.Queue,
		job.CreatedAt.UTC().Format(time.RFC3339), finalizedAt, lastError, args); err != nil {
		return fmt.Errorf("write job row: %w", err)
	}
	return nil
}

// runRetryJob makes a single River job available to run again (by numeric id).
// On an exhausted job River bumps max_attempts so it can run once more. Safe
// with crm-api running — the live worker pool picks it up.
func runRetryJob(ctx context.Context, opts runOptions, deps adminDeps) error {
	if opts.retryJobID < 1 {
		return fmt.Errorf("--retry-job must be a positive job id (got %d)", opts.retryJobID)
	}
	job, err := deps.jobs.JobRetry(ctx, opts.retryJobID)
	if err != nil {
		if errors.Is(err, rivertype.ErrNotFound) {
			return fmt.Errorf("no job with id %d (already cleaned by retention?)", opts.retryJobID)
		}
		return fmt.Errorf("retry job %d: %w", opts.retryJobID, err)
	}
	if _, err := fmt.Fprintf(deps.stdout,
		"retried job id=%d kind=%s state=%s scheduled_at=%s\n",
		job.ID, job.Kind, job.State, job.ScheduledAt.UTC().Format(time.RFC3339)); err != nil {
		return fmt.Errorf("write retry summary: %w", err)
	}
	return nil
}

// correspondenceBackfillFloor is the lower bound for the one-time display-name
// re-derivation. It matches the Gmail onboarding backfill floor (the earliest
// mail the integration ever ingested), so the catchup covers the entire stored
// backlog.
const correspondenceBackfillFloor = "2026-01-01"

// runRederiveCorrespondenceNames runs the one-time historical display-name
// re-derivation over the full range: re-fetch already-ingested email messages
// and additively backfill participant display names into comms_message
// metadata (preserving all existing content + provenance). Candidate discovery
// now runs in-sync on the Gmail fetch loop (between fetch and storage), not
// here, so this subcommand no longer runs a producer pass. Prints a counts-only
// summary; returns a non-nil error iff any row FAILED (Skipped* outcomes do NOT
// fail the run), so the operator can fix the cause and re-run safely
// (idempotent).
func runRederiveCorrespondenceNames(ctx context.Context, deps adminDeps) error {
	since, err := time.Parse("2006-01-02", correspondenceBackfillFloor)
	if err != nil {
		return fmt.Errorf("parse backfill floor: %w", err)
	}

	res, err := deps.rederive.RederiveNames(ctx, since)
	if err != nil {
		return fmt.Errorf("rederive correspondence names: %w", err)
	}

	if _, err := fmt.Fprintf(deps.stdout, "rederive-correspondence-names summary:\n"+
		"  scanned:              %d\n"+
		"  rederived:            %d\n"+
		"  skipped_no_gmail_id:  %d\n"+
		"  skipped_unavailable:  %d\n"+
		"  failed:               %d\n",
		res.Scanned, res.Rederived, res.SkippedNoGmailID, res.SkippedUnavailable, res.Failed); err != nil {
		return fmt.Errorf("write summary: %w", err)
	}
	if res.Failed > 0 {
		return fmt.Errorf("%d row(s) failed to re-derive (see logs); re-run after fixing the cause", res.Failed)
	}
	return nil
}

// buildProductionDeps wires up the production service stack. Returns
// a cleanup function to call before exit (releases the river client). opts
// gates lazily-built dependencies: the correspondence re-derivation stack
// requires Google OAuth and is built ONLY when --rederive-correspondence-names
// is set, so every other subcommand keeps working on a Google-less host.
func buildProductionDeps(ctx context.Context, cfg *config.Config, database *db.Database, opts runOptions) (adminDeps, func(), error) {
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
	// AssertService.Assert (used by --migrate-tags + --migrate-contact-knowledge-
	// columns) publishes assertion.accepted/superseded, which now enqueue a
	// knowledge_cache_updater job — register the kind so the insert-only client
	// accepts it (the cache refresh runs in the crm-api worker, not here).
	river.AddWorker(workers, &noopKnowledgeCacheWorker{})
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

	// Graph repos + AssertService + knowledge-cache updater. Reuses the same
	// event bus (so the tagged_as asserts emit assertion.* events); AssertService
	// owns proposition identity / dedup, which makes --migrate-tags idempotent.
	// Built ABOVE NewEnrichmentService so the knowledge-writer pair can be passed
	// as constructor args: the address-book reconcile path
	// (--reconcile-address-book-methods → EnrichContactFromExternal) persists an
	// inferred location/birthday through the assertion store + refreshes the cache
	// inline, instead of erroring on the now-required knowledge writer.
	graphNodeRepo := repository.NewNodeRepository(database.Queries)
	graphEntityRepo := repository.NewEntityRepository(database.Queries)
	graphPredicateRepo := repository.NewPredicateRepository(database.Queries)
	graphAssertionRepo := repository.NewAssertionRepository(database.Queries)
	assertService := service.NewAssertService(
		database.Pool, graphNodeRepo, graphEntityRepo, graphPredicateRepo, graphAssertionRepo, eventBus,
	)
	knowledgeCacheUpdater := consumer.NewKnowledgeCacheUpdater(graphAssertionRepo, graphNodeRepo, contactRepo)

	// crm-admin never wires enrichment cadence (its paths never pass a cadence
	// override), so cadence is nil — preserving today's unset-cadence behavior.
	enrichmentService := service.NewEnrichmentService(
		database, contactRepo, contactMethodRepo, enrichmentRepo, eventBus, rematchService,
		nil, assertService, knowledgeCacheUpdater,
	)
	addressBookReconcile := service.NewAddressBookReconcileService(
		enrichmentService, contactRepo, contactMethodRepo, externalContactRepo,
	)

	tagMigration := service.NewTagMigrationService(
		database.Pool, repository.NewTagRepository(database.Queries),
		graphNodeRepo, graphEntityRepo, assertService,
	)
	contactKnowledgeMigration := service.NewContactKnowledgeMigrationService(
		database.Pool, contactRepo, assertService,
	)

	deps := adminDeps{
		tokens:     hostService,
		hosts:      hostService,
		revoker:    hostService,
		rematch:    rematch,
		reconcile:  addressBookReconcile,
		gmailReset: service.NewEmailBackfillCursorResetService(syncRepo),
		seed: seedAdapter{
			database: database,
			support:  repository.NewSyntheticSupportRepository(database.Queries),
		},
		// --list-jobs / --retry-job: the insert-only river client also satisfies
		// JobList / JobRetry (both go through the driver, no worker pool needed).
		jobs:                    riverClient,
		migrateTags:             tagMigration,
		migrateContactKnowledge: contactKnowledgeMigration,
		// --river-tier0: read-only wait/run-by-kind over live river_job.
		tier0: repository.NewJobSampleRepository(database.Queries),
	}

	// LAZY: build the correspondence re-derivation stack ONLY for
	// --rederive-correspondence-names. google.NewOAuthService errors when
	// Google creds are absent; building it unconditionally would break every
	// other subcommand on a Google-less host. The Gmail provider is
	// constructed with the real event bus + pool, but the re-derive seams
	// (RefetchParticipantNames, MeSet) never publish, so no email events are
	// emitted by this one-shot binary.
	if opts.rederiveCorrespondence {
		oauthRepo := repository.NewOAuthRepository(database.Queries)
		syncRepo := repository.NewSyncRepository(database.Queries)
		oauthService, err := google.NewOAuthService(cfg, oauthRepo, syncRepo)
		if err != nil {
			return adminDeps{}, nil, fmt.Errorf("--rederive-correspondence-names requires Google OAuth credentials: %w", err)
		}
		commsRepo := repository.NewCommsMessageRepository(database.Queries)
		gmailProvider := google.NewGmailSyncProvider(oauthService, commsRepo, eventBus, database.Pool)
		deps.rederive = google.NewCorrespondenceNameRederiveService(commsRepo, gmailProvider)
	}

	cleanup := func() {}
	return deps, cleanup, nil
}

// seedAdapter implements seedRunner over the synthetic toolkit. It owns the
// harness lifecycle the entrypoints require: build for the stable namespace, run
// the profile, and on SUCCESS Quiesce (stop the River client, LEAVE the rows);
// on ERROR run the full teardown (stop the client + clean the partial world), so
// a failed seed is never a leave-behind and the client is never leaked. The
// service is assumed STOPPED (asserted by --yes + the wrapper scripts), so the
// harness's River client is the sole client on the default queue.
type seedAdapter struct {
	database *db.Database
	support  *repository.SyntheticSupportRepository
}

// Seed is the additive path: it REFUSES if the river_job queue is not drained
// (an additive seed must not steal pre-existing work), then runs the profile.
func (a seedAdapter) Seed(ctx context.Context, params synthetic.SeedParams) (synthetic.ProfileResult, error) {
	nonFinal, err := a.support.CountNonFinalRiverJobs(ctx)
	if err != nil {
		return synthetic.ProfileResult{}, fmt.Errorf("count non-final river jobs: %w", err)
	}
	if nonFinal > 0 {
		return synthetic.ProfileResult{}, fmt.Errorf(
			"refusing additive --seed: %d queued/in-flight river_job row(s) — the queue must be drained "+
				"(is the crm-api service stopped?). Use --reset-and-seed for a clean baseline (it wipes river_job)",
			nonFinal)
	}
	return a.runProfile(ctx, params)
}

// ResetAndSeed hard-wipes the live data tables (incl. river_job — so it does NOT
// run the drain preflight) then reseeds the profile.
func (a seedAdapter) ResetAndSeed(ctx context.Context, params synthetic.SeedParams) (synthetic.ProfileResult, error) {
	if err := a.support.ResetSyntheticData(ctx); err != nil {
		return synthetic.ProfileResult{}, fmt.Errorf("reset synthetic data: %w", err)
	}
	return a.runProfile(ctx, params)
}

// runProfile builds the namespaced harness, runs the profile, and enforces the
// seed-and-leave (success) / full-teardown (error) lifecycle.
//
// On failure it returns the PARTIAL ProfileResult alongside the error rather
// than discarding it: which phase was running, how long the run had taken, and
// how many payloads had landed IS the diagnostic, and the entrypoints print it
// as a degraded summary. The error return is unchanged, so the exit code still
// reflects failure — this adds diagnostics, it swallows nothing.
func (a seedAdapter) runProfile(ctx context.Context, params synthetic.SeedParams) (synthetic.ProfileResult, error) {
	h, teardown, err := synthetic.NewHarnessWithDBForNamespace(ctx, a.database, params.Namespace, params.Seed)
	if err != nil {
		return synthetic.ProfileResult{}, fmt.Errorf("build harness: %w", err)
	}
	res, err := synthetic.RunProfile(ctx, h, params)
	if err != nil {
		// Failed seed: full teardown (stop client + clean the partial world).
		// Use a fresh context so a cancelled ctx still tears down. Surface a
		// teardown failure (e.g. Gate B did not clear, so cleanup was skipped and
		// the partial world is still standing) ALONGSIDE the seed error — silently
		// dropping it would hide that the "no leave-behind" guarantee was not met.
		if tdErr := teardown(context.Background()); tdErr != nil {
			return res, fmt.Errorf("seed failed: %w; AND partial-world teardown also failed (data may remain): %v", err, tdErr)
		}
		return res, err
	}
	// Success: Quiesce (stop client, LEAVE the rows). A Quiesce failure is NOT a
	// seed failure — the profile completed and teardown did not run, so the world
	// is intact. Tagging it errSeedWorldIntact keeps the summary out of the
	// PARTIAL branch; the error itself still fails the command.
	//
	// Quiesce returns nil unconditionally today, so this branch is unreachable
	// and has no test — the tag is here so the classification is already correct
	// if Quiesce ever grows a failure, rather than mislabelling a good world as a
	// failed run the day it does.
	if qErr := h.Quiesce(ctx); qErr != nil {
		return res, fmt.Errorf("%w: quiesce after seed: %w", errSeedWorldIntact, qErr)
	}
	return res, nil
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

// noopKnowledgeCacheWorker registers the knowledge_cache_updater kind so the
// insert-only admin River client accepts the enqueue that AssertService.Assert
// produces (assertion.accepted/superseded route a KnowledgeCacheUpdater job).
// --migrate-contact-knowledge-columns + --migrate-tags publish those events;
// the cache refresh itself runs in the always-on crm-api worker. Body is a no-op.
type noopKnowledgeCacheWorker struct {
	river.WorkerDefaults[consumerjobs.KnowledgeCacheUpdaterJobArgs]
}

func (w *noopKnowledgeCacheWorker) Work(_ context.Context, _ *river.Job[consumerjobs.KnowledgeCacheUpdaterJobArgs]) error {
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
