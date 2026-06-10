# Test Parallelism — per-test isolated DB clones

How to make a backend integration test parallel-safe when namespace scoping isn't enough — i.e. it starts a River worker, asserts DB-wide over `river_job`, or uses fixed-ID fixtures. The mechanism is a per-test **ephemeral clone**: its own database (copied from the pre-migrated, content-hashed template) with its own pool and River client, dropped on cleanup. Established by #428. See `.ai/rules/testing.md` → "Backend Integration-Test Parallelism" for when to reach for this vs. namespace scoping on the shared package DB.

## The helper (don't call `NewEphemeralClone` directly)

```go
database, cfg := newIsolatedRiverTestDB(t, ctx)
```

`newIsolatedRiverTestDB` (in `backend/tests/river_isolated_testdb_test.go`) does the `DATABASE_URL=="" → t.Skip`, mints the clone, builds `cfg` with a small pool (`MaxConns=6`, `WorkerConcurrency=2`), and registers `database.Close()` + clone-drop on `t.Cleanup`.

**Why the seam and not `testdb.NewEphemeralClone` directly:** `internal/testdb` compiles only under the `integration_testdb` build tag, but `make test-unit` builds `./tests/...` WITHOUT it. The helper has a no-tag fallback (`river_isolated_testdb_fallback_test.go`) that cleanly `t.Skip`s, so both build paths compile. Importing `testdb` directly from an untagged test file breaks the unit build.

## Conversion recipe

Replace an inline shared-DB setup:

```go
databaseURL := os.Getenv("DATABASE_URL")
if databaseURL == "" { t.Skip("DATABASE_URL not set") }
require.NoError(t, db.RunMigrations(ctx, databaseURL, getMigrationsPath())) // DROP — clone is pre-migrated
cfg := config.TestConfig(); cfg.Database.URL = databaseURL
database, err := db.NewDatabase(ctx, cfg.Database)
```

with:

```go
database, cfg := newIsolatedRiverTestDB(t, ctx)
```

Then add `t.Parallel()` to each top-level func. If several files share one setup helper (e.g. `setupAggregationTest`, `newFollowUpIntegrationDB`), convert the helper once and all its callers isolate at once.

## Gotchas

- **`client.Start(ctx)` must get the test's BASE ctx**, never a `context.WithTimeout(...)` ctx — River derives its fetch loop from that ctx and silently stops fetching when it cancels (the test then hangs in a `waitFor*` poll). The conversion changes only which `*db.Database` the client is built on; it must NOT change the ctx passed to `Start`.
- **Drop the now-redundant `db.RunMigrations` call** and remove whatever imports/helpers (`getMigrationsPath…`) the build then flags unused. Verify both paths: `go build -tags integration_testdb ./...` and a no-tag `go vet ./tests/...`.
- **Don't change a shared helper that enqueue-only callers also use** (e.g. `newEventBusTestDB`). Change only the call sites that need isolation; enqueue-only tests (never `Start`) belong on the shared DB and adding clones to them is wasted mint cost.
- **Raw SQL against `river_job`** stays only where it's the subject under test (e.g. `sync_repo_enqueue_test.go`) — the AGENTS.md "raw SQL is fine when it's the subject under test" carve-out. Don't add new raw SQL; isolation alone fixes the DB-wide-count collision.

## Validate (the gate)

Run the changed package under race + repetition + shuffle at the local `-p`/`-parallel`, with a guaranteed non-empty DSN — an empty DSN makes `testdb.SetupPackage` skip cloning and every DB test self-`Skip`s into a false green:

```bash
DSN="${TEST_DATABASE_URL:?must be set — refusing a no-clone false-green}"
cd backend && DATABASE_URL="$DSN" go test -tags integration_testdb -race -count=10 -shuffle=on -p 9 -parallel 9 ./tests/
```

Confirm the converted tests show `--- PASS`, not `--- SKIP`. Fixed-ID collisions surface only under `-count>=2`, so the `-count=10` run is the proof isolation worked. After the run, `make test-clean-clones` should report ~0 leaked clones, and the `pg_stat_activity` peak should sit well under the 187 usable ceiling (`max_connections=200` minus reserved/margin).
