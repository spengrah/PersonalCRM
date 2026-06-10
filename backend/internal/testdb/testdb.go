//go:build integration_testdb

// Package testdb provides per-package template-database isolation for the
// backend integration test suite. The migrations are replayed once into a
// template database; each `go test` package process then gets its own fresh
// clone via `CREATE DATABASE ... TEMPLATE`, so cross-package contamination,
// live-River-worker bleed, and shared-pool contention disappear by
// construction.
//
// This package is compiled ONLY under the `integration_testdb` build tag (set
// by the Makefile integration targets via `-tags integration_testdb`). It is
// never compiled into production binaries (`go build ./cmd/crm-api` has no
// tag), satisfying the "no production source changes" guardrail.
//
// # Bounded raw-SQL exception (test-only)
//
// AGENTS.md rule #2 bans raw SQL in Go. The harness needs raw SQL because the
// operations below have NO sqlc representation: they run in the maintenance DB
// with no application schema, are admin/DDL statements with no result set, or
// use placeholder-forbidden identifiers. This is a narrow, explicitly
// enumerated, test-only exception, confined to internal/testdb behind the
// build tag, on dedicated admin/template pgx.Conn's (never the application
// pool). NOTHING ELSE in the harness may use raw SQL. The complete allow-list:
//
//	CREATE DATABASE <name> [TEMPLATE ...]    DDL in maintenance DB; identifier via pgx.Identifier{name}.Sanitize() after assertCreatableTestDBName
//	DROP DATABASE IF EXISTS <name> WITH (FORCE)  DDL in maintenance DB; identifier via Sanitize() after assertDroppableTestDBName (clone sweep)
//	DROP DATABASE IF EXISTS <name>  NON-FORCE DDL in maintenance DB; identifier via Sanitize() after assertDroppableTestDBName (template reaper)
//	SELECT pg_advisory_lock($1) / pg_advisory_unlock($1)  session function; parameterized; constant lock id
//	CREATE EXTENSION IF NOT EXISTS "uuid-ossp" / ... vector  DDL inside the template; static literal
//	CREATE TABLE _testdb_template_marker(...) / INSERT ... / SELECT hash ...  marker table exists only inside test DBs; static DDL; value parameterized via $1
//	SELECT 1 FROM pg_database WHERE datname = $1  system catalog read; parameterized
//	SELECT oid FROM pg_database WHERE datname = $1  system catalog read (test-only drop+recreate detection); parameterized
//	SELECT datname FROM pg_database WHERE datname LIKE $1 ESCAPE '\'  system catalog read (clone + template sweep); parameterized; escaped `_`
//	SELECT numbackends FROM pg_stat_database WHERE datname = $1  system catalog read (template reaper active-session count); parameterized
//
// Production query paths gain ZERO raw SQL.
package testdb

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"personal-crm/backend/internal/db"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
)

const (
	// baseDBName is the only base test database the harness will operate on.
	// The env URL MUST point at this database before any DDL runs.
	baseDBName = "personal_crm_test"

	// templatePrefix prefixes every per-migration-set template database. The
	// full name is templatePrefix + the first templateHashPrefixLen hex chars
	// of the migration content hash (27 + 32 = 59 ≤ Postgres's 63-char
	// identifier limit). Each migration set keeps its own template so divergent
	// branches/worktrees never contend over one name.
	templatePrefix = "personal_crm_test_template_"

	// templateHashPrefixLen is how many hex chars of the sha256 digest name the
	// template. 32 hex = 128 bits, collision-free across coexisting migration
	// sets. MUST stay in sync with the `{32}` quantifier in dbNamePattern: the
	// regex is the validation counterpart of this constant; change them
	// together (see the unit assertion in TestTemplateNameArithmetic).
	templateHashPrefixLen = 32

	// clonePrefix prefixes every per-package / per-test clone database.
	clonePrefix = "personal_crm_test_clone_"

	// markerTable is a one-row table inside the template recording the hash of
	// the migration + River inputs the template was built from.
	markerTable = "_testdb_template_marker"

	// maintenanceDBName is the maintenance database the admin connection
	// targets so CREATE/DROP DATABASE run outside any user schema.
	maintenanceDBName = "postgres"

	// templateBuildAdvisoryLockID serializes template build/check and clone
	// creation across the parallel `go test` package processes. Distinct from
	// River's migration lock (9230423_0001) in internal/db/migration.go.
	templateBuildAdvisoryLockID int64 = 9230423_0002
)

// dbNamePattern matches the only database names the harness may CREATE or DROP:
// hash-named templates (template_<32 lowercase hex>), and clones whose token is
// lowercase hex. The bare base personal_crm_test is intentionally NOT matched
// (we never CREATE or DROP it), and neither is the legacy bare
// personal_crm_test_template (the new harness never produces it; reclaim it
// manually on a dev box with `DROP DATABASE personal_crm_test_template`).
//
// The template suffix is pinned to exactly {32} hex — the validation
// counterpart of templateHashPrefixLen = 32. The two must change together.
var dbNamePattern = regexp.MustCompile(`^personal_crm_test_(template_[0-9a-f]{32}|clone_[0-9a-f]+)$`)

// templateNameFromHash returns the content-hash-named template DB name for a
// migration set. hash is the full 64-hex sha256 from templateHashFromInputs;
// only its first templateHashPrefixLen chars name the template, so two migration
// sets whose hashes differ within those chars get distinct templates and never
// contend. Panics if hash is shorter than the prefix length — an internal
// invariant that holds by construction (templateHashFromInputs always returns a
// 64-hex digest); the panic makes a future hash-shortening change fail loudly
// instead of slicing out of range. Never reachable from valid inputs.
func templateNameFromHash(hash string) string {
	if len(hash) < templateHashPrefixLen {
		panic(fmt.Sprintf("testdb: template hash %q shorter than prefix length %d", hash, templateHashPrefixLen))
	}
	return templatePrefix + hash[:templateHashPrefixLen]
}

// originalBaseURL captures the env database URL as it was at SetupPackage entry,
// BEFORE the package clone rewrite of DATABASE_URL. NewEphemeralClone derives
// its admin + clone URLs from this, never from the (rewritten) live env var.
// Set once by SetupPackage.
var originalBaseURL string

// migrationsPath is captured from SetupPackage's WithMigrationsPath option so
// NewEphemeralClone can rebuild the template if needed.
var migrationsPath string

// Option configures SetupPackage.
type Option func(*options)

type options struct {
	migrationsPath string
}

// WithMigrationsPath supplies the absolute path to the migrations directory for
// the calling package (tests vs tests/api differ by one ".." level).
func WithMigrationsPath(p string) Option {
	return func(o *options) { o.migrationsPath = p }
}

// SetupPackage is the per-package entrypoint. The caller's TestMain does:
//
//	func TestMain(m *testing.M) { os.Exit(testdb.SetupPackage(m, testdb.WithMigrationsPath(...))) }
//
// It ensures the template exists/current, clones a private database for this
// package process, rewrites DATABASE_URL to the clone (so all existing
// os.Getenv("DATABASE_URL") call sites transparently use it), runs the tests,
// then drops the clone. Returns the exit code for the caller to os.Exit.
//
// If neither DATABASE_URL nor TEST_DATABASE_URL is set, it skips cloning and
// runs the tests directly — the existing per-test DATABASE_URL-unset guards
// then self-skip the integration tests, exactly as today.
func SetupPackage(m *testing.M, opts ...Option) int {
	var o options
	for _, opt := range opts {
		opt(&o)
	}

	baseURL := envBaseURL()
	if baseURL == "" {
		// No DB configured: run as today; tests self-skip.
		return m.Run()
	}

	originalBaseURL = baseURL
	migrationsPath = o.migrationsPath

	ctx := context.Background()

	// Refuse to run any DDL unless the env base DB is exactly personal_crm_test.
	// A mis-set DATABASE_URL/TEST_DATABASE_URL pointing at the dev DB
	// (personal_crm) must fail loudly before any CREATE/DROP can execute.
	if err := assertSafeBaseURL(baseURL); err != nil {
		fmt.Fprintf(os.Stderr, "testdb: %v\n", err)
		return 1
	}

	// Ensure the template exists and matches the current migration + River
	// inputs. Acquires + releases the advisory lock internally.
	if err := ensureTemplate(ctx, baseURL, o.migrationsPath); err != nil {
		fmt.Fprintf(os.Stderr, "testdb: ensure template: %v\n", err)
		return 1
	}

	// Create this package's private clone under the advisory lock, after
	// re-verifying the template marker.
	cloneName, cloneConnURL, err := createCloneFromTemplate(ctx, baseURL, o.migrationsPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "testdb: create package clone: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "testdb: package clone created: %s\n", cloneName)

	// Rewrite DATABASE_URL so all downstream os.Getenv sites use the clone.
	if err := os.Setenv("DATABASE_URL", cloneConnURL); err != nil {
		fmt.Fprintf(os.Stderr, "testdb: set DATABASE_URL: %v\n", err)
		return 1
	}

	code := m.Run()

	// Best-effort drop of the package clone. Failure is logged, never fatal —
	// leaked clones are reaped by `make test-clean-clones`.
	if err := dropDatabase(ctx, baseURL, cloneName); err != nil {
		fmt.Fprintf(os.Stderr, "testdb: drop package clone %s (non-fatal): %v\n", cloneName, err)
	}

	return code
}

// NewEphemeralClone returns a fresh clone URL + cleanup func for a single
// test. Used by schema-mutation tests that roll the schema down/up: a mid-test
// failure dirties only this ephemeral clone, never the package clone or
// siblings. The clone is derived from originalBaseURL / the template, NOT from
// the (rewritten) package-clone DATABASE_URL.
func NewEphemeralClone(t testing.TB) (cloneConnURL string, drop func()) {
	t.Helper()
	if originalBaseURL == "" {
		t.Fatal("testdb.NewEphemeralClone: SetupPackage was not called with a configured DATABASE_URL/TEST_DATABASE_URL")
	}

	ctx := context.Background()
	cloneName, cloneConnURL, err := createCloneFromTemplate(ctx, originalBaseURL, migrationsPath)
	if err != nil {
		t.Fatalf("testdb.NewEphemeralClone: %v", err)
	}

	drop = func() {
		if err := dropDatabase(context.Background(), originalBaseURL, cloneName); err != nil {
			t.Logf("testdb.NewEphemeralClone: drop %s (non-fatal): %v", cloneName, err)
		}
	}
	return cloneConnURL, drop
}

// CleanClones is a thin backward-compatible shim over CleanStaleDatabases. With
// no migrations path it cannot exclude the current run's template from the
// reaper, but the advisory lock plus the per-template numbackends skip still
// protect any in-use template; at worst the current template is reaped and
// rebuilt next run (a cost, never a correctness problem).
func CleanClones() error {
	return CleanStaleDatabases("")
}

// CleanStaleDatabases sweeps leaked clones AND stale per-migration-set templates
// in a single pass. It is the explicit-sweep entrypoint invoked ONLY by
// `make test-clean-clones` via the standalone go-run cmd; it never runs during
// `go test`. The base personal_crm_test is never touched. Every drop is routed
// through assertDroppableTestDBName.
//
// Operating model: run ONLY when no integration tests are in flight. The
// advisory lock (held during the template drop pass) makes the reaper safe
// against an in-flight CREATE ... TEMPLATE copy, but NOT against a different
// worktree's still-running test process that may clone from a template LATER —
// the lock is released between that process's operations. That stronger
// cross-worktree-concurrent guarantee is out of scope (tracking #424).
//
// migrationsPath, when it resolves, names the current run's template so the
// reaper keeps it warm (never drops it). Pass "" to skip that exclusion.
//
// Returns a non-nil error only when a guarded drop the reaper ATTEMPTED
// actually failed; templates skipped because they are the current run's hash or
// have open backends are expected, logged skips — not errors. So the make
// target exits 0 when everything droppable was dropped and the rest legitimately
// skipped, non-zero only on a real drop failure.
func CleanStaleDatabases(migrationsPath string) error {
	baseURL := envBaseURL()
	if baseURL == "" {
		return errors.New("testdb.CleanStaleDatabases: DATABASE_URL/TEST_DATABASE_URL not set")
	}
	baseName, err := dbNameFromURL(baseURL)
	if err != nil {
		return fmt.Errorf("testdb.CleanStaleDatabases: parse base URL: %w", err)
	}
	if err := assertBaseDBName(baseName); err != nil {
		return fmt.Errorf("testdb.CleanStaleDatabases: %w", err)
	}

	ctx := context.Background()
	admin, err := connectAdmin(ctx, baseURL)
	if err != nil {
		return fmt.Errorf("testdb.CleanStaleDatabases: connect admin: %w", err)
	}
	defer func() { _ = admin.Close(ctx) }()

	// Sweep clones first, WITHOUT the advisory lock: clones are never a
	// CREATE ... TEMPLATE copy source, so no build/clone op can be racing them.
	cloneErr := sweepClones(ctx, admin)

	// Resolve the current run's template name so the reaper keeps it warm. If
	// the path can't be resolved, the exclusion is skipped (still safe).
	var currentTemplateName string
	if migrationsPath != "" {
		if currentHash, err := templateHashFromInputs(migrationsPath); err == nil {
			currentTemplateName = templateNameFromHash(currentHash)
		} else {
			fmt.Fprintf(os.Stderr, "testdb.CleanStaleDatabases: compute current template hash (skipping current-template exclusion): %v\n", err)
		}
	}

	candidates, listErr := listStaleTemplateCandidates(ctx, admin, currentTemplateName)
	if listErr != nil {
		return errors.Join(cloneErr, listErr)
	}
	dropped, reapErr := reapTemplates(ctx, admin, candidates, currentTemplateName)
	fmt.Fprintf(os.Stderr, "testdb.CleanStaleDatabases: dropped %d stale template(s) of %d candidate(s)\n", dropped, len(candidates))

	return errors.Join(cloneErr, reapErr)
}

// sweepClones lists and drops every leaked personal_crm_test_clone_* database on
// the admin connection. Not run under the advisory lock — clones are never a
// CREATE ... TEMPLATE copy source. Returns a non-nil error if any guarded clone
// drop failed.
func sweepClones(ctx context.Context, admin *pgx.Conn) error {
	// Pattern: clones are clonePrefix + hex. The literal `_` after `clone` is
	// escaped so it is not a LIKE single-char wildcard.
	pattern := `personal_crm_test_clone\_%`
	rows, err := admin.Query(ctx, `SELECT datname FROM pg_database WHERE datname LIKE $1 ESCAPE '\'`, pattern)
	if err != nil {
		return fmt.Errorf("testdb.sweepClones: list clones: %w", err)
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return fmt.Errorf("testdb.sweepClones: scan: %w", err)
		}
		names = append(names, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("testdb.sweepClones: rows: %w", err)
	}

	var dropped int
	var dropErrs []error
	for _, name := range names {
		if err := assertDroppableTestDBName(name); err != nil {
			// Defense in depth: the LIKE pattern should already exclude
			// non-clone names, but never drop a name the guard rejects. A
			// matching-but-rejected name is unexpected, so surface it.
			dropErrs = append(dropErrs, fmt.Errorf("skip non-droppable %q: %w", name, err))
			continue
		}
		if err := dropDatabaseConn(ctx, admin, name); err != nil {
			dropErrs = append(dropErrs, err)
			continue
		}
		dropped++
	}
	fmt.Fprintf(os.Stderr, "testdb.sweepClones: dropped %d leaked clone(s)\n", dropped)
	if len(dropErrs) > 0 {
		return fmt.Errorf("testdb.sweepClones: %d clone(s) could not be dropped: %w", len(dropErrs), errors.Join(dropErrs...))
	}
	return nil
}

// listStaleTemplateCandidates returns every hash-named template_<hex> database
// EXCEPT currentTemplateName. Pure listing — no drops, no lock — so it is safe
// to run under the parallel suite. The `_` chars in the LIKE pattern are escaped
// so they are not single-char wildcards.
func listStaleTemplateCandidates(ctx context.Context, admin *pgx.Conn, currentTemplateName string) ([]string, error) {
	pattern := `personal_crm_test_template\_%`
	rows, err := admin.Query(ctx, `SELECT datname FROM pg_database WHERE datname LIKE $1 ESCAPE '\'`, pattern)
	if err != nil {
		return nil, fmt.Errorf("testdb.listStaleTemplateCandidates: list templates: %w", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("testdb.listStaleTemplateCandidates: scan: %w", err)
		}
		if name == currentTemplateName {
			continue // keep the current run's template warm
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("testdb.listStaleTemplateCandidates: rows: %w", err)
	}
	return names, nil
}

// reapTemplates drops the stale templates in candidates, the entire pass running
// under templateBuildAdvisoryLockID — the SAME lock every build/clone holds, so
// no CREATE ... TEMPLATE copy can be in flight while it runs (the load-bearing
// safety mechanism; the numbackends skip below is only belt-and-suspenders).
//
// Per candidate, under the lock: skip if it is currentTemplateName
// (defense-in-depth even though the listing already excluded it); skip if it has
// open backends (a stray manual psql session — never force-terminated); else
// non-FORCE drop after assertDroppableTestDBName.
//
// Return-value contract: a skipped candidate (current-run hash or open backends)
// is NOT an error — it is an expected, logged skip. The returned error is
// non-nil ONLY when a guarded drop reapTemplates ATTEMPTED actually failed (an
// unexpected DROP pgerror, or a listed name failing assertDroppableTestDBName).
//
// Tests drive this directly with their own test-unique candidate slice so an
// automated reaper test never lists, considers, or drops a template it did not
// itself create — concurrent worktrees and sibling packages stay untouchable.
func reapTemplates(ctx context.Context, admin *pgx.Conn, candidates []string, currentTemplateName string) (dropped int, err error) {
	var dropErrs []error
	lockErr := withAdvisoryLock(ctx, admin, func() error {
		for _, name := range candidates {
			if name == currentTemplateName {
				continue // keep the current run's template warm
			}
			backends, err := templateActiveBackends(ctx, admin, name)
			if err != nil {
				dropErrs = append(dropErrs, err)
				continue
			}
			if backends > 0 {
				// A stray manual session is open against this template. Skip
				// (not an error) rather than force-terminate it: a template is a
				// potential copy source and a possibly-intentional session.
				fmt.Fprintf(os.Stderr, "testdb.reapTemplates: skipping %s (%d open backend(s))\n", name, backends)
				continue
			}
			if err := assertDroppableTestDBName(name); err != nil {
				// A LIKE-matched name failing the guard is unexpected; surface
				// it rather than dropping it.
				dropErrs = append(dropErrs, fmt.Errorf("skip non-droppable %q: %w", name, err))
				continue
			}
			if err := dropDatabaseNoForceConn(ctx, admin, name); err != nil {
				dropErrs = append(dropErrs, err)
				continue
			}
			dropped++
		}
		return nil
	})
	if lockErr != nil {
		dropErrs = append(dropErrs, lockErr)
	}
	if len(dropErrs) > 0 {
		return dropped, fmt.Errorf("testdb.reapTemplates: %d template(s) could not be dropped: %w", len(dropErrs), errors.Join(dropErrs...))
	}
	return dropped, nil
}

// templateActiveBackends returns the number of sessions connected to the named
// database, via the pg_stat_database catalog. A template mid-CREATE ... TEMPLATE
// copy has ZERO user backends (Postgres copies it without a separate user
// session), so this count is the secondary skip, NOT the gate that protects
// against the copy race — the advisory lock is.
func templateActiveBackends(ctx context.Context, conn *pgx.Conn, name string) (int, error) {
	var n int
	err := conn.QueryRow(ctx, `SELECT numbackends FROM pg_stat_database WHERE datname = $1`, name).Scan(&n)
	if errors.Is(err, pgx.ErrNoRows) {
		// No catalog row ⇒ the DB does not exist ⇒ zero backends.
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read active backends for %q: %w", name, err)
	}
	return n, nil
}

// dropDatabaseNoForceConn runs a NON-FORCE DROP DATABASE IF EXISTS after the
// droppable-name guard. Used ONLY by the template reaper: non-FORCE so that if a
// backend appears between the numbackends read and the drop, Postgres refuses
// rather than terminating a live session (a template is a potential copy source
// and a possibly-intentional manual session). The clone sweep keeps the FORCE
// variant (dropDatabaseConn); the two branches never cross-feed names.
func dropDatabaseNoForceConn(ctx context.Context, conn *pgx.Conn, name string) error {
	if err := assertDroppableTestDBName(name); err != nil {
		return err
	}
	stmt := "DROP DATABASE IF EXISTS " + pgx.Identifier{name}.Sanitize()
	if _, err := conn.Exec(ctx, stmt); err != nil {
		return fmt.Errorf("drop database %q: %w", name, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Template build
// ---------------------------------------------------------------------------

// ensureTemplate builds or refreshes the hash-named template DB under the
// advisory lock. On exit the template for this migration set exists and its
// marker equals the current wantHash.
func ensureTemplate(ctx context.Context, baseURL, migPath string) error {
	wantHash, err := templateHashFromInputs(migPath)
	if err != nil {
		return fmt.Errorf("compute template hash: %w", err)
	}
	templateName := templateNameFromHash(wantHash)

	admin, err := connectAdmin(ctx, baseURL)
	if err != nil {
		return fmt.Errorf("connect admin: %w", err)
	}
	defer func() { _ = admin.Close(ctx) }()

	return withAdvisoryLock(ctx, admin, func() error {
		exists, err := databaseExists(ctx, admin, templateName)
		if err != nil {
			return err
		}
		if !exists {
			return buildTemplate(ctx, admin, baseURL, templateName, migPath, wantHash)
		}

		gotHash, ok, err := readTemplateMarker(ctx, baseURL, templateName)
		if err != nil {
			return err
		}
		switch {
		case ok && gotHash == wantHash:
			// Reuse as-is: the template for this migration set is already built.
			return nil
		case !ok:
			// Missing marker = a crashed/partial build of THIS name (CREATE
			// DATABASE ran but writeTemplateMarker did not). It is the only
			// surviving rebuild path: drop the incomplete DB and rebuild so no
			// clone is ever copied from a template with an incomplete schema.
			if err := dropDatabaseConn(ctx, admin, templateName); err != nil {
				return fmt.Errorf("drop partial template: %w", err)
			}
			return buildTemplate(ctx, admin, baseURL, templateName, migPath, wantHash)
		default:
			// ok && gotHash != wantHash: a complete build recorded a hash other
			// than the one the name is derived from. Under hash-naming a distinct
			// hash yields a distinct name, so this is a logically-impossible /
			// corrupted state, never a routine cross-branch event. Fail loud
			// rather than silently drop+rebuild (which would mask real
			// corruption); a developer drops it manually.
			return fmt.Errorf("template %s has marker hash %s but its name implies %s: corrupted/incompatible template; drop it manually", templateName, gotHash, wantHash)
		}
	})
}

// buildTemplate creates the named template DB, runs migrations into it, and
// writes the marker. Must be called while holding the advisory lock. Disconnects
// from the template fully before returning so a subsequent CREATE ... TEMPLATE
// against it (under the same lock by a cloner) cannot fail on a live session.
func buildTemplate(ctx context.Context, admin *pgx.Conn, baseURL, templateName, migPath, wantHash string) error {
	if err := createDatabaseConn(ctx, admin, templateName); err != nil {
		return fmt.Errorf("create template: %w", err)
	}

	templateURL, err := withDatabase(baseURL, templateName)
	if err != nil {
		return err
	}

	// Extensions are also created by migration 001 (CREATE EXTENSION IF NOT
	// EXISTS), but we create them explicitly first per the documented
	// allow-list so the template's extension set is unambiguous.
	if err := createExtensions(ctx, templateURL); err != nil {
		return fmt.Errorf("create extensions in template: %w", err)
	}

	if err := db.RunMigrations(ctx, templateURL, migPath); err != nil {
		return fmt.Errorf("run migrations in template: %w", err)
	}

	if err := writeTemplateMarker(ctx, templateURL, wantHash); err != nil {
		return fmt.Errorf("write template marker: %w", err)
	}
	return nil
}

// createExtensions opens a short-lived connection to the template and creates
// the required extensions, then closes it.
func createExtensions(ctx context.Context, templateURL string) error {
	conn, err := pgx.Connect(ctx, templateURL)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS "uuid-ossp"`); err != nil {
		return err
	}
	if _, err := conn.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS vector`); err != nil {
		return err
	}
	return nil
}

// writeTemplateMarker creates and populates the one-row marker table inside the
// template, on a short-lived connection that is closed before returning.
func writeTemplateMarker(ctx context.Context, templateURL, hash string) error {
	conn, err := pgx.Connect(ctx, templateURL)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, `CREATE TABLE `+markerTable+` (hash text not null)`); err != nil {
		return err
	}
	if _, err := conn.Exec(ctx, `INSERT INTO `+markerTable+` (hash) VALUES ($1)`, hash); err != nil {
		return err
	}
	return nil
}

// readTemplateMarker reads the marker hash from the named template on a
// short-lived connection. ok is false if the marker table does not exist (a
// partial/crashed build that created the DB but not the marker).
func readTemplateMarker(ctx context.Context, baseURL, templateName string) (hash string, ok bool, err error) {
	templateURL, err := withDatabase(baseURL, templateName)
	if err != nil {
		return "", false, err
	}
	conn, err := pgx.Connect(ctx, templateURL)
	if err != nil {
		return "", false, err
	}
	defer func() { _ = conn.Close(ctx) }()

	err = conn.QueryRow(ctx, `SELECT hash FROM `+markerTable+` LIMIT 1`).Scan(&hash)
	if err != nil {
		// Expected "no usable marker" cases → rebuild: the marker table does
		// not exist yet (template built by a prior incompatible harness, or a
		// partial build), or it exists but is empty.
		if errors.Is(err, pgx.ErrNoRows) {
			return "", false, nil
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgerrcode.UndefinedTable {
			return "", false, nil
		}
		// Anything else (connection failure, permission error, etc.) is a real
		// error that must be surfaced rather than silently rebuilding.
		return "", false, fmt.Errorf("read template marker: %w", err)
	}
	return hash, true, nil
}

// ---------------------------------------------------------------------------
// Clone creation
// ---------------------------------------------------------------------------

// createCloneFromTemplate ensures the template is current, then creates a fresh
// clone under the advisory lock after re-verifying the template marker.
// Returns the clone DB name and the full connection URL pointed at it.
func createCloneFromTemplate(ctx context.Context, baseURL, migPath string) (cloneName, cloneConnURL string, err error) {
	wantHash, err := templateHashFromInputs(migPath)
	if err != nil {
		return "", "", fmt.Errorf("compute template hash: %w", err)
	}
	templateName := templateNameFromHash(wantHash)

	token, err := randomToken()
	if err != nil {
		return "", "", err
	}
	cloneName = clonePrefix + token

	admin, err := connectAdmin(ctx, baseURL)
	if err != nil {
		return "", "", fmt.Errorf("connect admin: %w", err)
	}
	defer func() { _ = admin.Close(ctx) }()

	lockErr := withAdvisoryLock(ctx, admin, func() error {
		// Re-read the marker under the lock right before CREATE ... TEMPLATE.
		// With hash-naming this is a pure assertion that should never fail after
		// ensureTemplate just succeeded (a distinct hash would be a distinct
		// name, so no cross-branch race can trip it); surface it loudly if it
		// somehow does.
		gotHash, ok, err := readTemplateMarker(ctx, baseURL, templateName)
		if err != nil {
			return fmt.Errorf("re-read template marker: %w", err)
		}
		if !ok || gotHash != wantHash {
			return fmt.Errorf("template marker mismatch before clone (want %s, got %s, present=%t)", wantHash, gotHash, ok)
		}
		return createDatabaseFromTemplateConn(ctx, admin, cloneName, templateName)
	})
	if lockErr != nil {
		return "", "", lockErr
	}

	cloneConnURL, err = withDatabase(baseURL, cloneName)
	if err != nil {
		return "", "", err
	}
	return cloneName, cloneConnURL, nil
}

// ---------------------------------------------------------------------------
// Name guards (the safety core)
// ---------------------------------------------------------------------------

// assertCreatableTestDBName accepts only hash-named template_<32-hex> and
// clone_<hex> names. The bare base personal_crm_test is rejected (we never
// CREATE it).
func assertCreatableTestDBName(name string) error {
	if !dbNamePattern.MatchString(name) {
		return fmt.Errorf("refusing to CREATE database %q: not a creatable test-family name", name)
	}
	return nil
}

// assertDroppableTestDBName accepts only hash-named template_<32-hex> and
// clone_<hex> names. The base personal_crm_test, the legacy bare
// personal_crm_test_template, dev personal_crm, postgres, template0/1 are all
// rejected by construction.
func assertDroppableTestDBName(name string) error {
	if !dbNamePattern.MatchString(name) {
		return fmt.Errorf("refusing to DROP database %q: not a droppable test-family name", name)
	}
	return nil
}

// assertSafeBaseURL is the SetupPackage guard chokepoint: it parses the env base
// URL and refuses any database other than personal_crm_test, so a mis-set
// DATABASE_URL/TEST_DATABASE_URL pointing at the dev DB fails loudly before any
// CREATE/DROP runs. This is the exact path SetupPackage invokes; unit-tested in
// testdb_internal_test.go (TestBaseURLGuardRejectsProdDB).
func assertSafeBaseURL(baseURL string) error {
	name, err := dbNameFromURL(baseURL)
	if err != nil {
		return fmt.Errorf("parse base URL: %w", err)
	}
	return assertBaseDBName(name)
}

// assertBaseDBName requires the env base DB to be exactly personal_crm_test.
// Used once at SetupPackage entry to make DDL against the dev DB structurally
// impossible.
func assertBaseDBName(name string) error {
	if name != baseDBName {
		return fmt.Errorf("refusing to run testdb harness: base database is %q, expected %q", name, baseDBName)
	}
	return nil
}

// ---------------------------------------------------------------------------
// DDL helpers (raw-SQL allow-list) — all name-guarded.
// ---------------------------------------------------------------------------

// createDatabaseConn runs CREATE DATABASE after the creatable-name guard.
func createDatabaseConn(ctx context.Context, conn *pgx.Conn, name string) error {
	if err := assertCreatableTestDBName(name); err != nil {
		return err
	}
	stmt := "CREATE DATABASE " + pgx.Identifier{name}.Sanitize()
	if _, err := conn.Exec(ctx, stmt); err != nil {
		return fmt.Errorf("create database %q: %w", name, err)
	}
	return nil
}

// createDatabaseFromTemplateConn runs CREATE DATABASE ... TEMPLATE after the
// creatable-name guard on the new name. The template name is also asserted
// creatable (it is a test-family name) for defense in depth.
func createDatabaseFromTemplateConn(ctx context.Context, conn *pgx.Conn, name, template string) error {
	if err := assertCreatableTestDBName(name); err != nil {
		return err
	}
	if err := assertCreatableTestDBName(template); err != nil {
		return err
	}
	stmt := "CREATE DATABASE " + pgx.Identifier{name}.Sanitize() +
		" TEMPLATE " + pgx.Identifier{template}.Sanitize()
	if _, err := conn.Exec(ctx, stmt); err != nil {
		return fmt.Errorf("create database %q from template %q: %w", name, template, err)
	}
	return nil
}

// dropDatabase opens an admin connection and drops the named database. Used by
// the package-clone teardown and NewEphemeralClone cleanup.
func dropDatabase(ctx context.Context, baseURL, name string) error {
	admin, err := connectAdmin(ctx, baseURL)
	if err != nil {
		return fmt.Errorf("connect admin: %w", err)
	}
	defer func() { _ = admin.Close(ctx) }()
	return dropDatabaseConn(ctx, admin, name)
}

// dropDatabaseConn runs DROP DATABASE IF EXISTS ... WITH (FORCE) after the
// droppable-name guard.
func dropDatabaseConn(ctx context.Context, conn *pgx.Conn, name string) error {
	if err := assertDroppableTestDBName(name); err != nil {
		return err
	}
	stmt := "DROP DATABASE IF EXISTS " + pgx.Identifier{name}.Sanitize() + " WITH (FORCE)"
	if _, err := conn.Exec(ctx, stmt); err != nil {
		return fmt.Errorf("drop database %q: %w", name, err)
	}
	return nil
}

// databaseExists checks pg_database for the named database.
func databaseExists(ctx context.Context, conn *pgx.Conn, name string) (bool, error) {
	var one int
	err := conn.QueryRow(ctx, `SELECT 1 FROM pg_database WHERE datname = $1`, name).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check database exists %q: %w", name, err)
	}
	return true, nil
}

// databaseOID returns the pg_database OID of the named database, and ok=false if
// it does not exist. Used ONLY by the harness's own integration tests to detect
// a drop+recreate (a recreated DB gets a new OID) WITHOUT writing a sentinel —
// it is the same maintenance-connection, parameterized, read-only catalog
// pattern as databaseExists, strictly weaker than the numbackends read.
func databaseOID(ctx context.Context, conn *pgx.Conn, name string) (oid uint32, ok bool, err error) {
	err = conn.QueryRow(ctx, `SELECT oid FROM pg_database WHERE datname = $1`, name).Scan(&oid)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read database oid %q: %w", name, err)
	}
	return oid, true, nil
}

// ---------------------------------------------------------------------------
// Advisory lock (mirrors internal/db/migration.go:runRiverMigrations)
// ---------------------------------------------------------------------------

// withAdvisoryLock acquires the session-scoped advisory lock on conn, runs fn,
// then releases it. Acquire and release use the same connection because the
// lock is session-scoped.
func withAdvisoryLock(ctx context.Context, conn *pgx.Conn, fn func() error) (retErr error) {
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", templateBuildAdvisoryLockID); err != nil {
		return fmt.Errorf("acquire advisory lock: %w", err)
	}
	defer func() {
		if _, err := conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", templateBuildAdvisoryLockID); err != nil {
			if retErr == nil {
				retErr = fmt.Errorf("release advisory lock: %w", err)
			} else {
				fmt.Fprintf(os.Stderr, "testdb: release advisory lock (non-fatal, masked by prior error): %v\n", err)
			}
		}
	}()
	return fn()
}

// ---------------------------------------------------------------------------
// URL derivation
// ---------------------------------------------------------------------------

// withDatabase parses baseURL, replaces only the database path segment with
// dbName, preserves credentials/host/port/query, and returns the new URL.
func withDatabase(baseURL, dbName string) (string, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("parse database URL: %w", err)
	}
	u.Path = "/" + dbName
	return u.String(), nil
}

// dbNameFromURL extracts the database name (path segment) from a connection URL.
func dbNameFromURL(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("parse database URL: %w", err)
	}
	return strings.TrimPrefix(u.Path, "/"), nil
}

// connectAdmin opens a single admin connection to the maintenance DB derived
// from baseURL, so CREATE/DROP DATABASE run outside any user schema.
func connectAdmin(ctx context.Context, baseURL string) (*pgx.Conn, error) {
	adminConnURL, err := withDatabase(baseURL, maintenanceDBName)
	if err != nil {
		return nil, err
	}
	return pgx.Connect(ctx, adminConnURL)
}

// ---------------------------------------------------------------------------
// Template hash
// ---------------------------------------------------------------------------

// riverMigrationFingerprint is the version + SQL content of a single River
// migration, used as a hash input so a River driver change invalidates the
// template. Defined to keep templateHash unit-testable without a live driver.
type riverMigrationFingerprint struct {
	version int
	sqlUp   string
	sqlDown string
}

// templateHashFromInputs reads the migration files and the River migration set,
// then computes the deterministic template hash.
func templateHashFromInputs(migPath string) (string, error) {
	files, err := readMigrationFiles(migPath)
	if err != nil {
		return "", err
	}
	river, err := riverMigrationFingerprints()
	if err != nil {
		return "", err
	}
	return templateHash(files, river), nil
}

// readMigrationFiles returns the sorted (by name) contents of every *.up.sql and
// *.down.sql file in the migrations dir.
func readMigrationFiles(migPath string) ([][]byte, error) {
	entries, err := os.ReadDir(migPath)
	if err != nil {
		return nil, fmt.Errorf("read migrations dir %q: %w", migPath, err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".up.sql") || strings.HasSuffix(name, ".down.sql") {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	files := make([][]byte, 0, len(names))
	for _, name := range names {
		// Prefix each file's content with its name so a rename also changes
		// the hash.
		content, err := os.ReadFile(filepath.Join(migPath, name))
		if err != nil {
			return nil, fmt.Errorf("read migration %q: %w", name, err)
		}
		files = append(files, append([]byte(name+"\x00"), content...))
	}
	return files, nil
}

// riverMigrationFingerprints derives the River migration set from the linked
// driver WITHOUT touching any database (riverpgxv5.New(nil) only stores the
// pool; AllVersions reads the embedded migration FS).
func riverMigrationFingerprints() ([]riverMigrationFingerprint, error) {
	migrator, err := rivermigrate.New(riverpgxv5.New(nil), nil)
	if err != nil {
		return nil, fmt.Errorf("construct river migrator: %w", err)
	}
	versions := migrator.AllVersions()
	out := make([]riverMigrationFingerprint, 0, len(versions))
	for _, v := range versions {
		out = append(out, riverMigrationFingerprint{
			version: v.Version,
			sqlUp:   v.SQLUp,
			sqlDown: v.SQLDown,
		})
	}
	return out, nil
}

// templateHash computes a SHA-256 over the migration file contents and the
// River migration set (version + SQL). Pure function over its inputs so it is
// unit-testable with fixtures.
func templateHash(migrationFiles [][]byte, riverMigrations []riverMigrationFingerprint) string {
	h := sha256.New()
	// hash.Hash.Write never returns an error (documented contract), so the
	// Fprintf return values are intentionally discarded.
	for _, f := range migrationFiles {
		// Length-prefix each input so concatenation is unambiguous.
		_, _ = fmt.Fprintf(h, "f%d:", len(f))
		_, _ = h.Write(f)
	}
	// River migrations come from AllVersions sorted by version; hash version +
	// both SQL bodies so a rewrite of an existing migration also invalidates.
	for _, rm := range riverMigrations {
		_, _ = fmt.Fprintf(h, "r%d:%d:%d:", rm.version, len(rm.sqlUp), len(rm.sqlDown))
		_, _ = h.Write([]byte(rm.sqlUp))
		_, _ = h.Write([]byte(rm.sqlDown))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ---------------------------------------------------------------------------
// Misc
// ---------------------------------------------------------------------------

// randomToken returns 16 lowercase-hex chars from 8 crypto/rand bytes,
// collision-proof across parallel processes.
func randomToken() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate clone token: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// envBaseURL returns DATABASE_URL if set, else TEST_DATABASE_URL, else "".
func envBaseURL() string {
	if u := os.Getenv("DATABASE_URL"); u != "" {
		return u
	}
	return os.Getenv("TEST_DATABASE_URL")
}
