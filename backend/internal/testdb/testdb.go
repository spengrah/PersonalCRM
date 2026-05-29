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
//	DROP DATABASE IF EXISTS <name> WITH (FORCE)  DDL in maintenance DB; identifier via Sanitize() after assertDroppableTestDBName
//	SELECT pg_advisory_lock($1) / pg_advisory_unlock($1)  session function; parameterized; constant lock id
//	CREATE EXTENSION IF NOT EXISTS "uuid-ossp" / ... vector  DDL inside the template; static literal
//	CREATE TABLE _testdb_template_marker(...) / INSERT ... / SELECT hash ...  marker table exists only inside test DBs; static DDL; value parameterized via $1
//	SELECT 1 FROM pg_database WHERE datname = $1  system catalog read; parameterized
//	SELECT datname FROM pg_database WHERE datname LIKE $1 ESCAPE '\'  system catalog read (clone sweep); parameterized; escaped `_`
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

	// templateDBName is the template the per-package and per-test clones are
	// copied from. Never swept; reused across runs.
	templateDBName = "personal_crm_test_template"

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
// the template, and clones whose token is lowercase hex. The bare base
// personal_crm_test is intentionally NOT matched (we never CREATE or DROP it).
var dbNamePattern = regexp.MustCompile(`^personal_crm_test_(template|clone_[0-9a-f]+)$`)

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

	// Refuse to run any DDL unless the env base DB is exactly
	// personal_crm_test. A mis-set TEST_DATABASE_URL pointing at the dev DB
	// (personal_crm) must fail loudly before any CREATE/DROP can execute.
	baseName, err := dbNameFromURL(baseURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "testdb: parse base URL: %v\n", err)
		return 1
	}
	if err := assertBaseDBName(baseName); err != nil {
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

// CleanClones drops every leaked personal_crm_test_clone_* database. It is the
// explicit-sweep entrypoint invoked ONLY by `make test-clean-clones` via the
// standalone go-run cmd; it never runs during `go test`. The template and base
// are never touched. Every drop is routed through assertDroppableTestDBName.
// Returns a non-nil error if any guarded drop fails, so the make target exits
// non-zero when leaked clones remain.
func CleanClones() error {
	baseURL := envBaseURL()
	if baseURL == "" {
		return errors.New("testdb.CleanClones: DATABASE_URL/TEST_DATABASE_URL not set")
	}
	baseName, err := dbNameFromURL(baseURL)
	if err != nil {
		return fmt.Errorf("testdb.CleanClones: parse base URL: %w", err)
	}
	if err := assertBaseDBName(baseName); err != nil {
		return fmt.Errorf("testdb.CleanClones: %w", err)
	}

	ctx := context.Background()
	admin, err := connectAdmin(ctx, baseURL)
	if err != nil {
		return fmt.Errorf("testdb.CleanClones: connect admin: %w", err)
	}
	defer func() { _ = admin.Close(ctx) }()

	// Pattern: clones are clonePrefix + hex. The literal `_` after `clone` is
	// escaped so it is not a LIKE single-char wildcard.
	pattern := `personal_crm_test_clone\_%`
	rows, err := admin.Query(ctx, `SELECT datname FROM pg_database WHERE datname LIKE $1 ESCAPE '\'`, pattern)
	if err != nil {
		return fmt.Errorf("testdb.CleanClones: list clones: %w", err)
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return fmt.Errorf("testdb.CleanClones: scan: %w", err)
		}
		names = append(names, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("testdb.CleanClones: rows: %w", err)
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
	fmt.Fprintf(os.Stderr, "testdb.CleanClones: dropped %d leaked clone(s)\n", dropped)
	if len(dropErrs) > 0 {
		return fmt.Errorf("testdb.CleanClones: %d clone(s) could not be dropped: %w", len(dropErrs), errors.Join(dropErrs...))
	}
	return nil
}

// ---------------------------------------------------------------------------
// Template build
// ---------------------------------------------------------------------------

// ensureTemplate builds or refreshes the template DB under the advisory lock.
// On exit the template exists and its marker equals the current wantHash.
func ensureTemplate(ctx context.Context, baseURL, migPath string) error {
	wantHash, err := templateHashFromInputs(migPath)
	if err != nil {
		return fmt.Errorf("compute template hash: %w", err)
	}

	admin, err := connectAdmin(ctx, baseURL)
	if err != nil {
		return fmt.Errorf("connect admin: %w", err)
	}
	defer func() { _ = admin.Close(ctx) }()

	return withAdvisoryLock(ctx, admin, func() error {
		exists, err := databaseExists(ctx, admin, templateDBName)
		if err != nil {
			return err
		}
		if exists {
			gotHash, ok, err := readTemplateMarker(ctx, baseURL)
			if err != nil {
				return err
			}
			if ok && gotHash == wantHash {
				return nil // reuse as-is
			}
			// Stale or marker-less template: rebuild.
			if err := dropDatabaseConn(ctx, admin, templateDBName); err != nil {
				return fmt.Errorf("drop stale template: %w", err)
			}
		}
		return buildTemplate(ctx, admin, baseURL, migPath, wantHash)
	})
}

// buildTemplate creates the template DB, runs migrations into it, and writes
// the marker. Must be called while holding the advisory lock. Disconnects from
// the template fully before returning so a subsequent CREATE ... TEMPLATE
// against it (under the same lock by a cloner) cannot fail on a live session.
func buildTemplate(ctx context.Context, admin *pgx.Conn, baseURL, migPath, wantHash string) error {
	if err := createDatabaseConn(ctx, admin, templateDBName); err != nil {
		return fmt.Errorf("create template: %w", err)
	}

	templateURL, err := withDatabase(baseURL, templateDBName)
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

// readTemplateMarker reads the marker hash from the template on a short-lived
// connection. ok is false if the marker table does not exist (a template built
// by a prior incompatible harness, or a partial build).
func readTemplateMarker(ctx context.Context, baseURL string) (hash string, ok bool, err error) {
	templateURL, err := withDatabase(baseURL, templateDBName)
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
		// Re-verify the template still exists and matches wantHash before
		// cloning, guarding against a concurrent rebuild between ensureTemplate
		// and here.
		gotHash, ok, err := readTemplateMarker(ctx, baseURL)
		if err != nil {
			return fmt.Errorf("re-read template marker: %w", err)
		}
		if !ok || gotHash != wantHash {
			return fmt.Errorf("template marker mismatch before clone (want %s, got %s, present=%t)", wantHash, gotHash, ok)
		}
		return createDatabaseFromTemplateConn(ctx, admin, cloneName, templateDBName)
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

// assertCreatableTestDBName accepts only the template and clone_<hex> names.
// The bare base personal_crm_test is rejected (we never CREATE it).
func assertCreatableTestDBName(name string) error {
	if !dbNamePattern.MatchString(name) {
		return fmt.Errorf("refusing to CREATE database %q: not a creatable test-family name", name)
	}
	return nil
}

// assertDroppableTestDBName accepts only the template and clone_<hex> names.
// The base personal_crm_test, dev personal_crm, postgres, template0/1 are all
// rejected by construction.
func assertDroppableTestDBName(name string) error {
	if !dbNamePattern.MatchString(name) {
		return fmt.Errorf("refusing to DROP database %q: not a droppable test-family name", name)
	}
	return nil
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
