# C — Staging Environment — Design

**Date:** 2026-06-07
**Status:** Design — filled out this session. Implementation-detail threads remain.
**Author:** spengrah (brainstormed with Claude)
**Parent:** `2026-06-07-deploy-and-staging-overview-design.md`

## Scope

An **always-on staging instance** of the CRM on the VPS, deployed from a **`develop`** branch, running the **identical container image set as prod** (Podman Quadlets), with all read sync sources disabled, Todoist outbound writes structurally guarded, reachable from the operator's phone over Tailscale, resettable to a known seed dataset on demand, and serving as the target for the agentic UX QA harness (#380). Cures the "can only verify UI changes in prod" pain.

## Architecture: staging *is* prod, run from the same image

The big simplification from this session: once the deploy artifact is a **container image** (decided in A) and the runtime is **Podman Quadlets** (decided in A/overview), the earlier "systemd-parity vs Compose" framing for staging **dissolves**. Staging is not a *similar* stack — it is the **identical `arm64` image set as prod**, run under the same kind of Quadlet units, on the VPS. Parity is by construction, which is the whole point: staging is a faithful canary for the prod deploy.

Everything that differs between staging and prod is **configuration, not composition**:

- env file (`CRM_ENV=staging`, no sync creds, own DB URL + API key)
- published ports + DB name (`personal_crm_staging`)
- which branch deploys to it (`develop` vs `main`)
- its own Tailscale Serve / MagicDNS name and staging Caddyfile upstreams

This is staging's place in the **three-tier parity model** (see overview): **dev tier** (Mac + VPS sandbox; native/flat toolchain pinned to CI) → **staging** (prod-parity) → **prod**. Staging's parity contract is to match *prod*; the sandbox's is to match *CI*. Each environment matches the right neighbor for its job.

## Topology on the VPS

Staging runs as Quadlet-supervised containers; **Caddy stays native** on the VPS (mirrors prod, binds `:80` without the rootless-port wrinkle). Prod and staging Caddy live on different hosts, so there is no port conflict between them; distinct *published* ports on the VPS are for operator clarity, not necessity.

| Component | Staging (VPS) | Prod (Pi) | Notes |
|---|---|---|---|
| backend | container, published `:8081` | container, published `:8080` | same image |
| frontend | container, published `:3002` | container, published `:3001` | same image |
| postgres | container, db `personal_crm_staging`, own volume | container, db `personal_crm` | same image, isolated data |
| caddy | **native** `:80`, staging Caddyfile | **native** `:80`, prod Caddyfile | `/api`→backend, `/`→frontend, edge key injection |
| edge | Tailscale Serve → `:80` | Tailscale Serve → `:80` | own MagicDNS name |
| sandbox | standalone rootless Podman Quadlet (off-tailnet) | — | B; isolated from staging |

## Decisions locked (from brainstorm)

- **Same image, same Quadlets, on the VPS** (not ephemeral, not on the Pi). Co-locating on the Pi was rejected (resource contention + shared blast radius with prod). The staging units are the prod Quadlet units with a staging env file.
- **Caddy native on the VPS**, with a **staging-specific Caddyfile**: `/api` → backend container, `/` → frontend container, plus `header_up X-API-Key {env.API_KEY}` edge injection (A's pattern) so the **staging key never reaches the browser** either. `NEXT_PUBLIC_API_URL` stays empty (same-origin), as in prod.
- **Runtime tier of the three-tier parity model.** Staging's contract is prod-parity; reference the overview model rather than re-deriving it.
- **All read sources OFF** in staging — no credentials for Gmail / GCal / GChat / Telegram / Mac Contacts → providers never initialize → no real PII ingested onto the VPS. Realism comes from the synthetic seed (spec D), not live sync.
- **Todoist guarded by `CRM_ENV`:** hard **refuse + warn-log** at the **single Todoist HTTP-client chokepoint** (covers all ~6 task create/update paths with one guard, not six) when `CRM_ENV != production`. Defense-in-depth beyond simply omitting creds — survives an accidentally-copied prod `.env`. (Todoist is the only outbound writer; all other sources are read-only ingest.)
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

## Open threads / TODO (fill out here)

- [ ] **Staging Quadlet units.** The `.container`/`.volume`/`.network` units for staging (parameterized by the staging env file); how `deploy-artifact.sh` swaps them (`podman pull <tag>` → `systemctl --user restart` the generated units). Confirm they're the prod units with a different `EnvironmentFile`.
- [ ] **`CRM_ENV` guard implementation.** Exact chokepoint in the Todoist HTTP client; the refuse error surface (return vs no-op + log); test coverage for "staging never writes to real Todoist"; audit GChat / Gmail / GCal / Telegram to **confirm** Todoist is the only outbound writer.
- [ ] **Staging `.env` management.** `/srv/personalcrm-staging/.env` (`CRM_ENV=staging`, no sync creds, own DB URL + staging API key); provisioned by B's `setup-vps.sh`, kept out of git; Caddy env wiring for `API_KEY` (scoped env file vs `EnvironmentFile`). Generate a staging-only API key.
- [ ] **`make staging-reset` mechanics.** Truncate-respecting-FK vs drop/migrate/reseed (recall: soft-delete does **not** cascade → a true wipe is a hard reset of the staging DB). Pairs with D's reset design. Must be **programmatically callable** by the QA harness, not just interactive.
- [ ] **QA harness (#380) targeting.** Harness hits staging over localhost; reset-before-tour contract for determinism; scheduling/tour internals owned by #380 (nightly ~3am per B).
- [ ] **Phone access specifics.** Tailscale Serve config on the VPS; confirm operator-device → VPS:443 is allowed under today's default-allow tailnet (revisit when B's deferred full ACL-as-code lands).
- [ ] **Relationship to A's workflows.** Reuse A's deploy workflow with a branch→env mapping (`develop`→staging via the `staging` runner label, `main`→prod via `pi`) vs a separate staging workflow.

## Existing primitives to build on

- **`CRM_ENV`** (already gates `/seed/*` routes) — reuse for the Todoist guard and the seed gate.
- Spec D's `crm-admin --seed` for the dataset; spec A's image artifact + Quadlet deploy + lockdown + Caddy-edge key injection for the `develop` deploy.
- The prod Quadlet units (A) as the staging stack template — staging is the same units with a staging env file.
- `infra/` (systemd units today; Quadlet `.container` units after A's containerization) as the stack source of truth.

## Dependencies

- **Needs B** (the VPS, its Podman runtime, the tailnet join, host isolation).
- **Needs D** (synthetic seed) to be useful; pairs with it.
- **Reuses A** (image artifact, Quadlet deploy mechanism + lockdown, Caddy-edge key injection, `develop` branch).
