# C — Staging Environment — Design

**Date:** 2026-06-07 (updated 2026-06-26)
**Status:** Design — READY TO PLAN. The primitives this spec depends on all shipped since it was written (A deploy automation + A0 containerization + D synthetic seed are COMPLETE and LIVE in prod; B's substrate is LIVE on `stovepipes`), which closed most of the original open threads. The two remaining design forks (staging unit shape, deploy runner model) are now decided — see "Status update 2026-06-26."
**Author:** spengrah (brainstormed with Claude)
**Parent:** `2026-06-07-deploy-and-staging-overview-design.md`

## Scope

An **always-on staging instance** of the CRM on the VPS, deployed from a **`develop`** branch, running the **identical container image set as prod** (Podman Quadlets), with all read sync sources disabled, Todoist outbound writes structurally guarded, reachable from the operator's phone over Tailscale, resettable to a known seed dataset on demand, and serving as the target for the agentic UX QA harness (#380). Cures the "can only verify UI changes in prod" pain.

## Architecture: staging *is* prod, run from the same image

The big simplification from this session: once the deploy artifact is a **container image** (decided in A) and the runtime is **Podman Quadlets** (decided in A/overview), the earlier "systemd-parity vs Compose" framing for staging **dissolves**. Staging is not a *similar* stack — it is the **identical `arm64` image set as prod**, run under the same kind of Quadlet units, on the VPS. Parity is by construction, which is the whole point: staging is a faithful canary for the prod deploy.

Everything that differs between staging and prod is **configuration, not composition**:

- env file (`CRM_ENV=staging`, no sync creds, own `DATABASE_URL`, own `CRM_API_KEY`) — per DECISION A (below) this `.env` is the ONLY app-level thing that differs from prod
- which branch deploys to it (`develop` vs `main`)
- its own Tailscale Serve / MagicDNS name (the Caddyfile is otherwise identical to prod's — same `:8080`/`:3001` upstreams, only the injected key differs)

> Superseded by DECISION A: the earlier "distinct published ports + `personal_crm_staging` DB name" is dropped — staging reuses prod's `:8080`/`:3001`/`personal_crm` verbatim; isolation is by separate tenant/host/volume, not by name.

This is staging's place in the **three-tier parity model** (see overview): **dev tier** (Mac + VPS sandbox; native/flat toolchain pinned to CI) → **staging** (prod-parity) → **prod**. Staging's parity contract is to match *prod*; the sandbox's is to match *CI*. Each environment matches the right neighbor for its job.

## Topology on the VPS

Staging runs as Quadlet-supervised containers; **Caddy stays native** on the VPS (mirrors prod, binds `:80` without the rootless-port wrinkle). Prod and staging Caddy live on different hosts, so there is no port conflict between them; distinct *published* ports on the VPS are for operator clarity, not necessity.

| Component | Staging (VPS) | Prod (Pi) | Notes |
|---|---|---|---|
| backend | container, published `:8080` | container, published `:8080` | same image + same unit (DECISION A) |
| frontend | container, published `:3001` | container, published `:3001` | same image + same unit (DECISION A) |
| postgres | container, db `personal_crm`, own rootless volume | container, db `personal_crm` | same image + unit; isolation by separate tenant/host/volume, not name |
| caddy | **native** `:80`, staging Caddyfile | **native** `:80`, prod Caddyfile | `/api`→backend, `/`→frontend, edge key injection |
| edge | Tailscale Serve → `:80` | Tailscale Serve → `:80` | own MagicDNS name |
| sandbox | standalone rootless Podman Quadlet (off-tailnet) | — | B; isolated from staging |

## Decisions locked (from brainstorm)

- **Same image, same Quadlets, on the VPS** (not ephemeral, not on the Pi). Co-locating on the Pi was rejected (resource contention + shared blast radius with prod). The staging units are the prod Quadlet units with a staging env file.
- **Caddy native on the VPS**, with a **staging-specific Caddyfile**: `/api` → backend container, `/` → frontend container, plus `header_up X-API-Key {env.CRM_API_KEY}` edge injection (A's pattern; the env var is `CRM_API_KEY`, sourced from a staging `/etc/caddy/crm.env` — match the prod Caddyfile, including its `@daemon` X-Mac-Host-ID bypass) so the **staging key never reaches the browser** either. `NEXT_PUBLIC_API_URL` stays empty (same-origin), as in prod.
- **Runtime tier of the three-tier parity model.** Staging's contract is prod-parity; reference the overview model rather than re-deriving it.
- **All read sources OFF** in staging — no credentials for Gmail / GCal / GChat / Telegram / Mac Contacts → providers never initialize → no real PII ingested onto the VPS. Realism comes from the synthetic seed (spec D), not live sync.
- **Todoist guarded by `CRM_ENV`:** hard **refuse + warn-log** at the Todoist Sync-client write methods (`Sync` with commands + `QuickAdd`, covering all ~6 task create/update paths) when `CRM_ENV` is not a production alias. **Scope correction (verified in `sync.go`):** this is defense-in-depth for the *partial-copy / restored-prod-DB* case ONLY — a verbatim full prod-`.env` clone (which carries `CRM_ENV=production`) is intentionally NOT defended, and an `.env` that simply *omits* `CRM_ENV` is treated as production (guard OFF). So the **primary** protection is "no Todoist creds in staging," and the staging `.env` MUST set `CRM_ENV=staging` explicitly. Audit result: Todoist `Sync`/`QuickAdd` are the only guarded outbound *data* writes; Google (gchat/gmail/gcal) + Telegram are read-only ingest; the Todoist OAuth connect/revoke POSTs in `oauth.go` are NOT behind this guard but only fire during an interactive connect/disconnect with creds (low risk on a cred-less staging).
- **`develop` → staging deploy** via a self-hosted runner on the VPS (label e.g. `staging`), reusing the spec-A runner mechanism, image artifact, and lockdown.
- **`make staging-reset`** = wipe the staging DB + reseed to a known dataset via `crm-admin --seed` (spec D). Neutralizes data drift; gives the QA harness deterministic state.
- **Branching inherited from A:** feature branches PR into `develop` (auto-deploys staging); promote by merging `develop` → `main` (auto-deploys prod). Same CI gate keeps `develop` green. Lightweight for a solo dev, but the canonical GitOps environment-per-branch pattern.
- **Migrations: staging is A's migration canary.** The same image-based deploy + automated snapshot-restore rollback (A) runs on every `develop` deploy, so a bad migration is caught on staging before it can reach prod. Deliberately push a known-bad migration to staging once to prove the rollback path actually works.
- **Phone access via Tailscale Serve** on the VPS (own MagicDNS name); auth = tailnet membership.

## Resolved this session

- Stack shape → **identical prod image set under Podman Quadlets** (the systemd-vs-Compose question dissolved once the artifact became an image).
- Caddy → **native on the VPS**, staging Caddyfile with edge key injection.
- Parity → staging = **runtime tier** of the three-tier model (prod-parity); sandbox/Mac = dev tier (CI-parity).
- `CRM_ENV` guard → **refuse + warn-log at the single Todoist client chokepoint**.
- Reset → **`make staging-reset` = wipe + `crm-admin --seed`**.
- Branching/migrations → **inherited from A** (`develop`→staging, staging as migration canary).

## Status update 2026-06-26 — substrate live, dependencies shipped, two forks decided

Since this spec was written (2026-06-07), the primitives it depends on all shipped and run in prod, which turns most of the original open threads from "design" into "integration." Recorded here so the implementation plan reads from accurate state.

**Dependencies — now satisfied:**

- **B (VPS substrate) — LIVE.** `stovepipes` is provisioned (netcup arm64); the `staging` tenant exists (uid 1995, linger on, rootless Podman, the `~/.config/containers/systemd` Quadlet drop-in dir already created at home `/var/lib/staging`), under its cgroup slice ceiling, behind the host nftables (host→Pi DROP + sandbox→tailnet DROP). The deferred sandbox↔staging observe path (observe network + rule-3 ALLOW + read-only PG role) is still spec C's to land.
- **D (synthetic seed) — COMPLETE.** `crm-admin --seed` / `--reset-and-seed --profile prod-shaped` exist and back the reset.
- **A (deploy automation + A0 containerization) — COMPLETE + LIVE in prod.** Prod runs the GHCR image set under Podman Quadlets; `scripts/deploy-artifact.sh` does pull → migrate-check → snapshot → migrate → image-swap → health-gate → auto-rollback-with-restore; a self-hosted runner (`gha-runner`, a system user ≠ workload ≠ root, single sudoers → one root-owned script) deploys on `push: main` via `.github/workflows/deploy-prod.yml`.

**Original open threads — resolved by shipped work (no longer TODO):**

- ~~**`CRM_ENV` guard implementation**~~ → **BUILT.** `backend/internal/todoist/sync.go` returns `ErrNonProdWriteRefused` from both write methods (`Sync` when commands are present, and `QuickAdd`) via `NewSyncClientForEnv`; the factory is wired once in `cmd/crm-api/main.go` with the running `CRM_ENV`; `config.IsProductionCRMEnv` treats unset as prod; tests cover the non-prod refuse set. The spec's "single chokepoint" landed as the two write methods on the client (still the client layer). Audit done (see the scope correction in "Decisions locked"): Todoist `Sync`/`QuickAdd` are the only guarded outbound *data* writes (Google + Telegram are read-only ingest); the `oauth.go` connect/revoke POSTs are unguarded but cred-gated, so harmless on a cred-less staging.
- **`make staging-reset` → PARTIALLY built; needs a rootless-tenant variant** (moved to remaining-TODO). The reset *primitive* is done (`crm-admin --reset-and-seed --profile prod-shaped` = HARD wipe of every live data table + reseed, resolving the soft-delete-doesn't-cascade concern). But `scripts/staging-reset.sh` as written drives a *system* `systemctl` service + a repo-local `.env` + `go run` — it does NOT drive the rootless `staging` tenant (uid 1995, `systemctl --user`, container-only, no Go toolchain on the box).
- ~~**QA harness (#380) targeting**~~ → the reset hook will be harness-callable once the rootless variant lands; the tour/scheduling internals remain #380's.

**Forks — now DECIDED (integration detail, not open design):**

- [x] **Staging Quadlet units → DECISION A: reuse the prod units; the staging `.env` is the only app-level delta.** Staging reuses the prod `.container`/`.network`/`.volume` units with the same container names (`crm-backend`/`crm-frontend`/`crm-postgres`), volume (`personalcrm-db`), PG role + DB (`crm_user` / `personal_crm`, both baked as `Environment=` in the db unit), and published loopback ports (`:8080`/`:3001`, Caddy `:80`). No port collision: staging is a separate rootless tenant on a separate host from prod, and the only other tenant on the box (sandbox) publishes solely its sshd on `127.0.0.1:2222` (verified against the sandbox slot contract). Isolation is by separate tenant/host/volume, not by name — so the earlier distinct-ports (`:8081`/`:3002`) + `personal_crm_staging` idea is dropped (it would actually *fight* verbatim reuse). **Two caveats keep "verbatim" honest (both surfaced in review):** (1) the units hardcode host paths `EnvironmentFile=/srv/personalcrm/.env` and a bind-mount of `/srv/personalcrm/infra/init-db.sql`, so staging must **replicate that same `/srv/personalcrm/{.env,infra/init-db.sql}` layout on the VPS** (staging contents; `.env` readable by uid 1995). Because the path matches prod's, `deploy-artifact.sh`'s default `ENV_FILE` needs no override and staging's `DATABASE_URL` uses db `personal_crm`. (2) `deploy-artifact.sh` itself is parameterized (`CRM_USER`/`CRM_HOME`) and reused, **but the `backup-db.sh`/`restore-db.sh` it shells out to are NOT** — they hardcode `/var/lib/personalcrm` + `id -u crm`, so on the `staging` tenant they die and break the snapshot→migrate→rollback (the migration canary) on any pending-migration deploy. So "reuse `deploy-artifact.sh`" is true, but the *plumbing as a whole* needs a small, prod-safe parameterization (see remaining-TODO) — NOT "zero change." The units are placed once under `/var/lib/staging/.config/containers/systemd/` (as A0's cutover placed prod's); `deploy-artifact.sh` thereafter only rewrites the live unit's `Image=` line.
- [x] **`develop`→staging deploy → DECISION (a): self-hosted runner, reuse `deploy-artifact.sh` behind a wrapper.** A `deploy-staging.yml` deploys each new `develop` SHA, but the **TRIGGER must be `on: workflow_run`** keyed to `ci.yml` (and `build-images.yml`) **completing** on `develop` — NOT `push: develop`. (deploy-prod's `status=completed` REST gate works only because `main` is fast-forwarded to an *already-CI'd* develop SHA; a `push: develop` deploy fires concurrently with its own CI + image build, so the gate would read `in_progress`/`missing` and abort nearly every deploy — flagged in review.) No `pull_request` trigger (a push/workflow_run runs the workflow from the default branch, never fork code; the repo is private + solo). It still verifies the SHA's CI + image-build success, then runs a root-owned **`deploy-staging.sh` wrapper** on the box. **Env-trust seam (load-bearing):** `deploy-artifact.sh` *trusts* its `CRM_USER`/`CRM_HOME`/`DEPLOY_ENV_FILE` env to decide what it mutates, and `sudo` resets the environment — so the staging overrides MUST be hardcoded inside the root-owned, runner-immutable `deploy-staging.sh` (which `exec`s `deploy-artifact.sh "$SHA"`), **never** supplied by the workflow (a runner that controls those vars could redirect a root-run deploy; the sudoers entry must not use `SETENV`/`env_keep`). Runner trust model mirrors prod exactly: a dedicated `gha-runner`-class system user (≠ the `staging` tenant, ≠ root, no login shell), whose entire root capability is one sudoers line to `deploy-staging.sh`. **Safe-by-construction on this box:** firewall-isolated from prod (nftables rule 1, stovepipes→Pi DROP, both families), no real PII (synthetic seed only), no prod creds (sync off + the Todoist guard), outbound-only / no inbound. (GHCR pull: `build-images.yml` creates the packages *private* on first run — verify they are public, else give the tenant a rootless `podman login`; see remaining-TODO.) Reusing `deploy-artifact.sh` (with the backup/restore parameterization from DECISION A) keeps staging a faithful **migration canary** (same snapshot→migrate→rollback path as prod).

## Open threads / TODO — genuinely remaining (the plan's actual work)

Two workstreams: **deploy plumbing** (adapt A's reusable-but-prod-shaped scripts/workflow for a second tenant) and **host provisioning** (stand the staging stack + edge up on stovepipes). Order: plumbing + host provisioning land before the first-run bootstrap, which lands before turning on the automated runner.

**Deploy plumbing:**

- [ ] **Parameterize `backup-db.sh` + `restore-db.sh` for `CRM_USER`/`CRM_HOME`** (P0 — blocks the migration canary). They hardcode `/var/lib/personalcrm` + `id -u crm`, so `deploy-artifact.sh`'s snapshot/rollback dies on the `staging` tenant and aborts every pending-migration deploy. Make them honor the same env `deploy-artifact.sh` already reads (export `CRM_USER`/`CRM_HOME` to the children), or ship staging copies wired via the wrapper's `BACKUP_SCRIPT`/`RESTORE_SCRIPT`. Prod-safe (defaults unchanged); add tests.
- [ ] **`deploy-staging.yml` trigger = `on: workflow_run`** keyed to `ci.yml` (+ `build-images.yml`) completion on `develop` — NOT `push: develop` (P0 — `push` races its own in-progress CI and aborts the `status=completed` gate). No `pull_request`. Verify the SHA's CI + image-build success, then `sudo /usr/local/sbin/deploy-staging.sh "$SHA"`.
- [ ] **Root-owned `deploy-staging.sh` wrapper.** Hardcodes the staging overrides (`CRM_USER=staging`, `CRM_HOME=/var/lib/staging`) and `exec`s `deploy-artifact.sh "$SHA"`. Overrides live in the wrapper, NEVER the workflow (sudo resets env). Sudoers entry: single line to the wrapper, no `SETENV`/`env_keep`.
- [ ] **Verify GHCR package visibility (or provision pull auth).** `build-images.yml` creates the packages *private* on first run ("flip to public once"); `deploy-artifact.sh` does an unauthenticated `podman pull`. Confirm `personalcrm-backend`/`-frontend` are public, else give the rootless `staging` tenant a `podman login ghcr.io` credential (placed + refreshed).
- [ ] **First-run bootstrap (cold stack).** `deploy-artifact.sh` reads the *live* units' rollback anchor and `podman inspect`s a running container — both fail on a never-started stack. Define the one-time bring-up: place units → `systemctl --user daemon-reload` + start under the `staging` user (linger already on) → fresh volume runs `init-db.sql`, backend migrate-on-boot builds schema → initial `crm-admin --reset-and-seed` → THEN the first automated deploy (special, like prod's `:latest`→digest first deploy).
- [ ] **`staging-reset.sh` rootless-tenant variant.** Seed logic is built; the wrapper needs a path that stops/starts via `sudo -u staging … systemctl --user`, sources the deployed staging `.env`, and seeds via `podman exec crm-backend /usr/local/bin/crm-admin --reset-and-seed`. Stay programmatically callable by #380.

**Host provisioning (on stovepipes):**

- [ ] **Staging env + host layout.** Replicate the prod host layout the verbatim units expect: `/srv/personalcrm/.env` (staging contents — `CRM_ENV=staging` [unset = treated as prod = Todoist guard OFF, so set it explicitly], staging `DATABASE_URL` with db `personal_crm`, a freshly-minted staging-only `CRM_API_KEY`, no sync creds; readable by uid 1995) + `/srv/personalcrm/infra/init-db.sql`. Kept out of git. (`setup-vps.sh` explicitly defers this to spec C.)
- [ ] **Native Caddy + staging Caddyfile (hard prerequisite).** `deploy-artifact.sh`'s health-gate step 3 curls `http://localhost:80/api/v1/contacts` *through native Caddy with key injection* — so native Caddy + a staging Caddyfile (`/api`→`:8080`, `/`→`:3001`, `header_up X-API-Key {env.CRM_API_KEY}` from a staging `/etc/caddy/crm.env`, plus the prod `@daemon` X-Mac-Host-ID bypass) + the key must be installed BEFORE the first deploy, or every deploy fails the gate.
- [ ] **Runner install on stovepipes.** Reproducible runbook mirroring `infra/runner-installation-runbook.md`: the `gha-runner` system user (≠ tenant, ≠ root), agent labeled `staging` (`runs-on: [self-hosted, staging]`), the `deploy-staging.sh` wrapper + co-installed (possibly parameterized) `backup-db.sh`/`restore-db.sh` (root-owned, runner-immutable), the single sudoers entry, the ntfy env file (degrade-open is acceptable on staging, unlike prod's mandatory gate).
- [ ] **Phone access via Tailscale Serve.** Tailscale Serve → native Caddy `:80` on the box, own MagicDNS name; confirm operator-device → VPS:443 under today's default-allow tailnet (revisit under B's deferred Phase-2 ACL-as-code).
- [ ] **Deferred sandbox↔staging observe path.** Spec C owns the observe Podman network + nftables rule-3 ALLOW + read-only PG role — separable from getting staging running (do it after). NOTE: the operator intends the sandbox to be **on-tailnet** (not the current off-tailnet ProxyJump posture); that is a spec-B-posture revisit (rule 2, the `--accept-dns=false`/DNS expectation, the reach pattern) that overlaps this observe path, so fold the two together rather than treating them as separate streams.

**Non-issue, noted to prevent re-flagging:** the backend unit runs golang-migrate on boot AND `deploy-artifact.sh` migrates explicitly; prod already does both (post-migrate the on-boot run is a no-op; on rollback the restored DB + old image are up-to-date), so there is no staging-specific conflict here.

## Existing primitives to build on

- **`CRM_ENV`** (already gates `/seed/*` routes) — reuse for the Todoist guard and the seed gate.
- Spec D's `crm-admin --seed` for the dataset; spec A's image artifact + Quadlet deploy + lockdown + Caddy-edge key injection for the `develop` deploy.
- The prod Quadlet units (A) as the staging stack template — staging is the same units with a staging env file.
- `infra/` (systemd units today; Quadlet `.container` units after A's containerization) as the stack source of truth.

## Dependencies

- **Needs B** (the VPS, its Podman runtime, the tailnet join, host isolation).
- **Needs D** (synthetic seed) to be useful; pairs with it.
- **Reuses A** (image artifact, Quadlet deploy mechanism + lockdown, Caddy-edge key injection, `develop` branch).
