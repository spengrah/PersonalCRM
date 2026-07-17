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

### Branch model: `develop` (default) + `main` (prod)

- The default branch is `develop`. ALL PRs target `develop` (protected: requires a PR + green CI).
- `main` is prod and is fast-forward-only from `develop` — never commit or open PRs directly against it.
- Promote with `make promote` (`git push origin develop:main`), which triggers the self-hosted-runner prod deploy via `deploy-prod.yml`.
- Pushes to `main` skip the local pre-push checks: the content was already reviewed + CI-gated on `develop`, and `deploy-prod.yml` re-verifies CI for the SHA before deploying.

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
| `sqlc generate` | Use `make sqlc` (sqlc is in ~/go/bin) |
| Hand-rolling integration-test fixtures (raw SQL, ad-hoc inserts) | Prefer the synthetic toolkit factories/harness (`synthetic.NewHarnessForNamespace`, `migrationGenerator`/`seedMigrationContact`) — see `.ai/patterns/synthetic-seed-toolkit.md` and `.ai/rules/testing.md`. Raw SQL stays banned in ALL Go code (production + tests + fixtures); if the toolkit can't express a fixture, add a test-only sqlc query (e.g., `InsertFooAtTime`) + repository wrapper rather than inlining `pool.Exec(ctx, "INSERT ...")`. (Raw SQL is fine when it's the *subject* under test, e.g. the migration-runner tests in `integration_test.go`.) |
| Calling `queries.X()` from handler | Call `repo.X()` instead |
| Querying DB directly | `docker exec crm-postgres psql -U crm_user -d personal_crm -c "..."` |
| Assuming repository method names | Read the repository file first |
| Not building after `make sqlc` | Always run `go build ./cmd/crm-api` to verify compilation |
| sqlc changed types after regeneration | Update repository to use `pgtype.X{Value: v, Valid: true}` wrappers |
| Assuming all tables have `updated_at` | It's per-table opt-in (explicit column + `update_updated_at_column()` trigger); many tables lack it (e.g. `tag`, `interaction`, `reminder`). Check the table's migration before selecting or setting `updated_at` |
| `git add -A` includes binaries | Review `git status` before commit, exclude `backend/crm-api` |
| Pre-push hook push hangs then the SSH connection dies mid-run | The pre-push hook gates the PUSHING worktree's tree (not the `core.hooksPath` checkout's), and a long hook run can outlive GitHub's ~6-min idle SSH channel. Fix: set a local HTTPS pushurl once (`git config remote.origin.pushurl https://github.com/<owner>/<repo>.git` + `git config credential."https://github.com".helper '!gh auth git-credential'`), or per-push `git -c credential.helper='!gh auth git-credential' push https://github.com/<owner>/<repo>.git <branch>` |
| Merging PRs with UI changes | Never merge UI PRs autonomously - wait for human review |
| `git add path/[id]/file` fails | Use quotes: `git add "path/[id]/file"` (bash interprets brackets as globs) |
| Fixing only one instance of a pattern | Search entire codebase and fix ALL instances to maintain consistency |
| Building multi-step wizard modals | Use single-view scrollable modals (like ImportLinkModal) - all steps visible in one view |
| Using `\n` in `gh` CLI body/comment strings (renders as literal `\n`) | Use a heredoc or multi-line string for `gh pr create/edit/comment` |
| `git diff --quiet` to detect new files | Only sees tracked files; add `git ls-files --others --exclude-standard` for untracked |
| Feature removal only grepping source files | Grep ALL file types (tests, docs, comments) for feature name to catch all references |
| Editing `.claude/CLAUDE.md` directly | It's a symlink to `AGENTS.md` - stage `AGENTS.md` for git commits |
| Outdated Go version in docs | Search `go1\.2[0-9]\.` and update all references to match go.mod |
| E2E test waiting for page load | Use `domcontentloaded` + `getByRole` with explicit timeout, not `networkidle` |
| OAuth callback routes inside auth middleware | Register callback routes BEFORE `v1.Use(auth.APIKeyMiddleware)` - external providers can't include API keys |
| String concat for OAuth redirect params | Use `url.Values{}.Encode()` for ALL redirect query params - prevents injection vulnerabilities |
| Adding settings hooks (or any source) without test-map entry | Spec self-entries are now mechanically guarded — `scripts/hooks/test-map-coverage-check.sh` (pre-push LINT) fails the push if any `frontend/tests/e2e/*.spec.ts` is unmapped. An unmapped `frontend/src/`/`backend/internal/` SOURCE file is not push-blocking but produces a loud warning from `make test-e2e-diff`; add a `frontend/tests/e2e/test-map.json` entry mapping it to an `@area` to silence the warning + keep diff-selection accurate (e.g. new hooks like use-todoist-accounts.ts → `@area:settings`) |
| Redefining helper functions in new repository files | Use existing helpers from `repository/conversions.go` (uuidToPgUUID, stringToPgText, timeToPgTimestamptz) |
| Hand-rolling contact-list URL params (sort/order/search/filters) | All list URL state goes through `frontend/src/lib/contact-list-params.ts` (canonical types, defaults, `parseListContext`, `buildContactListUrl`/`buildContactDetailUrl`). Never build a `URLSearchParams` for list context inline — a param dropped by one builder silently breaks list↔detail round-tripping |
| SQL queries without ORDER BY for "unsorted" case | Always provide deterministic ordering; arbitrary DB order breaks navigation consistency |
| E2E `getByText` in strict mode with non-unique values | Use `.first()` or scope to specific row when DB may contain multiple matches from previous runs. (Not yet retired: E5 migrated the backend Go suite, not Playwright; a future per-worker-namespaced scenario catalog will reduce this.) |
| `make test-e2e-local GREP="..."` ignored | Use `PLAYWRIGHT_GREP="..." make test-e2e-local` (Makefile expects environment variable, not Make variable) |
| `bunx playwright test` directly fails with 401 | Use `make test-e2e-diff` or `make test-e2e-local` which set up API keys and environment |
| `make test-e2e-diff` missing clean DB in pre-push | `test-e2e-diff` target needs `e2e-db` prerequisite like `test-e2e` and `test-e2e-local` have. Without it, pre-push hook runs E2E tests against accumulated DB state instead of clean DB, causing flaky failures from test pollution |
| E2E parallel workers see each other's candidates in import/link modal | The resolver modal is keyed by candidate id: clicking your own (prefixed) card deterministically opens your own candidate. Assert it after opening with `expectModalCandidate(page, name)` so a regression fails loudly. The "X of Y" pager stays global — never assert absolute Y; if a test must reach a second own candidate while the modal is already open (post-advance), walk with the modal's own Prev/Next buttons |
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
| Adding new contact list filter | Update the 3 unified queries in contact.sql (ListContacts, ListContactIDs, CountContacts — their WHERE blocks are copies; keep in lockstep), plus repository ListContactsParams/ListContactIDsParams, handler query struct, `frontend/src/lib/contact-list-params.ts` (ContactListContext + parse/build), ContactListParams in types/contact.ts, and the API client |
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
| Integration tests fail with "Contact X should be in list" or limit-based assertions | Test DB (`personal_crm_test`) accumulates state across runs (e.g. 255 contacts). Reduced by toolkit seeding: scope assertions to your namespace (`syntheticNS(t)`) instead of asserting over the whole list/limit window — see `.ai/patterns/synthetic-seed-toolkit.md`. For any unscoped list-bounded test: `make e2e-db` is wired into `test-e2e` but NOT into `test-integration`, so run `make e2e-db` manually before `make test-integration` if it fails. Symptom: tests pass on a fresh DB and on `develop`, but fail mid-session after many runs |
| Test failures during PR rebase work | Verify against `develop` first (`git checkout develop && make test-integration`) — pre-existing failures (e.g. flaky LONG_TESTS-gated scheduler rescuer tests) are not regressions from your rebase. Distinguish before debugging |
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
| Integration suite in a LINKED git worktree silently using the shared :5432 | Running `make test-integration*` in a linked worktree auto-provisions a per-worktree Postgres PROCESS on a derived port (`scripts/worktree-test-pg.sh`, gh #433) so concurrent worktree agents don't contend for the shared `crm-postgres` connection budget + template-build lock. If the local toolchain is missing any prerequisite — Postgres 16 binaries, the `vector`/`pg_trgm`/`uuid-ossp` extensions, or an `en_US.UTF-8` locale — `ensure` warns LOUDLY on stderr and falls back to the shared :5432 (install via `make setup`). `CRM_WORKTREE_PG=0` forces the shared instance; `CRM_WORKTREE_PG=strict` fails the target instead of falling back; an explicit `TEST_DATABASE_URL=...` override wins and provisions nothing. The instance + its templates/clones persist across runs for speed — prune dead-worktree instances with `make test-pg-reap` (or `make test-pg-stop`/`test-pg-teardown` for this worktree). The real-cluster proof is `make test-pg-smoke` (NOT in pre-push; it owns a DB + binds a port); the shim-only unit test (`scripts/test/test-worktree-test-pg.sh`) rides the pre-push FILTER lane. The main checkout is untouched (byte-identical render against shared :5432) |
| Per-worktree test-pg instance wedged ("lock file already exists" on `make test-integration*`, or the run silently degraded to shared `:5432`) | A warm-reconcile failure in `scripts/worktree-test-pg.sh` clears an instance's `meta` after force-stopping its postmaster; if the force-stop could not bring the server down (postmaster ignoring SIGQUIT, or no resolvable `pg_ctl`), the meta is gone but the server is up, so the next `ensure` cold-starts against a live data dir and wedges. Recover with `make test-pg-teardown` — it stops by DATA DIR (not by meta) and force-stops (`pg_ctl -m immediate`, resolved independently of `psql`), then removes the instance ONLY once the server is verified down; the next `make test-integration*` re-provisions cleanly. `make test-pg-reap` only prunes instances whose worktree is GONE, so it will NOT touch a wedged LIVE-worktree instance — use teardown. If the postmaster still can't be stopped (ignores SIGQUIT, or no `pg_ctl` on PATH at all — extremely unlikely, since `start` required one), teardown deliberately KEEPS the data dir and warns rather than deleting it under a live server; stop it manually (`pg_ctl -D <datadir> -m immediate stop`, or kill the postmaster pid from `<datadir>/postmaster.pid`) and re-run `make test-pg-teardown`. |
| Capturing a tour's post-mutation state right after `waitForApi` | The response landing and the DOM re-rendering are DIFFERENT MOMENTS (measured: ~515ms for the dashboard's overdue widget to re-render after its refetch resolves). A capture taken in that window records the PRE-mutation UI alongside the POST-mutation API response — evidence that is internally contradictory but looks perfectly coherent to the judge, which will then produce a confident, well-cited, COMPLETELY FALSE regression report. The grounding rule cannot save you: the citation is real, the inference is not. Always wait for the OBSERVABLE CONSEQUENCE (`page.waitForFunction` on the thing that should have changed — a row gone, a count moved), bounded by a timeout, and record whether it happened in `fields` (e.g. `targetStillListed`, `reflectedWithinMs`). Do NOT throw on timeout: a real regression must be CAPTURED for the judge to grade, not killed before the capture. A racing tour is a false-positive generator and nothing downstream can detect it |
| Giving a JOURNEY intent `serves:` edges | `bindIntentCaptures` binds `{intent.id} ∪ servedBy`, sorts by **tour name alphabetically**, then truncates at `INTENT_CAPTURE_CAP` (8). A journey walks behaviors that the PER-SURFACE tours also tag, so serves edges pull in those tours' captures — which sort earlier (`cadence-followup` < `dashboard` < `relationship-loop`) and fill the cap, SILENTLY EVICTING the journey's own captures. The judge then grades the journey on everything except the journey, over captures from multiple tours whose uuid placeholders are per-test (so one contact appears under two identities). A journey intent MUST have `servedBy: []` and bind by DIRECT CAPTURE TAG (tag every capture in the tour with the intent id). See CAD-038 / `relationship-loop.tour.ts` |

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
| Contact soft-delete logic | Identity cleanup, note cascade, contact_method cascade, AND the person node: `ContactService.DeleteContact` propagates `node.deleted_at` (node.id == contact.id) in the same tx so the contact's assertions drop from graph reads (node-level live filter); assertions are retained in the table |
| Contact merge | `ContactService.MergeContacts` runs the GRAPH merge alongside the existing method/note/interaction transfers: tombstone the loser node (`SetNodeMergedIntoTx` → `merged_into`+`deleted_at`) and re-point its assertions onto the winner (`AssertService.MergeAssertionsTx`). The graph merge runs BEFORE the field-selection knowledge apply so the user's per-field choice is the last writer. The `RepointAssertionSubject/Object` sqlc primitives are called ONLY by `MergeAssertionsTx`, never the normal write path. The dormant `connection` table was dropped in migration 072 (D10), so merge no longer transfers/dedups `connection` rows |
| Sync provider implementation | Provider registry table in `.ai/patterns/sync.md` |
| New React Query hook | Hooks inventory in `.ai/patterns/frontend.md` |
| New API endpoint | API routes table in `.ai/guides/feature-development.md` |
| Query invalidation rules | `frontend/src/lib/query-invalidation.ts` domain events |
| New database table | Database tables in `.ai/guides/architecture.md` |
| New entity, sync source, or downstream record | Add the matching factory (`synthetic/factory`) + replay/seed coverage (`synthetic/replay` adapter, profile in `synthetic/profiles.go`) to the synthetic toolkit — new features write their own seeding (see `.ai/patterns/synthetic-seed-toolkit.md`) |
| OAuth flow changes | Both callback route AND auth URL endpoint |
| Contact method types | `contact_method` CHECK constraint + `identity.IdentifierType` enum |
| Cadence options | `contact` CHECK constraint + frontend dropdown options |
| `contact.location`/`birthday`/`how_met` write path | These are DERIVED cache columns, NOT directly writable. `KnowledgeCacheUpdater` is their sole writer (recompute from the current-accepted `lives_in`/`birthday`/`how_met` assertion). The contact SQL (`CreateContact`/`UpdateContact`) does NOT write them. Any code that wants to set one MUST emit an assertion via `AssertService` + refresh via `KnowledgeCacheUpdater.RefreshTx` (see `ContactService.knowledge` / `EnrichmentService.assertInferredKnowledge`). Adding a new direct writer of these columns is a bug; route it through the assertion store |
| E2E test file patterns | `frontend/tests/e2e/test-map.json` tag mappings |
| Scheduled job changes | Scheduler section in `.ai/guides/architecture.md` |
| New sortable contact field | Shared ORDER BY ladder in ListContacts AND ListContactIDs (copies — keep in lockstep) + handler oneof + `SortField` type + `ContactListParams.sort` |
| New contact list filter param | 3 unified SQL queries (ListContacts/ListContactIDs/CountContacts, shared WHERE) + repo params + handler query struct + `lib/contact-list-params.ts` + ContactListParams type + API client |
| Wire DTO structs covered by `backend/tygo.yaml` (e.g. `handlers/contact_dto.go`, `api/response.go`) | Run `make api-types` and commit the regenerated `frontend/src/types/generated/*` — the CI api-types-drift job and the pre-push LINT lane (`make api-types-check`) fail on drift. New JSON payload structs for a covered domain go in the `*_dto.go` file, not the handler file |
| Semantic column to table with dedup logic | All FindInWindow queries (add param), unique constraints, repository signatures, service call sites, `make sqlc`, regression test for false-positive case |
| New build/run site for `crm-api` or `crm-admin`, or a new worker/type the binary needs | Build the PACKAGE (`./cmd/crm-api` / `./cmd/crm-admin`), NEVER a file (`cmd/crm-api/main.go`). New workers/types/adapters go in sibling `.go` files in `package main` (e.g. `cmd/crm-api/wire_*.go`) — do NOT inline into `main.go`. Build sites: `Makefile` (build/crm-admin/ci-build-backend/api-build/test-cadence-*), `.github/workflows/ci.yml`, `.github/workflows/build-images.yml`, `frontend/playwright.config.ts`, `scripts/start-backend.sh`, `scripts/dev-seed.sh`, `infra/install-systemd.sh`. Sole file-mode exception: `swag init -g cmd/crm-api/main.go` (`api-docs`). Confirm the ldflags stamp still lands (`build-images.yml` grep + `TestHealthEndpoint_StampedBuildInfo`). |
| Path-filter groups (`path-filters.yml`) | CI dorny step (`.github/workflows/ci.yml` `changes` job) AND `scripts/hooks/pre-push` group-aware parser (`file_in_group` call sites + `check_swift`) AND the filter unit-test harness assertions |
| New pre-push test phase/command (`.ai/pre-push.json`) | The hook's `run_phases_parallel` lane classifier in `scripts/hooks/pre-push` (DB/port commands run exclusive-last, others concurrent) AND the phase guard test `scripts/hooks/test/test-pre-push-phases.sh` |
| Makefile test-parallelism (`test-integration*` `-p`/`-parallel`) | `scripts/test-parallelism.sh` (single-source formula) + the render guard `scripts/ci/test-parallelism-render-guard.sh` + the CI `GITHUB_ACTIONS` 4/4 pin (keep CI byte-identical) |
| Per-worktree test-pg resolver (`scripts/worktree-test-pg.sh`) | Makefile `TEST_DATABASE_URL` default (the `GITHUB_ACTIONS` ifeq split + `$(or $(WORKTREE_TEST_DB_URL),literal)`) + the `worktree-test-pg-ensure` prereq on `test-integration{,-fast,-slow}` + the render guard's section-5 assertions (forced-shared byte-identity + resolver-invocation counts via `CRM_WORKTREE_PG_COUNT_FILE`) + the shim-only unit test `scripts/test/test-worktree-test-pg.sh`. Keep the main-checkout/CI render byte-identical (verified render-vs-render) |
| App logic that changes behavior in a domain with a `spec/<domain>.yaml` file | Update the affected behaviors in `spec/<domain>.yaml` in the SAME PR — extend-in-place vs retire-and-mint per `spec/README.md` — and run `make spec-lint` + `make spec-coverage`. Every domain is `e2e_settled: true`, so a new/changed `surface: ui` then-item must land with its citing E2E test or an explicit waiver in the same PR (orphans block), and editing a cited behavior's then list must update citing indexes (out-of-range citations block) |
