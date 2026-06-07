# A — Deploy Automation (self-hosted runners) — Design

**Date:** 2026-06-07
**Status:** Skeleton — decisions locked, details to be filled out in its own brainstorm.
**Author:** spengrah (brainstormed with Claude)
**Parent:** `2026-06-07-deploy-and-staging-overview-design.md`

## Scope

Merging to main auto-deploys the new build to the Pi (priority) and the Mac daemon (nice-to-have), via **self-hosted GitHub Actions runners**, gated on green CI, with the repo kept **public** and the runner unreachable from PRs. Replaces manual `make deploy-all`. Independent of the VPS/staging work.

## Decisions locked (from brainstorm)

- **Self-hosted runners on the Pi (label `pi`) and the Mac (label `mac`).** Rationale and trade-off analysis vs cloud+Tailscale and pull-loop are in the overview ("Deploy mechanism"). Outbound-only; no Pi-reaching credential off-box.
- **Build in the cloud; the Pi runner only deploys.** Keep heavy compile off the Pi (operator preference — reserve Pi compute for prod). A cloud `build` job builds the `arm64` **container images** (`crm-api` + `crm-admin` baked in, and the Next standalone frontend) and pushes them to **GHCR** (free for public repos), tagged by commit SHA. The self-hosted `deploy` job (Pi for main, staging-VPS for develop) does `podman pull <tag>` (outbound, over the runner's existing connection — no rsync, no SSH, no Pi-reaching credential), then update-tag + migrate + restart-Quadlet + `/health`. `deploy.sh` collapses to these post-build steps; rename to `deploy-artifact.sh` (or similar). The Go binaries still cross-compile on x64 with no emulation (`GOARCH=arm64`, COPY into a distroless arm64 base); the Next standalone is portable JS wrapped in an arm64 base — so cloud image build stays as cheap as the tarball build would have been.
- **Reusable workflows for test logic.** Factor the existing CI suites into a `workflow_call` workflow; both `ci.yml` (PR + push) and the deploy workflow `uses:` it. Reuse the *build logic* (`make ci-build-backend` / `ci-build-frontend`), not CI's current job outputs — those bake a dummy `NEXT_PUBLIC_API_KEY`, don't upload, are path-gated, and omit `crm-admin`. A dedicated artifact-producing `build` job calls the same targets with real (non-secret) config.
- **`develop` is introduced in A** (not deferred to C): A establishes both `develop`→staging and `main`→prod deploys. (C then layers the staging *stack/data* onto the `develop` deploy A creates.)
- **Frontend API key: inject server-side at the Caddy edge (option 2a, Caddy flavor).** The key must never reach the browser, never be baked into the build, never enter GitHub. Prod topology (confirmed): browser → Tailscale Serve (`/`→`:80`) → **Caddy** path-splits `/api`→`:8080`, `/`→`:3001`; backend also listens on `*:8080` for direct service clients. So: add `header_up X-API-Key {env.API_KEY}` to Caddy's `/api` `reverse_proxy`, supply `API_KEY` to `caddy.service` from the host env, and **delete** the `X-API-Key` header from `frontend/src/lib/api-client.ts` (and `settings/page.tsx`, `test-api/page.tsx`). Browser sends no key; Caddy injects it; Tailscale guards the browser→Caddy leg. Zero added latency (Caddy already proxies `/api`), zero Tailscale Serve change. Service clients (Mac daemon, future MCP) keep hitting `:8080` directly with their own key. `NEXT_PUBLIC_API_URL` stays empty (same-origin) for both environments — not environment-specific.
- **Mac runner** runs `make mac-daemon` + `crm-mac install --upgrade` locally; fires only when the Mac is awake (job queues otherwise). The runner's ~50s long-poll will keep the Mac from deep sleep (like `caffeinate`) — acceptable, or leave Mac manual.
- **Public-repo lockdown** (full list in overview "Security model"): push-to-main-only trigger; fork-PR approval for all outside collaborators; cloud CI vs self-hosted split; green-CI gate; `production` GitHub Environment with `main`-only branch rule; hardened unprivileged runner user + narrow sudoers; SHA-pinned actions.

## Resolved this session

- Build location → **cloud build of `arm64` container images pushed to GHCR, pulled by the deploy runner** (above).
- Runtime & artifact → **full containerization; the container image is the deploy artifact; Podman Quadlets supervise; Caddy stays native** — split into standalone precursor **A0** (`2026-06-07-containerization-cutover-design.md`); A depends on it. One-time prod cutover from today's systemd + Docker-Postgres hybrid (Postgres already containerized → near-zero data risk).
- Test reuse → **reusable `workflow_call` workflow** shared by `ci.yml` and the deploy workflow (above).
- Branching → **A introduces `develop`** (`develop`→staging, `main`→prod).
- Frontend key → **Caddy-edge injection (2a)** — key never in browser/build/GitHub (above).
- `NEXT_PUBLIC_API_URL` → **stays empty (same-origin)**, not environment-specific.
- Caddyfile → **moved into `infra/`**, version-controlled and deployed like the Quadlet units; Caddy itself stays **native** (not containerized) — binds `:80` cleanly and sidesteps the rootless privileged-port wrinkle (implementation detail in open threads).
- Migrations & rollback → **explicit pre-cutover migration (1b-offline) + fully automated snapshot-restore, no expand/contract mandate, no human gate** — see dedicated section below.

## Containerization & artifact (see A0)

The deploy artifact is a **container image** and prod/staging run **fully containerized under Podman Quadlets** (Caddy native). The one-time cutover from today's systemd + Docker-Postgres hybrid — building the images, authoring the Quadlet units, migrating the Postgres volume Docker→Podman — is its own standalone precursor sub-project, **A0** (`2026-06-07-containerization-cutover-design.md`). A0 produces the images + Quadlet units that A automates and C reuses for staging; **A depends on A0 landing first.** Load-bearing consequence for A: the build job produces + pushes an `arm64` image to GHCR, and the deploy job is `podman pull` + tag-swap + Quadlet restart (reflected throughout A).

## Migrations & rollback (decided)

Fully automatic deploy with automatic rollback — no authoring-discipline mandate, no human approval gate. Rests on three composing choices: explicit pre-cutover migration (1b-offline), automated snapshot-restore, and staging-as-canary.

**Why no expand/contract requirement.** Expand/contract is only needed when rollback is code-only (the old binary must survive the new schema). 1b-offline has no version-skew window (old code is stopped before migrating), and auto-restore rolls back code *and* schema together to a consistent prior state — so backward-compatible migrations are a free nicety when convenient, never a requirement. This is the deliberate trade for dropping the discipline burden: recoverability comes from the snapshot, not from authoring care.

**Deploy/rollback flow** — one self-contained script (`deploy-artifact.sh`), runs identically on staging via `develop` and prod via `main`:

```
0. record current image tag as the rollback target
1. podman pull new image tag
2. pending migrations?
   NO  → update Quadlet to new tag → restart → health-gate
            FAIL → restore previous tag → restart → NOTIFY → exit 1
            (no DB touched → no restore needed; fast, zero DB downtime)
   YES → backup DB (physical snapshot, services stopped)
         crm-admin --migrate
            FAIL → restore snapshot → restart previous tag → NOTIFY → exit 1
                   (prod returns to exact pre-deploy state; dirty-state wiped)
         update Quadlet to new tag → start services
         health-gate (/health + frontend + smoke-test.sh)
            FAIL → stop new → restore snapshot → restore previous tag → restart → NOTIFY → exit 1
            PASS → mark last-good, retain snapshot until next success → NOTIFY → exit 0
```

**Components to build:**
- `crm-admin --migrate` subcommand reusing `db.RunMigrations` (app + River migrations), runnable as an explicit pre-cutover step. Today migrations run on backend startup (`cmd/crm-api/main.go:258`) and `Fatal` on error — keep that as an idempotent backstop (it will be no-change after the explicit step), but the explicit step is what gates the deploy.
- A **restore counterpart to `scripts/backup-db.sh`** (today backup-only): copy the `.bak` volume back. Retain the snapshot until the next successful deploy; never delete it on a failed restore (manual recovery must always be possible).
- **Notifications** on success and (loudly) on failure/rollback — a requirement, since no human watches.

**Optimizations / safety:**
- Back up **only when migrations are pending** (golang-migrate reports no-change cheaply) — code-only deploys skip the Postgres stop entirely: fast, zero DB downtime, artifact-only rollback.
- Staging rehearses both the migration and the rollback on every deploy; deliberately push a known-bad migration to staging once to verify the rollback path actually works.

**Residual risks (accepted):**
- Auto-rollback covers **deploy-time failures only** (migration errors + immediate health-check). A latent bug found hours later → **fix-forward** (snapshot already rotated; can't restore without losing accumulated data).
- Physical snapshot/restore downtime **scales with DB size** (Gmail-correspondence growth) — revisit logical dump or WAL-based PITR if it becomes painful.
- Down-migrations (~60 `.down.sql` exist) are a **manual last resort only** — never auto-run (data-loss risk).

## Open threads / TODO (fill out here)

- [ ] **Caddyfile into the repo.** The Caddyfile is currently hand-managed on the Pi (`/etc/caddy/Caddyfile`), not version-controlled. Bring it into `infra/`, parameterize the upstreams + the `header_up X-API-Key` injection, and deploy it like the systemd units. Required for both automated key-injection config and staging parity (staging has no Caddyfile yet). Provision `API_KEY` into `caddy.service`'s environment (scoped env file vs `EnvironmentFile=/srv/personalcrm/.env`).
- [ ] **SSR backend calls.** Confirm whether Next does any server-side data fetching against the backend (api-client returns relative URLs server-side). If so, route those server-side calls direct to `127.0.0.1:8080` with the key from Next's runtime env (or through Caddy) — don't let the key-removal break SSR. Likely minimal (app is React-Query/client-heavy), but verify.
- [ ] **Workflow shape / branch→env mapping.** With reusable workflows: `deploy.yml` job `test` (`uses:` the reusable suite) → `build` (cloud, real config) → `deploy` (self-hosted, label by branch: `pi` for main, `staging` for develop). Decide `workflow_run` vs in-workflow `needs:` for the green-gate; confirm `services:` (Postgres) + `secrets: inherit` semantics in the reusable workflow. Decide whether deploy build is path-gated or always-build.
- [ ] **Image portability / cross-arch build.** Build `arm64` images in the x64 cloud: the Go binary cross-compiles with `GOARCH=arm64` (no emulation; COPY into a distroless arm64 base — already proven in CI). The Next standalone is portable JS, but **verify `node_modules` has no x64-native addons** before wrapping it in an arm64 base (if any exist, build the frontend image on a free public **arm64** GitHub runner, or via buildx/QEMU). The same arm64 image then serves both prod (Pi) and staging (VPS) — single-arch artifact (resolved by ARM parity).
- [ ] **Concurrency.** `concurrency: deploy-pi` / `deploy-staging` / `deploy-mac` so two deploys never overlap per target.
- [ ] **Runner installation runbook.** One-time `config.sh --labels pi` (and `mac`, `staging`) + systemd service for the runner agent; dedicated unprivileged runner user; exact narrow sudoers entry (only `systemctl restart personalcrm.target` + service/Caddyfile install + the backup/restore stop-start).
- [ ] **`deploy-all.sh` / `.deploy-state` fate.** Keep `make deploy-all` as a manual escape hatch, or retire it? Per-target change detection may still be useful for the manual path.
- [ ] **Notifications (required, not optional).** Notify on deploy success and — loudly — on failure/rollback, since prod deploys are unattended (no reviewer gate). Pick a channel. Deploy logs also live in the Actions UI.
- [ ] **Migration/rollback specifics.** Exact `crm-admin --migrate` interface + pending-migration check; restore script; snapshot retention/rotation; health-gate contents (which `/health` + smoke checks); how staging deliberately tests the rollback path.

## Existing code to build on

- `scripts/deploy.sh`, `scripts/deploy-mac-daemon.sh`, `scripts/deploy-all.sh`, `.deploy-state/`.
- `Makefile` targets: `deploy`, `deploy-pi`, `deploy-mac`, `deploy-all`, `ci-build` (`ci-build-backend` / `ci-build-frontend`), `setup-pi`.
- Systemd: `personalcrm.target`, `personalcrm-{backend,frontend,database}.service` in `infra/`. Frontend runs the Next standalone server (`node server.js`) on `:3001` with `EnvironmentFile=/srv/personalcrm/.env`. (These become Quadlet `.container` units after the containerization cutover; the `.target` orchestration model is preserved.)
- Caddy reverse proxy on `:80` (`/etc/caddy/Caddyfile`, currently hand-managed on the Pi) — the `/api`↔`/` path-splitter and the new key-injection point.
- Frontend key-header call sites to change: `frontend/src/lib/api-client.ts`, `frontend/src/app/settings/page.tsx`, `frontend/src/app/test-api/page.tsx`.
- CI: `.github/workflows/ci.yml` (to be refactored into a reusable suite + a deploy workflow).

## Dependencies

None hard for the **main→prod** path — standalone and first in sequence. The **develop→staging** deploy A creates is only *useful* once B (VPS) and C (staging stack + Caddyfile) exist; A can land the `develop` branch + workflow wiring first and C fills in the target.
