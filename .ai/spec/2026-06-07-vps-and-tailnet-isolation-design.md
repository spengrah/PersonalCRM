# B — VPS + Tailnet Isolation — Design

**Date:** 2026-06-07
**Status:** Skeleton — direction locked, details to be filled out in its own brainstorm.
**Author:** spengrah (brainstormed with Claude)
**Parent:** `2026-06-07-deploy-and-staging-overview-design.md`

## Scope

Provision the always-on **VPS** that becomes the consolidated **non-prod** box — hosting (1) an always-on dev/agent sandbox for long-running agent sessions, (2) the agentic UX QA harness (issue #380, Piece 4), and (3) the staging instance (spec C). Establish **tailnet ACLs** as the isolation boundary so non-prod work can never reach prod. The deploy runner does **not** live here (it stays on the Pi).

## Decisions locked (from brainstorm)

- **Host (decided fresh this session):** **Hetzner, ARM, CAX21** (4 vCPU / 8 GB, ~€6.49/mo), resize to **CAX31** (8 vCPU / 16 GB, ~€15.99/mo) on demand (Hetzner hourly billing w/ monthly cap). ARM chosen for **build-artifact parity with the Pi** (single `arm64` build serves prod + staging). 80 GB disk on CAX21; add a volume if repo/caches/staging-DB/snapshots outgrow it. (The #380 spec had *suggested* this; re-decided here on its own merits, not inherited.)
- **ARM parity dividend (cross-spec):** the VPS is `arm64` like the Pi, so the **same `arm64` build artifact deploys to both prod (Pi) and staging (VPS)** — A's build matrix stays single-arch. (Update A's "artifact portability" thread accordingly.)
- **Scope = secured substrate, not tenants.** B owns: the box, the container runtime (**Podman/Quadlets**, per A — one runtime on the box, no Docker daemon), the VPS↔Pi network isolation, the sandbox's *network* isolation, baseline hardening. The QA harness internals belong to #380; the staging stack belongs to C. Deliverable: "a hardened, isolated ARM box running **Podman (Quadlets)** + a network-isolated sandbox, ready for C and #380 to deploy onto."
- **One VPS** consolidates the three non-prod roles (separate containers per #380). Split the sandbox onto its own box only if contention/isolation later justifies it.
- **The deploy runner stays on the Pi**, not the VPS — the VPS never holds prod-deploy capability.
- **Isolation = contained VPS-side egress block now (approach C); full ACL-as-code deferred.** The Pi is a *shared* host (other apps beyond CRM), and a Tailscale default-deny ACL is *tailnet-wide* — flipping it would force re-allowing every legitimate flow across all apps/devices. So B does **not** tag the Pi and does **not** flip the tailnet to default-deny. Instead: tag only the VPS **`tag:crm-ci`** (role-scoped, not a sweeping `nonprod`), and add a **host-firewall egress rule on the VPS dropping traffic to the Pi's (stable) tailnet IP** — local to the new box, zero risk to other apps. The sandbox is off-tailnet regardless, so the highest-risk component can't reach the Pi no matter what. **Full ACL-as-code (default-deny, tailnet-wide, version-controlled) is a worthwhile but separate tailnet-hygiene project** (see open threads); when done it lives in a **PRIVATE** location (not this public repo — it contains tailnet structure + email), synced via Tailscale's GitHub Action.
- **The agent sandbox stays OFF the tailnet entirely** — no tailnet identity; blocked from the host's `tailscale0` (host firewall / container net policy). A compromised agent has no tailnet path to prod or staging. **Off-tailnet ≠ inaccessible:** reach it interactively from the Mac via the host — `ssh vps-host` (over tailnet) → `podman exec -it crm-sandbox tmux` — for both interactive Claude Code sessions and attaching/inspecting long-running runs; wrap in a one-liner alias. (Alternative if direct `ssh sandbox` is ever wanted: give the sandbox its own tag with operator-only-inbound + deny-all-outbound — defer to the full-ACL work.)
- **Sandbox capability requirement (from operator):** must run the **plan-and-ship skill** = both **Claude Code** and **Codex (+ codex-companion 2-account failover)** operational, plus a CI-grade toolchain (Go 1.24, bun, node, sqlc, golangci-lint, test Postgres, Playwright/Chromium) and GitHub auth. The existing dev-sandbox pattern is NOT locked in — redesign freely. plan-and-ship needs only public internet (Anthropic/Codex/GitHub/registries) + a local ephemeral stack — never the tailnet — so this is fully compatible with off-tailnet isolation. Driven by plain `ssh + podman exec + tmux`; **no agent-manager** (ID-Agents tooling removed). The sandbox is the **dev tier** of the three-tier parity model (see overview): its parity target is **CI** (correctness), not prod — flat CI-pinned toolchain, no nesting; the faithful running stack is staging's job.
- **Hardening:** no public inbound (firewall all public ports; reach the box only over the tailnet; SSH bound to the tailnet interface); key-only SSH; non-root service users (mirror `setup-pi.sh`'s `crm` user); unattended security upgrades.
- **Provisioning:** **cloud-init + a `setup-vps.sh`** (mirrors `setup-pi.sh`); Terraform considered and declined for a single long-lived box.
- **Sizing rationale:** 8 GB baseline because staging stack + QA (nightly ~3am) + sandbox (daytime) coexist, temporally staggered. plan-and-ship is the heavy tenant; an overnight multi-PR run can overlap the 3am QA sweep → use per-container cgroup caps + the CAX31 resize lever.

## Resolved this session

- Host/size/cost → **Hetzner CAX21 (8 GB ARM), CAX31 on demand** (decided fresh this session).
- Architecture → **ARM** (parity with Pi; single-arch **container image** artifact for prod + staging).
- VPS container runtime → **Podman (Quadlets)** (per A): staging as supervised Quadlets, the sandbox as a standalone rootless Quadlet, Caddy native. One runtime on the box — no Docker daemon, rootless by default.
- Isolation → **approach C: contained VPS-side egress block now** (drop egress to the Pi's tailnet IP); Pi NOT tagged; VPS tagged `tag:crm-ci`. **Full ACL-as-code deferred** as a separate tailnet-hygiene project.
- Agent sandbox → **off the tailnet**, outbound-internet-only; **standalone rootless Podman Quadlet**; reached interactively via `ssh host → podman exec → tmux`; must run **plan-and-ship** (Claude Code + Codex/codex-companion + flat CI-grade toolchain, no nesting). No agent-manager.
- Provisioning → **cloud-init + `setup-vps.sh`** (Terraform declined).
- Hardening → **no public inbound**, tailnet-only reach.
- ID-Agents tooling → **removed** (done).

## Open threads / TODO (fill out here)

- [ ] **VPS-side egress block (approach C mechanism).** Host-firewall rule dropping VPS→Pi tailnet-IP traffic (nftables/ufw on the `tailscale0` route); confirm the Pi's tailnet IP is pinned. Optional Pi-side inbound-from-VPS drop for symmetry.
- [ ] **Tailnet join + tagging.** Add VPS as a persistent node tagged `tag:crm-ci`; auth-key strategy for provisioning. (Pi stays untagged/user-owned.)
- [ ] **DEFERRED — full ACL-as-code (approach A).** Separate tailnet-hygiene project: inventory all legitimate flows across *all* apps/devices, flip to default-deny, version-control the HuJSON in a PRIVATE repo synced via the Tailscale GH Action. Not part of B.
- [ ] **Sandbox image + provisioning (dev tier, CI-parity).** Build a **CI-grade dev image** run as a **standalone rootless Podman Quadlet** (reuse CI/`make setup` toolchain defs, versions pinned to `go.mod` + CI): Claude Code + Codex + codex-companion + Go/bun/node/sqlc/golangci-lint. **Flat toolchain — no nesting:** bake a **Postgres 16 process** (pinned to prod's major) + **Chromium** directly into the image so tests need only a DB *endpoint* + a Chromium *binary*, never podman-in-podman (the least-reliable layer — avoided, not tamed). Persistent work/cache volume + clean-reset; per-container cgroup caps. Host firewall/container-net rule blocking the sandbox from `tailscale0`. **Verify** nothing in the toolchain hard-requires a **Docker socket** (testcontainers-style) — the repo uses a long-lived `TEST_DATABASE_URL`, so likely none; if any exists, expose Podman's Docker-compat socket. Parity target = **CI**, not prod.
- [ ] **Credential injection (sandbox).** Runtime-mounted secrets for Claude, Codex/codex-companion (broker + 2 accounts), and GitHub — never baked into the image, never transcript-echoed. Define the mechanism.
- [ ] **Baseline hardening specifics.** Firewall rules (drop public inbound; SSH on tailnet iface only), key-only SSH, unattended-upgrades, non-root users; light backup of sandbox/QA *config* (staging is reset-to-seed, low stakes).
- [ ] **Resource/contention policy.** cgroup caps per container; document the CAX31 resize trigger for hot/overnight plan-and-ship overlapping the 3am QA sweep.

## Existing primitives to build on

- The existing `dev-sandbox` skill/pattern — **not locked in**; redesign freely to meet the plan-and-ship requirement.
- CI toolchain definitions + `make setup` — the basis for the sandbox dev image.
- Existing Tailscale setup (Pi + Mac + operator devices already on the tailnet); Tailscale GitHub Action for ACL sync.
- `scripts/setup-pi.sh` — the shape to mirror for `setup-vps.sh`.

## Dependencies

- **Prerequisite for C** (staging needs a host).
- Independent of A.
