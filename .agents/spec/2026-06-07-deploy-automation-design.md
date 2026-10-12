# A — Deploy Automation (self-hosted runner) — Design

**Date:** 2026-06-07 (firmed up 2026-06-11, post-A0)
**Status:** Design firmed up — ready for implementation planning.
**Author:** spengrah (brainstormed with Claude)
**Parent:** `2026-06-07-deploy-and-staging-overview-design.md`
**Precursor:** `2026-06-07-containerization-cutover-design.md` (A0 — landed; prod now runs rootless Podman Quadlets)

## Scope

Promoting `develop` to `main` auto-deploys the new build to the Pi (prod), via a **self-hosted GitHub Actions runner**, gated on green CI, with the repo kept **public** and the runner unreachable from PRs. Replaces the manual native `deploy.sh` rsync path (now retired post-A0). The Mac-daemon runner and the `develop`→staging deploy *target* are explicitly deferred (see "Out of scope"); A lands the `develop` branch + image build so those drop in later with no rework.

## What A0 already delivered (so A does not rebuild it)

A0 landed before this design was firmed up and pre-built a large chunk of what the original skeleton listed as open:

- **Cloud image build is done.** `.github/workflows/build-images.yml` cross-compiles arm64 (no QEMU), bakes `crm-admin` into the backend image, builds the key-less Next standalone frontend, and pushes both to GHCR tagged `:<sha>` + `:latest`. Currently triggers on push to `main`.
- **Caddy edge key-injection is live.** `infra/caddy/Caddyfile` injects `X-API-Key` on `/api/*` (with a `@daemon` matcher that skips injection for Mac-daemon requests). Version-controlled, deployed to prod.
- **Podman Quadlet runtime is live.** `infra/quadlet/*` (network + volume + 3 `.container` units); `scripts/backup-db.sh` already targets the rootless Podman `personalcrm-db` volume.

Net effect on A: the "build job" is essentially shipped; A is mostly the **deploy half** plus the branch/promotion model, migration tooling, rollback, and runner lockdown.

## Decisions locked

### Branching & promotion model

Branch-per-environment, with the one principle worth taking from big-SaaS practice — **promote the immutable artifact, never rebuild**:

- **`develop`** is the PR target and integration branch. CI + image build run here. **No deploy.**
- **`main`** is the release/deploy branch. It is **fast-forward-only from `develop`**. Nothing lands on `main` via PR.
- **Promote** by fast-forwarding `main` to `develop`'s HEAD (`git push origin develop:main`), done by an agent when ship conditions are met. Because the SHA is identical, the `:<sha>` image is already built and already CI-green — the prod deploy just pulls it. No rebuild, no build/deploy race.

Rationale: trunk + artifact-promotion pipelines (environment approval matrices, Argo/Spinnaker) are built for many-engineer multi-tenant SaaS and are overkill for a single-user self-hosted app. Branch-per-environment is the pragmatic sweet spot and sets up `develop`→staging cleanly for when B lands. The fast-forward-same-SHA trick keeps the build-once/promote-artifact property without the pipeline machinery. A release PR was rejected: as sole reviewer you already saw each PR land on `develop`, so its only benefit (accumulated-diff visibility) is redundant.

**Branch protection rules:**
- `develop`: require PR + green CI to merge; linear history (squash); direct pushes blocked.
- `main`: fast-forward-from-`develop`-only; `production` GitHub Environment with a `main`-only branch rule gating the deploy job.

### CI / build / deploy topology

Three single-job workflows. No reusable `workflow_call` refactor (see below).

| Workflow | Triggers on | Runs on | Does |
|---|---|---|---|
| `ci.yml` *(exists)* | PR → `develop`; push → `develop` | GitHub cloud | The test gate. Unchanged except adding `develop` to triggers. |
| `build-images.yml` *(exists)* | push → `develop` | GitHub cloud | Builds + pushes `:<sha>` arm64 images to GHCR. Retarget `main`→`develop`. |
| `deploy-prod.yml` *(new)* | push → `main` | self-hosted `pi` runner | Pulls `:<sha>`, migrates, restarts Quadlets, health-gates, rolls back on failure. |

**Green-CI gate** (CI does not re-run on the `main` fast-forward, since it's the same SHA): `deploy-prod.yml`'s first step queries the `ci.yml` workflow conclusion for `$GITHUB_SHA` via `gh api` and **aborts unless it is `success`**. Combined with the `production` Environment's `main`-only branch rule, there are two independent guards — the artifact can't deploy unless that exact commit passed CI, and the job can't run from any branch but `main`.

**Why no reusable `workflow_call` refactor** (the original skeleton wanted one): that was premised on the deploy workflow *re-running* tests. Gating on CI's *result* for the SHA means we never re-run tests — nothing to share. One less moving part.

### Notifications

**ntfy.** `deploy-artifact.sh` posts distinct titled pushes per outcome branch: `deploy ok`, `migrate failed — restored`, `rolled back`, and a max-priority `ROLLBACK FAILED — prod degraded`. Topic/URL read from a Pi-local env file, never committed. Required, not optional — prod deploys are unattended, and the channel must distinguish "rolled back cleanly, prod fine" from "rollback failed, prod degraded." (GitHub's built-in workflow-failure email is too coarse for that distinction.)

### Runner security posture

- **Dedicated `gha-runner` system user** (not `crm`, not root, no login shell). The Actions runner agent runs as a system systemd service under `gha-runner`, registered with label `pi`.
- **Sudoers = exactly one allowlisted entry:** `gha-runner` may run *only* `/usr/local/sbin/deploy-artifact.sh <sha>` as root, nothing else. The script is root-owned (runner can't edit it), repo-reviewed, and does the `sudo -u crm` hops to crm's rootless podman/systemctl plus the genuine root ops (volume copy, `systemctl restart caddy`) internally. The runner's entire privileged capability is "invoke one immutable, reviewed script." Chosen over granular per-command allowlisting because that is brittle (every script change risks a sudoers update) and its arg-matching is its own foot-gun; the single-script entry is both narrower in practice and easier to audit. Runner identity stays separate from workload (`crm`) identity.

### Public-repo lockdown

- `deploy-prod.yml` triggers on `push: main` only — never `pull_request`, so fork code never reaches the runner.
- `production` GitHub Environment, branch rule restricted to `main`.
- Repo setting: require approval to run workflows for *all* outside collaborators.
- SHA-pin every `uses:` action in the new workflow.
- `concurrency: deploy-pi` so two promotions can't overlap.

### Frontend API key

A0 already injects `X-API-Key` at the Caddy edge and builds the frontend key-less. The remaining work is **source cleanup**: the browser still *sends* an empty `X-API-Key` header (dead code). Delete the send from `frontend/src/lib/api-client.ts` (2 call sites), `frontend/src/app/settings/page.tsx` (2), `frontend/src/app/test-api/page.tsx` (1). `NEXT_PUBLIC_API_URL` stays empty (same-origin) for both environments. Service clients (Mac daemon, future MCP) keep hitting `:8080` directly with their own key.

## Deploy / rollback mechanics

**Quadlet tag pinning (a needed change):** A0's units pin `Image=...:latest`. For deterministic rollback, `deploy-artifact.sh` rewrites the `Image=` line to the specific `:<sha>`, `daemon-reload`s, and restarts. The rollback target is whatever `:<sha>` the unit currently holds — read it *before* rewriting. The unit file always shows exactly what's running.

**`deploy-artifact.sh`** (runs on the Pi as root via the one sudoers entry; replaces the retired rsync `deploy.sh`):

```
0. rollback_sha = current Image= tag in the backend Quadlet unit
1. podman pull :<new-sha>            (fail fast if the image is missing)
2. crm-admin --migrate-check
   ├─ up-to-date → rewrite Image=→new, daemon-reload, restart, health-gate
   │                FAIL → rewrite Image=→rollback_sha, restart, ntfy "rolled back", exit 1
   │                       (no DB touched → no restore; fast, zero DB downtime)
   └─ pending    → backup-db.sh (snapshot, services stopped)
                   crm-admin --migrate
                     FAIL → restore-db.sh, restart rollback_sha, ntfy "migrate failed — restored", exit 1
                   rewrite Image=→new, daemon-reload, start, health-gate
                     FAIL → restore-db.sh + rollback_sha + restart, ntfy "rolled back", exit 1
                     PASS → retain snapshot as the recovery point, ntfy "deploy ok", exit 0
```

**Health-gate contents:** `/health` reports `database: healthy` (already the Quadlet boot gate) **+** frontend returns 200 **+** one authenticated read through Caddy (`GET localhost:80/api/v1/contacts` → 200), exercising the whole edge→key-injection→backend→DB read path. Reads only — never mutates prod.

**Why no expand/contract migration requirement** (carried from the original): 1b-offline migration (old code stopped before migrating) has no version-skew window, and auto-restore rolls back code *and* schema together to a consistent prior state. Backward-compatible migrations are a free nicety when convenient, never a requirement. Recoverability comes from the snapshot, not from authoring care.

**Residual risks (accepted):**
- Auto-rollback covers **deploy-time failures only** (migration errors + immediate health-check). A latent bug found hours later → **fix-forward** (snapshot already rotated; can't restore without losing accumulated data).
- Physical snapshot/restore downtime **scales with DB size** (Gmail-correspondence growth) — revisit logical dump or WAL-based PITR if it becomes painful.
- Down-migrations (~60 `.down.sql` exist) are a **manual last resort only** — never auto-run (data-loss risk).

## Components to build

- [ ] **`develop` branch + protection rules** (GitHub config): create `develop` from `main`; PR + green-CI rule on `develop`; fast-forward-only + `production`-Environment `main`-only rule on `main`; outside-collaborator workflow-approval setting.
- [ ] **Retarget `ci.yml` + `build-images.yml`** triggers from `main` to `develop` (CI also keeps PR triggers).
- [ ] **`deploy-prod.yml`** (new): `push: main` → self-hosted `pi` runner → CI-conclusion gate → invoke `deploy-artifact.sh $GITHUB_SHA` via the one sudoers entry. `concurrency: deploy-pi`, `production` environment, SHA-pinned actions.
- [ ] **`crm-admin --migrate` + `--migrate-check`**: `--migrate` applies app + River migrations (reuses `db.RunMigrations`, idempotent); `--migrate-check` reports pending without mutating (exit 0 = up-to-date, distinct exit code = pending) so the script snapshots *before* touching the DB. Backend startup keeps running migrations as a no-op backstop.
- [ ] **`scripts/deploy-artifact.sh`**: the orchestration above. Installed root-owned to `/usr/local/sbin/` by the runner runbook. Validates its SHA arg (40-hex). Rewrites Quadlet `Image=`, drives migrate/snapshot/restore/health-gate/ntfy.
- [ ] **`scripts/restore-db.sh`**: counterpart to `backup-db.sh` — stop crm services → copy the `.bak` volume back over the live volume → restart. Snapshot retained until the next successful deploy; never deleted on a failed restore.
- [ ] **ntfy integration** in `deploy-artifact.sh`: distinct titled/prioritized pushes per outcome; topic/URL from a Pi-local env file.
- [ ] **Frontend source cleanup**: delete the `X-API-Key` send from the 5 call sites above.
- [ ] **Retire the native deploy path**: remove `scripts/deploy.sh`, `scripts/deploy-all.sh`, `.deploy-state/`, and their `make deploy`/`deploy-pi`/`deploy-all` targets. Add `make promote` (the fast-forward push). `deploy-mac-daemon.sh` + `make deploy-mac` stay untouched.
- [ ] **Runner installation runbook** (`infra/` doc): one-time `gha-runner` user creation, runner agent + systemd service, label `pi`, the single sudoers entry, installing `deploy-artifact.sh` root-owned, and the Pi-local ntfy env file.

## Out of scope for A (deferred)

- **Mac-daemon runner** — fast-follow after the Pi path is proven. `deploy-mac-daemon.sh` stays as the manual path until then.
- **`develop`→staging deploy target** — needs B's VPS. A lands only the `develop` branch + image build; the staging deploy job is added when B/C exist (C layers the staging stack/data onto it). The `deploy-artifact.sh` is written environment-agnostic so staging reuses it.
- **GHCR image retention/pruning** — `develop` accumulates a per-SHA image each push. Note as a minor follow-up (prune untagged/old images); not blocking.

## Existing code to build on

- `.github/workflows/build-images.yml` (cloud build → GHCR, retarget to `develop`), `.github/workflows/ci.yml` (add `develop` triggers).
- `infra/quadlet/*` (Quadlet units — `Image=` line rewritten per deploy), `infra/caddy/Caddyfile` (edge key injection, already live).
- `scripts/backup-db.sh` (A0-current; targets the Podman `personalcrm-db` volume — `restore-db.sh` is its mirror).
- `backend/cmd/crm-admin/main.go` (subcommand harness — add `--migrate`/`--migrate-check`); `db.RunMigrations` (`backend/cmd/crm-api/main.go:258`, the startup backstop).
- Frontend key-send call sites: `frontend/src/lib/api-client.ts`, `frontend/src/app/settings/page.tsx`, `frontend/src/app/test-api/page.tsx`.
- To retire: `scripts/deploy.sh`, `scripts/deploy-all.sh`, `.deploy-state/`.

## Dependencies

A0 (landed). No hard dependency on B/C for the **main→prod** path — standalone and first in sequence. The `develop` branch + build that A creates are only *useful* for staging once B (VPS) and C (staging stack) exist, but they land now with no rework cost.
