# Core Rules

These rules apply to all AI agents working on this project.

## Absolute Rules (Never Violate)

1. **Never use `time.Now()`** → Use `accelerated.GetCurrentTime()`
2. **Never write raw SQL in Go** → Use sqlc-generated queries (`make sqlc`). Applies to ALL Go code: production code, integration tests, test fixtures, and helper scripts — add a test-only sqlc query + repository wrapper rather than inlining `pool.Exec(ctx, "INSERT ...", ...)` in a test file.
3. **Never skip layers** → Handler → Service → Repository → DB
4. **Never use npm/npx** → Use bun/bunx
5. **Never call queries from handlers** → Go through repository
6. **Always sign commits** → `git commit -S -m "..."`
7. **Always add comprehensive tests** → Unit for logic, integration for DB, E2E for flows

## Code Quality

- Keep solutions simple and direct
- Prefer boring, readable code over clever abstractions
- Do not over-engineer or add unrequested features
- Run lint/format after code changes
- Run tests to verify changes work during development

## Testing Requirements

- **Always add comprehensive tests** for new/changed code
- Run `make test && make test-e2e-diff` to verify changes work locally (CI runs full E2E)
- Run `make test-e2e` only for the full suite (CI or when explicitly requested)
- Use make test-e2e-local only when a specific grep is requested.
- Unit tests for business logic, integration tests for DB operations, E2E for user flows

## Pre-push Hooks

Git pre-push hooks run automatically and may block push:

- **Tests**: Runs lint and all test suites. Push blocked if tests fail.
- **Swift (mac-daemon)**: When the push range touches `mac-daemon/**`, runs `CI=true make test-daemon-local` (`swift test`, CI marker set so real-Keychain/notification tests skip). Blocks push on failure. Skips with a warning (exit 0) if Xcode/XCTest is unavailable — that skip is the intended escape hatch, not a push bypass.

## Code Review Approval Criteria

PRs must meet ALL of these to pass review:
- No security concerns (SQL injection, XSS, auth issues, secrets)
- No bugs or unhandled edge cases
- Comprehensive test coverage for new/changed code
- Follows repository conventions (this file)
- Proper error handling and validation
- No TODOs or technical debt introduced

See `.ai/rules/code-review.md` for details

## Git Practices

- Use conventional commits (feat:, fix:, docs:, refactor:, test:, chore:)
- First line under 72 characters
- Commit logical units of work, not partial changes
- Use conventional branches (feat/, fix/, refactor/, docs/, test/, chore/)

## Layered Architecture

```
Handler → Service → Repository → sqlc → PostgreSQL
```

See [Request Flow Diagram](../guides/architecture.md#why-layered) for the full sequence.

## Common Gotchas

| Mistake | Fix |
|---------|-----|
| `make test` from subdirectory | Run `make` commands from project root |
| `go test ./backend/...` | Use `make test-unit` or `cd backend && go test` |
| `npm install` | Use `bun install` |
| `sqlc generate` | Use `make sqlc` (sqlc is in ~/go/bin) |
| Hand-rolling integration-test fixtures (raw SQL, ad-hoc inserts) | Prefer the synthetic toolkit factories/harness (`synthetic.NewHarnessForNamespace`, `migrationGenerator`/`seedMigrationContact`) — see `.ai/patterns/synthetic-seed-toolkit.md` and `.ai/rules/testing.md`. Raw SQL stays banned in ALL Go code (production + tests + fixtures); if the toolkit can't express a fixture, add a test-only sqlc query (e.g., `InsertFooAtTime`) + repository wrapper rather than inlining `pool.Exec(ctx, "INSERT ...")`. (Raw SQL is fine when it's the *subject* under test, e.g. the migration-runner tests in `integration_test.go`.) |
| Calling `queries.X()` from handler | Call `repo.X()` instead |
| Missing `deleted_at IS NULL` in queries | All queries must filter soft deletes |
| Querying DB directly | `docker exec crm-postgres psql -U crm_user -d personal_crm -c "..."` |
| Assuming repository method names | Read the repository file first |
| Not building after `make sqlc` | Always run `go build ./cmd/crm-api` to verify compilation |
| sqlc changed types after regeneration | Update repository to use `pgtype.X{Value: v, Valid: true}` wrappers |
| Assuming all tables have `updated_at` | It's per-table opt-in (explicit column + `update_updated_at_column()` trigger); many tables lack it (e.g. `tag`, `interaction`, `reminder`, `connection`). Check the table's migration before selecting or setting `updated_at` |
| `git add -A` includes binaries | Review `git status` before commit, exclude `backend/crm-api` |
| Merging PRs with UI changes | Never merge UI PRs autonomously - wait for human review |
| `git add path/[id]/file` fails | Use quotes: `git add "path/[id]/file"` (bash interprets brackets as globs) |
| Fixing only one instance of a pattern | Search entire codebase and fix ALL instances to maintain consistency |
| Creating prototype HTML in repo root | Place prototypes in `temp/` (git-ignored), attach to issues for reference |
| Expecting soft-delete to cascade to related records | Soft-delete (UPDATE deleted_at) does NOT trigger ON DELETE CASCADE - explicitly delete related records first |
| Building multi-step wizard modals | Use single-view scrollable modals (like ImportLinkModal) - all steps visible in one view |
| Using `\n` in `gh` CLI body/comment strings (renders as literal `\n`) | Use a heredoc or multi-line string for `gh pr create/edit/comment` |
| `git diff --quiet` to detect new files | Only sees tracked files; add `git ls-files --others --exclude-standard` for untracked |
| Feature removal only grepping source files | Grep ALL file types (tests, docs, comments) for feature name to catch all references |
| Editing `.claude/CLAUDE.md` directly | It's a symlink to `AGENTS.md` - stage `AGENTS.md` for git commits |
| Outdated Go version in docs | Search `go1\.2[0-9]\.` and update all references to match go.mod |
| E2E test waiting for page load | Use `domcontentloaded` + `getByRole` with explicit timeout, not `networkidle` |
| OAuth callback routes inside auth middleware | Register callback routes BEFORE `v1.Use(auth.APIKeyMiddleware)` - external providers can't include API keys |
| String concat for OAuth redirect params | Use `url.Values{}.Encode()` for ALL redirect query params - prevents injection vulnerabilities |
| Adding settings hooks without test-map entry | Add new hooks (e.g., use-todoist-accounts.ts) to `frontend/tests/e2e/test-map.json` with `@area:settings` |
| Redefining helper functions in new repository files | Use existing helpers from `repository/conversions.go` (uuidToPgUUID, stringToPgText, timeToPgTimestamptz) |
| List display and navigation use different sort defaults | Extract DEFAULT_SORT_FIELD/ORDER constants; use in useState, buildContactUrl, and detail page listContext |
| SQL queries without ORDER BY for "unsorted" case | Always provide deterministic ordering; arbitrary DB order breaks navigation consistency |
| E2E `getByText` in strict mode with non-unique values | Use `.first()` or scope to specific row when DB may contain multiple matches from previous runs. (Not yet retired: E5 migrated the backend Go suite, not Playwright; a future per-worker-namespaced scenario catalog will reduce this.) |
| `make test-e2e-local GREP="..."` ignored | Use `PLAYWRIGHT_GREP="..." make test-e2e-local` (Makefile expects environment variable, not Make variable) |
| `bunx playwright test` directly fails with 401 | Use `make test-e2e-diff` or `make test-e2e-local` which set up API keys and environment |
| `make test-e2e-diff` missing clean DB in pre-push | `test-e2e-diff` target needs `e2e-db` prerequisite like `test-e2e` and `test-e2e-local` have. Without it, pre-push hook runs E2E tests against accumulated DB state instead of clean DB, causing flaky failures from test pollution |
| E2E parallel workers see each other's candidates in import/link modal | When E2E tests seed external contacts and open the import/link modal, parallel workers' seeded data can cause the modal to open showing a different worker's candidate (e.g., '2 of 4' pagination). Tests asserting on modal content without first navigating to the correct candidate will fail intermittently. Call `navigateModalToCandidate(page, handle)` after opening modal and before assertions to ensure test's own candidate is visible. (Not yet retired: E5 migrated the backend Go suite, not Playwright; a future per-worker-namespaced scenario catalog will reduce this.) |
| Replacing JSONB metadata wholesale on update | Read existing metadata first, modify keys, then write back - prevents losing other keys |
| New API module with custom URL construction | Use shared `apiClient` from `lib/api-client.ts` - include `/api/v1` in endpoint paths |
| Form inputs without explicit text color | Add `text-gray-900 placeholder-gray-400` - browser defaults appear washed out |
| `leading-7` + `truncate` on a heading clips descenders (y, g, j, p, q) | Use `leading-normal` for adequate line height on truncated text |
| Service verifying entity exists before query | Skip if FK constraints guarantee validity - adds unnecessary latency |
| React Query hooks in child components | Move to parent level to enable parallel loading - child mounting creates waterfall |
| Todoist QuickAdd `note` parameter for descriptions | `note` creates comments, not descriptions - use two-step: QuickAdd then Sync API `item_update` |
| Todoist v9 numeric IDs with v1 API | v1 returns alphanumeric IDs (e.g., `6fw9cQQ5JppCp7qX`) - migration complete as of Feb 2026; `tryRecoverPendingTempID` handles only failed temp ID resolution |
| Parsing Todoist CRM markers as full description | CRM markers are embedded after markdown prefix or as standalone JSON - `tryRecoverPendingTempID` uses `strings.LastIndex` to extract |
| Adding new Todoist task metadata key to only one path | Must update ALL 6 task creation/update paths: handleSkipTrigger, reconcileContactTasks (new task), reconcileExistingTask (3 backfill paths + drift), plus closeOnOutreach (outreach detection + pre-gate backfill) |
| Using UUID for external source references | Use TEXT - only GCal uses UUIDs; Todoist uses alphanumeric strings (e.g., `6fw9cQQ5JppCp7qX`) |
| Forward-only update semantics for all sources | Manual source must always update (user correction); forward-only is only for automated sources (gcal, todoist) |
| Creating FK-referencing record without existence check | Check parent exists first and return ErrNotFound - otherwise FK constraint violation returns 500 instead of 404 |
| Adding new struct field used in existing methods | Update all test factory functions (`newTestProvider`, `newTestProviderWithExternal`, etc.) to initialize the field - otherwise nil pointer panics in tests |
| PostgreSQL `DESC NULLS FIRST` for date columns | Use `NULLS LAST` for both ASC and DESC on date sorts - keeps NULLs at end regardless of direction |
| E2E sort test comparing row positions with DATE columns | Accelerated cadences make contact_by dates collapse to same day - verify sort via API request params (`page.waitForResponse` with `sort=field&order=direction`), not visual row positions |
| One-time action query params (e.g., `?action=edit`) left in URL | Clear with `router.replace()` in a mount-only `useEffect` after consuming - prevents re-triggering on page refresh |
| `PLAYWRIGHT_GREP` patterns with spaces | Makefile passes `--grep $$PLAYWRIGHT_GREP` unquoted - use dot wildcards (e.g., `context.menu`) or single words instead of multi-word patterns |
| Portal dropdown without ARIA attributes | Add `aria-label`, `aria-haspopup="menu"`, `aria-expanded` on trigger; `role="menu"` on dropdown container |
| Adding `role="menuitem"` without updating tests | `getByRole('button')` no longer matches elements with explicit `role="menuitem"` - update E2E selectors to `getByRole('menuitem')` |
| Adding `aria-label` to buttons with text content | `aria-label` overrides text content as accessible name - `getByRole('button', { name: 'Previous' })` breaks if `aria-label="Go to previous page"` is added |
| Adding params to existing sqlc queries | Convert ALL positional params ($1, $2) to named `sqlc.arg()` first - can't mix styles. Expect field name changes in generated structs (e.g., `Limit`→`PageLimit`). Run `go build ./cmd/crm-api` after `make sqlc` |
| E2E `waitForResponse` for param removal | Response may fire before listener is set up when removing a URL param (e.g., resetting filter). Use visibility assertions with timeout instead |
| Adding new contact list filter | Must update all 10 SQL queries (8 listing + 2 count), plus repository params, service, handler, frontend types, API client, contacts page, and detail page listContext/buildNavigationUrl |
| Writing new integration test in `backend/tests/` | Must call `db.RunMigrations(databaseURL, getMigrationsPath())` before `db.NewDatabase()` - CI has bare PostgreSQL without pre-existing schema |
| Filtering nullable string columns (e.g., cadence) with only IS NULL/IS NOT NULL | Must handle three states: NULL, empty string `''`, and valid values. Use `IS NOT NULL AND col != ''` for "has value" and `(IS NULL OR col = '')` for "no value" |
| Integration test asserting equality across separate count queries | Shared DB means other tests can create/delete rows between calls — assert per-query invariants (e.g., `>= 1`) instead of cross-query equality. Reduced by toolkit seeding: scope reads to your namespace (`syntheticNS(t)`) so counts are over your own rows, not the whole DB — see `.ai/patterns/synthetic-seed-toolkit.md`. The invariant guidance still applies to any unscoped/global count |
| Todoist REST API `close` vs `delete` for cleanup | `POST /tasks/{id}/close` marks as completed (triggers `handleTaskCompletion` on next sync); `DELETE /tasks/{id}` permanently removes. Use DELETE for cleanup scripts |
| Todoist Sync API incremental responses include completed/deleted items | Sync API returns all changed items including `Checked: true` and `IsDeleted: true` — matching logic must explicitly guard against these to avoid processing stale items as active |
| Test cleanup uses soft-delete on upsert-ed table | Upsert queries usually don't clear `deleted_at`, so soft-deleted rows from prior runs resurrect as phantom records. Use hard DELETE (or `DeleteExternalContactsBySourceIDPrefix`-style targeted queries) in integration-test cleanup |
| Integration sub-test reuses identifying names across `t.Run` blocks | Shared DB + fuzzy trigram matching means two sub-tests with the same full_name pollute each other (collision-gap rule fires on tied scores). Use the toolkit's namespace isolation (`synthetic.NewHarnessForNamespace` / `migrationGenerator` + `syntheticNS(t)`) so each sub-test's candidate pool is unique — see `.ai/patterns/synthetic-seed-toolkit.md`. For tests not yet on the toolkit, the older fallback is a randomized per-subtest suffix (`uniqueSuffix` / `uuid[:8]`) |
| Selecting top-N from a Go map | Map iteration order is randomized — equal-score items can swap each run. Collect to slice and `sort.Slice` with an explicit tie-breaker (e.g., score desc then ID asc) |
| Acting on LSP "compile error" diagnostics as ground truth | LSP state lags behind code after signature changes. Always verify with `go build ./...` from `backend/` before treating a diagnostic as real — passing build means the errors are stale |
| Multi-entity events using shared `source_ref` on child rows | One event affecting N entities (e.g., calendar event with N attendees → N interactions) must use per-entity `source_ref` like `gcal:event-123:contact-uuid-1`, else rows 2-N silently dedup against row 1 via the `(source, source_ref)` unique. The `(source, source_id)` dedup on the `event` table is separate and does NOT prevent this |
| Dedup time-window queries missing semantic dimensions | `FindInWindow` dedup queries must include ALL semantically-significant dimensions, not just entity + time. Example: interaction dedup was using `(contact_id, occurred_at ±30min, source)` but missing `direction` — caused false-positive dedup when outbound followed by inbound within 30 min. When adding semantic columns to tables with existing dedup logic, audit all `FindInWindow` queries and unique constraints |
| Mutating state before publishing a tx-bound event | Publish-before-mutate order within a `pgx.Tx`: `bus.PublishTx` → mutate state → commit. Reverse order strands interactions on publish failure — retries see the mutation already committed and skip the publish |
| Holding `pgx.Tx` open across external HTTP calls in consumers | Commit DB writes first, then call external APIs (Todoist, etc.) in a post-commit closure. Blocking the tx on network I/O stalls the connection pool and risks deadlocks |
| `river.Client.Start(ctx)` with a timeout-derived context | River silently stops fetching jobs when its fetch-loop ctx cancels. Pass the outer root context (never `context.WithTimeout(...)`). Applies to test harnesses too — use the test's base ctx, not a per-test timeout ctx |
| Echoing API keys or other secrets in bash commands or tool descriptions | Session transcripts persist across conversations. Read secrets inline from `.env` on the target host without emitting them: `ssh host "API_KEY=\$(grep -oP '^API_KEY=\\K.*' /path/.env) curl ..."`. Never include the literal value in a command visible to the transcript. If a secret already leaked into a transcript, rotate it |
| Integration tests fail with "Contact X should be in list" or limit-based assertions | Test DB (`personal_crm_test`) accumulates state across runs (e.g. 255 contacts). Reduced by toolkit seeding: scope assertions to your namespace (`syntheticNS(t)`) instead of asserting over the whole list/limit window — see `.ai/patterns/synthetic-seed-toolkit.md`. For any unscoped list-bounded test: `make e2e-db` is wired into `test-e2e` but NOT into `test-integration`, so run `make e2e-db` manually before `make test-integration` if it fails. Symptom: tests pass on a fresh DB and on `main`, but fail mid-session after many runs |
| Test failures during PR rebase work | Verify against `main` first (`git checkout main && make test-integration`) — pre-existing failures (e.g. flaky LONG_TESTS-gated scheduler rescuer tests) are not regressions from your rebase. Distinguish before debugging |
| Deleting tests in one commit with "Step N adds replacements" promissory note | Delete tests in the SAME commit as their replacements, or keep old tests until the new ones land and delete in a follow-up commit. A promissory deletion stranded between commits (e.g. long agent work hits auth failure mid-task) leaves the branch in a broken-tests state |
| Two-phase updates (`UpdateContact` → `ApplyContactByOverride`) returning a stale struct | The second write bumps `updated_at` and any columns it touches, but the struct returned from the first write is NOT refreshed. Refetch via `repo.GetContact(ctx, id)` inside the same tx after the second update, before returning. Applies to any two-phase write pattern where columns in the returned struct may change |
| Inline comments referencing `PR #N`, `Step N`, `Decision N`, `Round-N`, plan names, or "this fix" | These metadata references rot the moment the PR merges. Strip them — keep the underlying rationale (why the code behaves this way) but drop the scaffolding (which PR / step / decision it came from). The PR description is the right home for that context. Applies to `.go`, `.sql`, and any source file comments; documentation (`.md`, `README`, `CHANGELOG`) is allowed to cite PRs |
| Treating local Codex-clean + green CI as "PR complete" | `chatgpt-codex-connector[bot]` posts its findings as `COMMENTED` (not `CHANGES_REQUESTED`), so inline P1 comments don't block merge or surface in `gh pr view`. After pushing a PR, always fetch inline review comments with `gh api repos/{owner}/{repo}/pulls/{num}/comments --jq '.[] \| {path, line, user: .user.login, body}'` and address findings before marking work complete |
| `pgx.Tx` insert hitting a unique-violation aborts the outer tx | Any further query in the same tx after a 23505 fails with "current transaction is aborted". If you need to recover from a concurrent-writer collision (e.g. partial unique index like `idx_contact_task_followup_unique_live`), wrap the insert in a savepoint via `tx.Begin(ctx)`: commit the nested tx on success, `sp.Rollback(ctx)` on failure, then inspect `pgconn.PgError.Code == pgerrcode.UniqueViolation` (scope with `ConstraintName`) and re-read existing state on the outer tx |
| `UpdateX` with `RETURNING *` but caller discards the return | Two-phase patterns where the update changes columns the close/enqueue decision depends on (e.g. `state`, `external_task_id`) must read from the RETURNING row, not the pre-update snapshot. If another worker finalizes the row between your initial read and your update, the snapshot is stale and downstream branches misfire. Capture `updated, err := repo.UpdateXTx(...)` and gate on `updated.Field`, not `pre.Field` |
| Plain GIN on JSONB array used with `LOWER()` predicates | PostgreSQL cannot use `GIN(col jsonb_ops)` or `GIN(col jsonb_path_ops)` for `WHERE EXISTS(... LOWER(elem->>'k') = LOWER($1))`. Switching to plain GIN + `@>` rewrite silently drops case-insensitivity. Use a functional GIN over a STRICT IMMUTABLE helper that lowercases the projected values: `CREATE INDEX ... USING GIN(jsonb_array_lower_values(col, 'key'))` and rewrite the WHERE to `&& ARRAY[LOWER($1)]`. Also: scoping JSONB index work as "migration-only" is a trap — existing queries almost always need rewrites too |
| `CREATE OR REPLACE FUNCTION` in a migration that is referenced by an index | Use plain `CREATE FUNCTION` for any function that backs a functional index. A future migration that tries to redefine it will fail loudly (duplicate object) instead of silently overwriting the body and invalidating the index — the loud failure forces the author to think about whether dependent indexes need rebuilding. Reserve `OR REPLACE` for trigger functions that aren't index-backed |
| EXPLAIN ANALYZE with `SET enable_seqscan = off` | Honest perf measurement runs WITHOUT planner overrides. Forcing `enable_seqscan = off` proves only that the index is used when forced — not that it wins on real data. On small tables seq scan is genuinely faster, and an honest baseline catches that. Re-run with the override OFF before claiming an index will activate in prod |
| Setting `CI=1` to skip mac-daemon real-system tests in a script | The Swift suite gates real-Keychain/notification tests on `environment["CI"] == "true"` (string `"true"`, NOT `"1"`). Use `CI=true`; `CI=1` leaves those tests running against the real login Keychain |
| Adding a new path-trigger group | `path-filters.yml` (repo root) is the SINGLE source read by BOTH CI's dorny filter (`filters: ./path-filters.yml`) and the pre-push hook's group-aware parser (`file_in_group`). Keep it LCD: flat named groups of `'dir/**'` globs, no anchors/aliases/negation. The Go/frontend local gate keys off the `backend ∪ frontend` groups. The local Swift gate (`check_swift`) deliberately does NOT use the `mac_daemon` group — it uses a stricter literal `mac-daemon/` prefix test so a `ci.yml`-only push (which IS in the `mac_daemon` group, for CI's sake) does not run `swift test` locally |
| Writing a new backend integration test that runs serial (no `t.Parallel()`) | Default to `t.Parallel()` — the suite is parallel (#430/#438/#428) and a serial test widens the serial prefix. Scope assertions to your namespace (`syntheticNS(t)`) on the shared package DB; if it starts a River worker / asserts DB-wide over `river_job` / uses fixed-ID fixtures, isolate via `newIsolatedRiverTestDB` (per-test clone) before flipping. `mac_host_*` + migration-subject tests stay serial. See `.ai/rules/testing.md` "Backend Integration-Test Parallelism" + `.ai/patterns/test-parallelism.md` |
| Local Postgres connection errors during `make test-integration` | Local `crm-postgres` is `max_connections=200` (matches CI); the first `docker compose up -d` after pulling recreates the container to apply it (named volume persists, dev data safe). Until recreated, `scripts/test-parallelism.sh` cleanly falls back to `-p 4` — no regression, just no speedup |

## Error Handling

```go
// Proper error wrapping
if err != nil {
    return fmt.Errorf("create contact: %w", err)
}

// Proper error comparison
if errors.Is(err, db.ErrNotFound) {
    api.SendNotFound(c, "Contact")
    return
}
```

## Soft Deletes

All queries must filter `WHERE deleted_at IS NULL`. This is enforced in sqlc queries.

**Important:** Soft-delete (`UPDATE deleted_at = NOW()`) does NOT trigger FK cascades. When soft-deleting a parent record (e.g., contact), you must explicitly delete or reassign related records (e.g., contact_methods, notes) first. The ON DELETE CASCADE constraint only fires on actual DELETE statements.

## When You Change X, Check Y

Cross-cutting concerns that require checking multiple locations:

| When You Change | Also Check/Update |
|-----------------|-------------------|
| Contact soft-delete logic | Identity cleanup, note cascade, contact_method cascade |
| Sync provider implementation | Provider registry table in `.ai/patterns/sync.md` |
| New React Query hook | Hooks inventory in `.ai/patterns/frontend.md` |
| New API endpoint | API routes table in `.ai/guides/feature-development.md` |
| Query invalidation rules | `frontend/src/lib/query-invalidation.ts` domain events |
| New database table | Database tables in `.ai/guides/architecture.md` |
| New entity, sync source, or downstream record | Add the matching factory (`synthetic/factory`) + replay/seed coverage (`synthetic/replay` adapter, profile in `synthetic/profiles.go`) to the synthetic toolkit — new features write their own seeding (see `.ai/patterns/synthetic-seed-toolkit.md`) |
| OAuth flow changes | Both callback route AND auth URL endpoint |
| Contact method types | `contact_method` CHECK constraint + `identity.IdentifierType` enum |
| Cadence options | `contact` CHECK constraint + frontend dropdown options |
| E2E test file patterns | `frontend/tests/e2e/test-map.json` tag mappings |
| Scheduled job changes | Scheduler section in `.ai/guides/architecture.md` |
| New sortable contact field | 4 SQL sorted queries + handler oneof + `SortField` type + `ContactListParams.sort` |
| New contact list filter param | 10 SQL queries + repo params + service count calls + handler query struct + frontend types + API client + contacts page + detail page listContext |
| Semantic column to table with dedup logic | All FindInWindow queries (add param), unique constraints, repository signatures, service call sites, `make sqlc`, regression test for false-positive case |
| New type or worker referenced from `cmd/crm-api/main.go` | Inline into `main.go` (single-file build convention). If inlining isn't feasible, update ALL build sites to package form `./cmd/crm-api`: `.github/workflows/ci.yml`, `Makefile` (6+ targets), `frontend/playwright.config.ts` webServer |
| Path-filter groups (`path-filters.yml`) | CI dorny step (`.github/workflows/ci.yml` `changes` job) AND `scripts/hooks/pre-push` group-aware parser (`file_in_group` call sites + `check_swift`) AND the filter unit-test harness assertions |
| New pre-push test phase/command (`.ai/pre-push.json`) | The hook's `run_phases_parallel` lane classifier in `scripts/hooks/pre-push` (DB/port commands run exclusive-last, others concurrent) AND the phase guard test `scripts/hooks/test/test-pre-push-phases.sh` |
| Makefile test-parallelism (`test-integration*` `-p`/`-parallel`) | `scripts/test-parallelism.sh` (single-source formula) + the render guard `scripts/ci/test-parallelism-render-guard.sh` + the CI `GITHUB_ACTIONS` 4/4 pin (keep CI byte-identical) |
