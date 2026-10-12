# Deploy Automation + Staging Environment — Umbrella Design

**Date:** 2026-06-07
**Status:** Umbrella design (architecture-level). Detailed sub-specs (A/B/C/D) are skeletons to be filled out in their own brainstorms.
**Author:** spengrah (brainstormed with Claude)

## Problem

Two related pains, both rooted in the same gap between "merged to main" and "verified in a realistic environment":

1. **Deployment is manual.** After a PR merges to main, the operator runs `make deploy-all`, which builds locally and rsyncs artifacts to the Pi (and builds/installs the Mac daemon locally). Discipline-dependent (deploy only from main), and a chore.
2. **The local/test environment is thin.** The test DB is intentionally not wired to the real integrations, so it lacks realistic data. As a result, even UI changes often can only be meaningfully verified by the operator in **prod** — there is no realistic, safe place to look at changes before they ship.

This is a single-user, privacy-focused, local-first app (Pi backend + Mac daemon, accessed over Tailscale). It is also explicitly used as a **teaching ground for good engineering practices**, which biases decisions toward the "real CI/CD" answer even where a cheaper hack exists.

## Goal

- **Automate deployment**: merging to main deploys the new build to the Pi (priority) and Mac (nice-to-have) with no manual script run.
- **Stand up a staging environment**: a realistic, safe place to verify changes (especially UI/UX) before prod, that also serves as the target for the agentic UX QA harness (issue #380, Piece 4).
- Keep credentials and attack surface aligned with the app's privacy-first ethos.

## Settled decisions (the durable core)

These were decided during brainstorming and are the load-bearing choices the sub-specs build on.

### Deploy mechanism: self-hosted GitHub Actions runners on the Pi and the Mac

Chosen over (a) cloud runner + Tailscale and (b) a Pi-side pull-loop.

The decisive property: a self-hosted runner is **outbound-only**. The runner agent on the Pi opens a long-poll HTTPS connection *to* GitHub and pulls jobs down it; GitHub never dials the Pi. Therefore:

- **No "Pi-reaching credential" exists off-box.** The cloud+Tailscale option would require storing a Tailscale ephemeral auth key / SSH key in GitHub secrets — a credential to *join the private network*, held by a third party, whose mere possession grants access and whose leak (GitHub breach, malicious CI dependency) compromises the tailnet. The self-hosted runner instead holds a credential that authenticates *the Pi to GitHub*; its leak lets an attacker impersonate a runner (and steal jobs/secrets passed to it), but **cannot reach into the Pi**.
- **It adds no new way in.** GitHub-dispatched jobs *can* run code on the Pi, but only via jobs the Pi voluntarily pulls, gated by which triggers are allowed and who can fire them — which collapses to "people with repo write access," the same trust boundary the operator already lives with. A Pi-reaching credential, by contrast, adds a standing door an attacker could knock on.
- **It is the only option that can automate the Mac at all** (codesigning must happen on the physical Mac), so it unifies Pi and Mac under one mechanism.

Idle cost of the long-poll is ~100 MB RAM for the .NET listener and statistically-zero CPU/energy/bandwidth (the process is blocked on I/O between ~50s polls). Dispatch latency from "GitHub dispatches" to "first step on the Pi" is seconds (sub-second long-poll delivery + a few seconds of runner-side prep), and is dwarfed by CI-gate + build time. Self-hosted actually beats cloud-hosted on pickup latency (no VM to provision).

### Security model for a public repo + self-hosted runner

The repo stays **public**. The danger of public + self-hosted is exactly one thing — a fork PR running attacker code on the runner — and the lockdown is making the runner unreachable from PRs. Layered, strongest first:

1. **Deploy job triggers on `push: branches: [main]` only** — never `pull_request` / `pull_request_target`. Push-to-main requires write access; forks raise `pull_request`, which the deploy workflow does not listen to. (Load-bearing control.)
2. **Settings → Actions → Fork pull request workflows → "Require approval for ALL outside collaborators"** (public-repo default is first-time-only). Backstop against a PR that adds a malicious self-hosted-targeting workflow.
3. **Split compute by trust:** CI (tests/lint) runs on **cloud** runners for push and PRs (forks tested safely off-box); the **self-hosted** runner only ever runs the deploy job, only on push to main.
4. **Green-CI gate:** deploy depends on CI passing on the merge commit (single workflow with `needs:`, or `workflow_run`). Re-running tests on the merge commit is correct — it is what ships.
5. **GitHub `production` Environment** with deployment-branch rule = `main` only (optionally a required reviewer for a one-click approval per deploy). Defense-in-depth against a misconfigured trigger.
6. **Harden the runner host:** dedicated unprivileged runner user (not root, not `crm`); a *narrow* sudoers entry (only `systemctl restart personalcrm.target` + service-file copy); third-party actions pinned to full commit SHAs.

### Topology: Pi = prod + runner; VPS = everything non-prod

The deploy runner **lives on the Pi**, not on the VPS — so the VPS never holds prod-deploy capability, which dissolves most "should the VPS share roles?" tension. A VPS is being introduced anyway (for always-on agent sessions and the QA harness); it becomes the consolidated **non-prod** box.

| Role | Trust | Host | Why |
|------|-------|------|-----|
| Prod + **deploy runner** | Highest | **Pi** | Self-deploys; no Pi-reaching credential anywhere; never co-located with untrusted work |
| **Staging instance** + **agentic QA harness** | Medium (own code, fake data) | **VPS** | QA needs a running instance with realistic data to tour; staging *is* that instance |
| **Dev / agent sandbox** (long agent sessions) | **Lowest** (runs arbitrary AI-generated code) | **VPS**, contained | Untrusted by nature; isolate within the VPS (container) |

**Tailnet ACLs are the real isolation boundary.** Everything is on the tailnet, so ACLs (not box-counting) enforce isolation: the VPS — especially the agent sandbox — must **not** reach the Pi's SSH/admin ports. One VPS is economical for a single user; split the agent sandbox onto its own box only if resource contention or belt-and-suspenders isolation later justify it.

### Runtime & parity model: fully containerized, image-as-artifact, three tiers

Reconsidered greenfield and concluded the runtime should be **fully containerized with the container image as the deploy artifact** — because the *same `arm64` image bytes* running on the Pi (prod) and the VPS (staging) is the truest possible parity, which is the whole reason staging exists. Supervisor = **Podman Quadlets** (containers declared as systemd units: one supervisor, journald, rootless/no-daemon — fits the already-systemd box and the lockdown ethos; Compose rejected as a second supervisor + root daemon). **Caddy stays native** (binds `:80`, avoids the rootless privileged-port wrinkle). Today's prod is a systemd + **Docker-Postgres** hybrid, so the only stateful component is already containerized — the cutover is one-time and low-risk (Postgres volume doesn't move), and is cheapest done *before* A is built (A's artifact format is foundational).

This yields a **three-tier parity model**, each tier matching the right neighbor for its job:

| Tier | Members | Shape | Parity target |
|---|---|---|---|
| **dev** | Mac (local) + VPS sandbox | **native / flat**, toolchain pinned to CI; `make dev` native (hot-reload) | **CI** (correctness gate) |
| **runtime** | staging (VPS) + prod (Pi) | **containerized** `arm64` images under Podman Quadlets, Caddy native | **prod** (behavior gate) |

- **Dev tier matches CI, not prod.** A dev box's job is writing-and-testing code; correctness is gated by *tests matching CI*. So dev boxes need the **flat CI toolchain** (Go/bun/node/sqlc/golangci-lint, a Postgres-16 *endpoint*, a Chromium *binary*) — **no nesting**, no reproduction of the prod runtime. The Mac can't run Quadlets anyway (Linux/systemd-only), and shouldn't — that's the right outcome, not a gap.
- **Runtime tier matches prod by construction** — staging runs the identical image set as prod.
- **Postgres packaging is cosmetic.** In-process (dev) vs in-container (runtime) behaves identically; what matters is the **major version (16)** + config. Pin it across dev/CI/prod and the difference is immaterial.
- **Dev parity = toolchain-version parity, not runtime parity.** The Mac and the sandbox should provision from the **same pinned source** (`make setup` + `go.mod` + a tools manifest) so neither drifts from the other or from CI. That shared manifest is the actual "keep dev close enough" lever — codified once (by the sandbox image), applied natively by the Mac.

### Staging: always-on on the VPS, fed by `develop`, reset-to-seed

- **Always-on** (not ephemeral): directly cures the "can only verify in prod" friction — open `develop` on the phone over Tailscale, no spin-up ceremony. It is also the canonical GitOps environment-per-branch pattern (`develop` → staging, `main` → prod) and gives the QA harness a standing target.
- **`make staging-reset`** reloads a known seed dataset on demand — neutralizes the one real downside of always-on (data drift) and gives the QA harness deterministic state.

### Staging data + sync sources

- **Todoist is the only outbound writer**; every other source (Gmail, GCal, GChat, Telegram, Mac Contacts) is read-only ingestion. So the safety story splits by source type:
  - **Read sources:** the risk is *real PII landing on the wider-surface VPS*. Fix = **do not configure their credentials in staging** (no creds → providers never initialize → nothing real ingested). This is also *why* staging needs synthetic data.
  - **Todoist:** the risk is *mutating the real task list*. Baseline fix = no creds in staging; **defense-in-depth** = an environment guard (reuse existing **`CRM_ENV`**) so the Todoist provider refuses outbound writes when not `production`, surviving even an accidentally-copied prod `.env`.
- **Data = synthetic seed**, not a sanitized prod snapshot (free-text notes can't be reliably scrubbed; PII-on-VPS regression; teaching anti-pattern) and not always-on live test accounts (those are reserved **on-demand** for actively developing a specific integration).

## Decomposition and sequencing

Each piece gets its own brainstorm → spec → plan → implement cycle. Specs live alongside this file.

- **A0 — Containerization cutover** · `2026-06-07-containerization-cutover-design.md` · *Foundational precursor to A.* One-time move of prod + `infra/` from systemd + Docker-Postgres hybrid → fully containerized under Podman Quadlets (Caddy native), with the container image as the deploy artifact. Produces the images + Quadlet units that A automates and C reuses.
- **A — Deploy automation** · `2026-06-07-deploy-automation-design.md` · *Operator's stated priority.* Self-hosted runners (Pi + Mac), `develop`/`main`, reusable workflows, public-repo lockdown, image build→GHCR + `podman pull`/tag-swap deploy. Depends on A0.
- **B — VPS + tailnet isolation** · `2026-06-07-vps-and-tailnet-isolation-design.md` · Provision the VPS (Podman runtime), tailnet isolation, off-tailnet rootless agent sandbox. Prerequisite for C; independent of the A track.
- **C — Staging environment** · `2026-06-07-staging-environment-design.md` · Always-on staging stack on the VPS (same images as prod), `develop`→staging deploy, `CRM_ENV` Todoist guard, reset-to-seed, phone access via Tailscale.
- **D — Synthetic seed generator** · `2026-06-07-synthetic-seed-generator-design.md` · Library-first synthetic-data toolkit + `crm-admin --seed`; replay through the real sync pipeline; reusable for staging, tests, *and* local `make dev`; institutionalized via project rules (new features seed; new tests use it) + a suite migration onto the factories; supports the #380 QA harness.

**Dependency order — two parallel tracks that converge:**

```
A0 containerize prod ─→ A automate deploy ┐
B provision VPS ──────────────────────────┼─→ C staging
D seed (standalone, build early) ─────────┘
```

- **Track 1 (runtime/deploy):** A0 → A.
- **Track 2 (host):** B, independent of the A track; only needs to exist before C.
- **D floats:** standalone, no infra dependency — build early / in parallel; improves local `make dev` and de-risks C.
- **Converge at C:** staging needs A (image artifact + deploy workflow + Caddy pattern) **and** B (the host) **and** D (data to be useful).
- **Spec order ≠ build order:** all five are now specced; build can open A0, B, and D in parallel. The QA harness (issue #380, Piece 4) consumes C + D.

**Operator's chosen first build: D** (standalone, immediately useful).

## Existing primitives to build on

- `scripts/deploy.sh` (build-locally + rsync-to-Pi + restart + healthcheck), `scripts/deploy-mac-daemon.sh`, `scripts/deploy-all.sh` (per-target change detection via `.deploy-state/{pi,mac}.sha`). A will refactor `deploy.sh` into a native-build path.
- Systemd units: `personalcrm.target` and `personalcrm-{backend,frontend,database}` services (Pi); `xyz.spengrah.crm-mac` launchd (Mac).
- `crm-admin` operator CLI (already deployed to the Pi) — natural home for `--seed`.
- **`CRM_ENV`** already exists and already gates test-only `/seed/*` routes (`/seed/contacts`, `/seed/external-contacts`, `/seed/meeting-notes`, …) — reuse for the Todoist guard and as the staging seed gate.
- `frontend/tests/e2e/helpers/test-api.ts` (`TestAPI` seed/read helpers) — seed-data prior art for D.
- CI workflows: `.github/workflows/{ci.yml,backend-slow-tests.yml,codex-review.yml,claude.yml}`.

## Non-Goals

- Multi-user / multi-tenant anything. Single user throughout.
- Exposing a public inbound endpoint from the Pi or Mac. The whole point of the self-hosted-runner choice is to avoid that.
- Sanitized prod data in staging (rejected; see above).
- Pixel-perfect visual regression (that is the QA harness's domain, #380).

## Open cross-cutting questions

Mostly resolved while filling out the sub-specs:

- ✅ **Branching / PR flow** → A introduces `develop` (`develop`→staging, `main`→prod); feature → `develop` → promote to `main`. (A/C)
- ✅ **Build capacity** → build is **in the cloud as `arm64` container images** pushed to GHCR; the Pi only `podman pull`s. No native Pi build, no hybrid needed. (A)
- ✅ **Migration safety + rollback** → 1b-offline explicit pre-cutover migrate + automated snapshot-restore, no human gate; staging is the migration canary. (A/C)
- ✅ **Frontend build-time env** → key injected **server-side at the native Caddy edge**; never baked into the image or shipped to the browser; `NEXT_PUBLIC_API_URL` stays empty (same-origin). (A)

Remaining:

- Promotion cadence for a solo dev (`develop`→`main` fast-forward vs PR) — lightweight; settle during A's workflow plumbing.
- One-time **prod cutover** sequencing (systemd + Docker-Postgres hybrid → fully containerized under Podman Quadlets) — schedule it before/with A's first implementation, since A's artifact format depends on it.
