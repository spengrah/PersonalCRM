---
name: verify-personal-crm
description: Launch an isolated, seeded Personal CRM instance (Go API + Next.js web UI) from this checkout and drive it with Playwright to prove a change works the way a user sees it, capturing screenshots, API traffic, a trace and database state. Use when asked to verify, demo, screenshot or prove a frontend or backend behavior in the real app, rather than through unit, integration or E2E tests.
---

# Verify Personal CRM

Personal CRM's user surface is the Next.js web UI; the Go API behind it is the secondary surface (the Mac daemon and `crm-admin` CLI are not covered here). This skill starts a private copy of both from the current checkout, against its own Postgres database seeded with the synthetic `standard` world, on ports derived from the checkout path. It never touches the dev instance (`make dev`, ports 3000/8080, the `personal_crm` database), the E2E stack, staging or prod.

All helpers live in `.agents/skills/verify-personal-crm/bin/` and run from any directory inside the checkout. Paths below are relative to the repository root.

```bash
V=.agents/skills/verify-personal-crm
$V/bin/launch                                   # ~1 min warm; prints the URLs when ready
$V/bin/doctor                                   # exit 0 = worth driving
$V/bin/run create-contact $V/examples/create-contact.mjs
$V/bin/cleanup                                  # evidence survives
```

## Launch

Run `.agents/skills/verify-personal-crm/bin/launch`. It:

1. Refuses if this checkout already has an instance recorded, if either port is busy, or if a `next dev` already runs from this checkout's `frontend/` (two dev servers corrupt the shared `frontend/.next`). If the user's `make dev` runs from the same checkout, verify from a worktree instead.
2. Refuses if `frontend/node_modules` is missing. In a worktree, run `make worktree-deps`; in the main checkout, `cd frontend && bun install --frozen-lockfile`.
3. Starts Postgres if needed with `scripts/e2e-pg.sh ensure`: the shared `crm-postgres` container when a Docker daemon answers; otherwise this checkout's own cluster from `scripts/worktree-test-pg.sh` (Postgres 16 binaries, no Docker, no sudo); native Postgres on port 5432 when neither is available. It then creates a fresh database with `scripts/e2e-db-reset.sh`, which refuses if a foreign `crm-api` is attached to it.
4. Seeds it with `crm-admin --seed --profile standard` (migrations run first). Set `VERIFY_SEED_PROFILE` to seed another profile.
5. Builds `crm-api` from this checkout, stamped with `HEAD`, and starts it with the E2E environment (`.env.example.testing`, `CRM_ENV=testing`, `ENABLE_EXTERNAL_SYNC=true`, `EVENT_BUS_INGEST_ENABLED=true`). It never reads `.env`, so no real credentials or accounts are loaded.
6. Starts `next dev --turbopack` with `NEXT_PUBLIC_API_URL` pointing at this backend.

Ready means launch printed `✓ ready: <url>`: the backend answered `/health` and the frontend served `/dashboard`. Logs, PIDs and the binary are in `.agents/log/verify/instance/` (`backend.log`, `frontend.log`, `seed.log`, `db-reset.log`, `build.log`). On any failure launch names the log to read; run `bin/cleanup` before retrying.

Coordinates come from a slot (0–399) hashed from the checkout path: frontend `127.0.0.1:3400+slot`, backend `127.0.0.1:8400+slot`, database `personal_crm_test_verify_<slot>`. Print them with `eval "$(.agents/skills/verify-personal-crm/bin/env)"`, which exports `VERIFY_BASE_URL`, `VERIFY_API_URL`, `VERIFY_API_KEY`, `VERIFY_DB_NAME` and friends. If two checkouts collide on a slot, set `VERIFY_SLOT=<n>` for every helper call in one of them.

A backend change needs a relaunch: the binary is built once. Frontend changes hot-reload.

## Doctor

Run `.agents/skills/verify-personal-crm/bin/doctor` first, and again whenever anything looks off. It is read-only and exits non-zero if any check fails:

- Both recorded processes are alive, and each port is held by our process group, not someone else's server.
- `/health` is healthy and its `version.git_commit` equals `HEAD`, and no `backend/**/*.go` file is newer than the built binary. A stale backend fails here; relaunch.
- The API key is accepted, the database has live contacts, and the frontend serves `/dashboard`.

## Drive

Drive the browser with Playwright through `lib/pw.mjs`. Write a short ES module, then run it with `bin/run <label> <script.mjs>`. The script may live anywhere (a scratch directory is fine); import the library by a path relative to the script or by absolute path. `examples/create-contact.mjs` is the reference.

```js
import { drive } from '/abs/path/to/.agents/skills/verify-personal-crm/lib/pw.mjs'

await drive(async s => {
  await s.step('dashboard', async p => {
    await p.goto('/dashboard')                       // baseURL is the verify frontend
    await p.getByRole('heading', { name: 'Action Required' }).waitFor()
  })
  const { data } = await s.apiGet('/api/v1/contacts/overdue')   // observe, never act
  s.check(data.data.length > 0, 'overdue list is non-empty')
})
```

The session object `s` provides:

- `s.page`: a Playwright page (`baseURL` set, desktop viewport). Pass `{ device: 'iPhone 13' }` as the second argument to `drive` for a mobile viewport.
- `s.step(label, fn)`: logs the action, runs `fn(page)`, then screenshots the result.
- `s.shot(label)`: screenshot without an action.
- `s.check(condition, message)`: records an assertion. A false check fails the run.
- `s.note(label, data)`: records an observation next to the steps.
- `s.apiGet(path)`: reads the backend with the API key and records the response.

`drive` always closes the session, even when the script throws, so a failing run still leaves its evidence.

Locate elements by role and accessible name, label, placeholder or `data-testid`, as the E2E specs do. Each feature file in `features/` lists the real locators. Wait for rendered content (`locator.waitFor()`, `page.waitForURL`), never `networkidle` or fixed sleeps.

Other helpers:

- `bin/psql -c "<SQL>"` reads the verify database in whichever Postgres `scripts/e2e-pg.sh` resolves (`docker exec crm-postgres psql`, the checkout's own cluster, or native). Use it to read side effects; do not write.
- `bin/api <METHOD> <path> [json]` calls the backend with the API key and prints the body plus `[HTTP <code>]`. Use it for reads and for setup the UI cannot reach, never to perform the behavior under proof.

Seeded data: every seeded name starts with `synth-standard-`. Find contacts through the UI or `bin/api GET /api/v1/contacts?search=...` rather than hardcoding names. Create your own records with a unique whole-word marker (the example uses `Verify <stamp> Person`) so they never collide with seeded ones.

## Evidence

`bin/run` creates `.agents/log/verify/runs/<UTC timestamp>-<label>/` (gitignored) and prints its path. It holds:

- `drive.mjs`: a copy of the exact script that ran.
- `NN-<label>.png`: full-page screenshots after each step.
- `steps.jsonl`: steps, notes and checks in order.
- `api.jsonl`: every `/api/v1` request the page made, with method, path, status, and request and response bodies.
- `trace.zip`: the Playwright trace. Open it with `frontend/node_modules/.bin/playwright show-trace <path>`.
- `summary.json` and `run.log`: counts, 4xx responses (`clientErrors`), problems, and the exit code.

A run exits 1 on any problem: a page error, a console error other than a resource-load failure, an API response of 500 or above, a false `check`, a thrown script, or a request to a localhost port that is not this instance's (a frontend compiled against the wrong API). 4xx responses are listed but do not fail the run, because some are expected locally (see Gotchas).

Proof standards:

- Take the expected outcome from `spec/<domain>.yaml` by behavior ID, not from the feature map or the E2E specs. The map says how to drive and where to look; the spec says what should be there. Name the IDs you proved in the report.
- Exercise the real user path: navigate, click and type as a user would. Do not call the API or the `/api/v1/test/*` seeding routes to perform the behavior under test, and do not copy the E2E specs' `page.route` mocks.
- Capture the action and the resulting state with a `step` for each action, not only the final screen.
- Verify the side effect beside what is visible: read the row with `bin/psql` or the resource with `s.apiGet`, and record it in the run (`s.note`) or save the query output into the evidence directory.
- Integrations (Google, Todoist, Telegram, WhatsApp) are not connected. Their absence is the production boundary; do not fake a connection. A behavior that needs one cannot be proven here; say so instead of proving a nearby path.
- Report the evidence directory path with the result.

## Cleanup

Run `.agents/skills/verify-personal-crm/bin/cleanup`. It stops the two servers by their recorded PIDs (checking the command line first, never by process name), drops the verify database, and removes `.agents/log/verify/instance/`. It leaves Postgres running and never touches `.agents/log/verify/runs/`. It is safe after a failed or partial launch, and it warns, without killing anything, if something else still holds a port.

## Gotchas

- `CRM_ENV=testing` compresses cadences (`CAD-003`), so a rendered "day" is seconds and `days overdue` numbers are huge. Assert relative order and membership, not counts of days.
- Some behaviors carry `waivers` for testing-mode cadence (for example `CAD-045` and `CAD-046`). Read a behavior's waivers before proving it here.
- Contact tasks routes exist only when Todoist is configured, so contact detail pages log three 404s for `/api/v1/contacts/<id>/tasks`, and adding a task fails. Telegram and WhatsApp settings show "Configuration Required". These are expected here.
- Never click an OAuth connect, sync, Telegram or WhatsApp pairing control: they reach external services. `features/settings.md` lists them.
- Native `confirm()`/`alert()` dialogs (contact delete, task removal, backup validate) block the page. Register `page.once('dialog', d => d.accept())` before the click.
- The verify database persists across runs until cleanup, so records created by one run appear in the next. Relaunch for a clean world.
- The first visit to each route compiles it under `next dev`; if the first `waitFor` on a route times out at Playwright's default 30 seconds, pass `{ timeout: 60_000 }` rather than retrying.

## Feature map

`features/README.md` indexes one recipe per user-facing feature. A proof that drives one entry point is incomplete when the map lists others. Keep the map true as the app changes with the `maintain-verification-skill` skill.
