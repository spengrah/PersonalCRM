# A0 — Containerization Cutover — Design

**Date:** 2026-06-07 (decisions firmed up 2026-06-10)
**Status:** Design complete — all design decisions locked. Ready for implementation plan.
**Author:** spengrah (brainstormed with Claude)
**Parent:** `2026-06-07-deploy-and-staging-overview-design.md`

## Scope

A **one-time cutover** of prod (and the shared `infra/`) from today's **systemd + Docker-Postgres hybrid** to a **fully containerized** stack under **rootless Podman Quadlets**, with the **container image as the deploy artifact** and **Caddy kept native**. This is the **foundational precursor to A**: A automates a deploy that assumes containers + image artifacts, so the runtime must be containerized first. A0 also produces the image definitions + Quadlet units that **C reuses verbatim for staging**.

Split out as its own sub-project (rather than folded into A) because it is the single prod-touching, slightly-riskier milestone, and its artifact-format decision is foundational to everything in the A track.

## Why now (sequencing)

A's build job *is* "build and push the image"; A's deploy job *is* "pull the image and swap the Quadlet." If A were built against the current tarball/systemd model and the stack containerized later, the deploy path would be built twice. So containerization is cheapest done **before A** — this is the cheapest possible moment (pre-implementation of the whole initiative).

## Current prod topology (verified on the Pi, 2026-06-10)

The edge is layered, and the cutover must preserve it untouched:

```
Tailscale Serve (:443, TLS, tailnet-only)
  └─ Caddy (native, *:80)        /etc/caddy/Caddyfile — root-owned, untracked
        /api/*  → localhost:8080   (backend  — crm-api, native systemd today)
        /*      → localhost:3001   (frontend — next-server, native systemd today)
```

- **Postgres:** Docker container `crm-postgres` (`pgvector/pgvector:pg16`), volume `infra_postgres_data` at `/var/lib/docker/volumes/infra_postgres_data/_data` (root-owned, host UID 999), managed by the `personalcrm-database.service` oneshot wrapping `docker compose up -d`.
- **Backend / Frontend:** native systemd units (`personalcrm-{backend,frontend}.service`), run as user `crm`, `EnvironmentFile=/srv/personalcrm/.env`, ports 8080 / 3001.
- **Caddy:** native `/usr/bin/caddy`, active, binds `*:80`. **This was an open question in the original draft — confirmed live.** Tailscale Serve sits *in front* of Caddy for TLS; it is not a replacement for it.

## Decisions locked

### From the original brainstorm

- **Image-as-artifact.** The deploy artifact is an `arm64` container image. The *same image bytes* run on the Pi (prod) and the VPS (staging) — the truest prod/staging parity, which is the whole reason staging exists. Promote-by-tag; code rollback = "run the previous tag."
- **Podman Quadlets, not Compose.** Containers are declared as systemd units, so the existing model carries over (one supervisor, journald, `personalcrm.target` orchestration) and rootless/no-daemon fits the lockdown ethos. Compose was rejected as a second supervisor + root daemon on an already-systemd box.
- **Caddy stays native** (binds `:80`; avoids the rootless privileged-port wrinkle). Only **backend + frontend + Postgres** are containerized.
- **Build with Docker buildx in CI, run with Podman on the hosts** (OCI-standard). A's build job does the building; A0 defines the Dockerfiles.

### Firmed up 2026-06-10

1. **Rootless Podman + Postgres dump/restore (NOT adopt-in-place).** All three containers run **rootless as `crm`**. Postgres data is migrated by **`pg_dump` → `pg_restore` into a fresh named Podman volume** under `crm`'s user storage — *not* by adopting the existing Docker volume in place.
   - *Why:* rootless runs every container in a user namespace; the postgres process (image UID 999) maps to a host subuid (~100998), so adopting the root-owned Docker volume in place would require `podman unshare chown -R` on the data dir — which **mutates the original volume and burns the rollback path** (the old Docker container expects UID 999). Dump/restore leaves the original Docker volume **pristine**, so rollback = "restart the old Docker container on the untouched volume" with **zero data risk**. Rootless effectively *chooses* dump/restore, and the result is a *safer* rollback than the in-place path.
   - *This resolves the original draft's internal contradiction* ("volume doesn't move" vs "Postgres migrates Docker → Podman"): the **engine** moves Docker → rootless Podman; the **data** is re-loaded from a dump while the **source volume is never touched**.
   - *Cost is low:* single-user CRM, small DB. The fresh volume's `init-db.sql` hook pre-creates `uuid-ossp` + `vector` (same pgvector image, so the `vector` type exists before the data loads); `pg_dump -Fc` → `pg_restore`; downtime measured in minutes.

2. **Rehearse on a local arm64 VM, then a focused on-Pi host-check.** The Mac can't run Quadlets (Linux/systemd-only) and B's VPS doesn't exist yet, so:
   - **Stage 1 — local arm64 Linux VM (disposable):** iterate the full flow (Dockerfiles build → Quadlets up → dump/restore round-trip → health-gate → exercise rollback). Apple Silicon runs arm64 Linux natively/fast; reset-and-retry as needed. Proves the **mechanics**.
   - **Stage 2 — on-Pi host-check (before the real window):** validate only the **host-specific unknowns** that a VM can't (subuid ranges present for `crm`, cgroup-v2 delegation enabled for `crm`, linger) with a throwaway rootless container + a test restore into a scratch volume. Then perform the real cutover.

3. **A0 leaves the edge nearly untouched — except it pulls A's API-key edge-injection forward (the one exception).** Containers publish to **`127.0.0.1:8080`** (backend) and **`127.0.0.1:3001`** (frontend) — the exact ports Caddy already proxies to (and `127.0.0.1` binding is *tighter* than today's `*:8080`). Tailscale Serve and Caddy's routing keep working with zero change. **The single edge change A0 makes:** the frontend currently bakes `NEXT_PUBLIC_API_KEY` and sends `X-API-Key` *from the browser* (`api-client.ts`). A public, no-secret image (Decision 4) cannot bake the prod key, and a key-less image 401s. So A0 builds the frontend image **key-less + same-origin** (`NEXT_PUBLIC_API_KEY` empty, `NEXT_PUBLIC_API_URL` empty) and adds **one line** to the Caddyfile — `header_up X-API-Key {env.CRM_API_KEY}` on the `/api/*` reverse_proxy, key read from Caddy's edge env. This is precisely A's already-chosen edge-injection design, brought forward because containerization forces it; *everything else* edge-side (Caddyfile-into-`infra/`, TLS, broader edge refactor) still stays in A. **Scope increase for A0 accepted by the operator.** Net security win: the key leaves the browser bundle entirely.

4. **GHCR: public package, no host credential.** The image bakes **no secrets** (`.env` is injected at runtime via `EnvironmentFile`), and the repo is already public, so a public package exposes only already-public compiled code. The Pi pulls with **no credential** (`podman pull ghcr.io/spengrah/…` just works), which fits the umbrella's "fewest standing credentials" model. Tags = **commit SHA** (immutable). Retention policy deferred (nice-to-have).

5. **Base images: distroless-static (backend) + node:22-bookworm-slim (frontend).**
   - **Backend** binary is **fully static** (no CGO-forcing deps; cross-compile defaults `CGO_ENABLED=0`) → **`gcr.io/distroless/static-debian12:nonroot`**: no shell, runs nonroot, includes CA certs (outbound HTTPS to Todoist/Google) + tzdata. Both `crm-api` and `crm-admin` live in this image; `podman exec … crm-admin --flag` runs the binary directly (no shell needed).
   - **Frontend** runtime base = **`node:22-bookworm-slim`** (glibc — safe for any `linux-arm64` native prebuild such as `sharp`; alpine/musl can break prebuilt addons), running `node server.js` from the CI-built standalone. Chosen over distroless-node for glibc native-addon compatibility + easier debugging. *(De-risking 2026-06-10 found the frontend has zero runtime native addons — see below — so this is now headroom, not a requirement.)*

6. **Quadlet runtime via a pinned static podman ≥5.x (NOT stock apt).** Debian 12 (bookworm) ships only podman **4.3.1**, which **cannot run Quadlets** (the `.container` generator needs ≥4.4). So both the Pi and the rehearsal VM install a **maintained static arm64 podman 5.x binary** (e.g. the `podman-static` project, which bundles crun/conmon/netavark/aardvark) rather than the apt package. *Why this over `podman generate systemd` on 4.3.1:* `generate systemd` is deprecated and slated for removal, so building new infra on it is a teaching anti-pattern; pinning a modern podman keeps us on the current best-practice declarative Quadlet format that staging reuses verbatim. *Cost:* one manually-updated binary outside apt (occasional update chore + a third-party trust surface). The stock-apt rootless *runtime* deps are all present (crun 1.8.1, conmon, slirp4netns, passt, aardvark-dns, netavark, catatonit), so the static binary only has to supply podman itself.

## De-risking findings (verified 2026-06-10)

Reconnaissance done before authoring any artifacts — several risks retired, one decision (6) forced:

**Retired:**
- **Frontend makes no server-side calls to the backend.** `frontend/src/lib/api-client.ts` uses `process.env.NEXT_PUBLIC_API_URL || ''` → empty → all API calls are *browser-side* to `/api/*` (through Tailscale Serve → Caddy). No `route.ts` handlers, no server components fetching the backend. → the frontend container never talks to the backend container, so the original SSR-networking MUST-VERIFY is **moot**; only **backend↔Postgres** needs a shared Quadlet `.network`.
- **Frontend has zero runtime native addons.** `next.config.ts` has no `images` config, no `next/image` usage → no `sharp` at runtime. The `.next/standalone` bundle is pure JS; the `*-darwin-arm64` `.node` files in `node_modules` are all build-time tooling (SWC/Tailwind-oxide/Rollup/Vite/fsevents) that doesn't ship. The "x64-native addon" worry is fully retired.
- **Backend is CGO-free.** No `import "C"` in the tree; CI cross-compiles with plain `GOARCH=arm64 go build` (no `CC`) → fully static → distroless-static:nonroot validated.

**Pi host-prep reality (read-only recon):** Debian 12 (bookworm), aarch64, cgroup **v2 ✅**. But: **podman not installed**, **`uidmap` (newuidmap/newgidmap) missing**, **no `/etc/subuid`+`/etc/subgid` ranges for `crm`**, **no linger**. `crm` is **uid 995 — a system account** (and is in the `docker` group, which is how today's `database.service` drives `docker compose`). Rootless `systemctl --user` for a *system* account works but needs linger + a running user manager — validate in the Stage-2 on-Pi check.

## Rehearsal results — Stage 1 (lima Debian 12 arm64 VM, 2026-06-10)

Ran on a fresh lima Debian-12 aarch64 VM with a deliberately-created `crm` **system** account (uid 999, no pre-provisioned subuid — mirroring the Pi's hard case). **Both load-bearing risks retired:**

- **podman-static v5.8.2 + rootless Quadlets work as a system account.** Install = download the `mgoltzsche/podman-static` `podman-linux-arm64.tar.gz` and `cp -r usr etc /` (ships podman 5.8.2, crun, conmon 2.2.1, netavark 1.17.2, aardvark-dns 1.17.1, pasta, and the Quadlet generator + `user-generators/podman-user-generator` symlink — which is in systemd's user-generator search path). `apt install uidmap` still required (setuid `newuidmap`/`newgidmap`).
- **Host-prep sequence works:** create `crm` (system, with a home for `~/.config/containers/systemd/` + storage) → add `/etc/subuid`+`/etc/subgid` ranges → `loginctl enable-linger crm` (creates `/run/user/999` + the user manager). Invoke via `sudo -u crm XDG_RUNTIME_DIR=/run/user/999 DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/999/bus systemctl --user …`.
- **A `.container` Quadlet started end-to-end:** `daemon-reload` → generator produced `hello.service` → `systemctl --user start` → **active (running)** under `/user.slice/user-999.slice/user@999.service/app.slice/…`, pasta networking up, `podman ps` confirms.
- **Postgres dump/restore round-trip proven:** seed pgvector (vector + uuid-ossp, 3-row `vector(3)` table) → `pg_dump -Fc` → fresh **Quadlet-managed pgvector target on a named `.volume`** → `pg_restore --no-owner --clean --if-exists` → **3 rows restored, `<->` similarity query works, both extensions present, data survives a `systemctl --user restart`.** Extensions are carried by the custom-format dump, so init-db.sql pre-creation is belt-and-suspenders, not required.

**Still to rehearse (Stage 1 continued, lower-risk now that foundations hold):** build + run the actual app images (distroless backend, node-slim frontend) with health-gates; backend↔Postgres over a shared Quadlet `.network` (aardvark DNS, `postgres:5432`); the Caddy `X-API-Key` injection; a full rollback drill.

**Two minor items flagged for verification:** (1) `journalctl --user` returned no entries for a no-output test container — confirm container/service log routing with a container that actually logs (may need `journalctl _UID=999` from root for a system account). (2) podman reported `cgroupManager: cgroupfs`; confirm per-container `MemoryMax`/`CPUQuota` (the units set 512M/150%) actually apply under the user slice (cgroup-v2 controller **delegation** for `crm`).

## Cutover flow (one-time, manual, rehearsed per Decision 2)

```
1. CI builds arm64 images (backend incl. crm-admin → distroless-static; frontend standalone → node:22-slim) → push to GHCR (public, SHA-tagged)
2. author rootless Quadlet units (.container/.volume/.network) mirroring personalcrm.target as a `systemctl --user` target under `crm`
3. host prep on the Pi (all confirmed-needed by recon): install pinned static podman 5.x; apt install uidmap; add /etc/subuid+subgid ranges for crm; loginctl enable-linger crm; verify cgroup-v2 delegation for crm
4. pg_dump the live Docker Postgres → restore into a fresh named Podman volume (init-db.sql pre-creates extensions); ORIGINAL Docker volume untouched
5. repoint scripts/backup-db.sh to the new Podman volume path (under crm's storage)
6. stop systemd app units (backend, frontend); leave the old Docker Postgres reachable as the rollback anchor
7. start rootless Quadlet app containers (backend + frontend), pointed at the restored Podman Postgres
8. health-gate (/health + frontend + smoke)
      FAIL → stop Quadlet containers → restart old systemd units (+ old Docker Postgres on its pristine volume) → investigate
      PASS → disable old systemd app units + old Docker Postgres compose unit; commit the new infra/
```

## Host-setup checklist (rootless prerequisites — confirmed NEEDED by recon; verified in Stage-2 on-Pi check)

- [ ] **Install a pinned static podman ≥5.x** (Decision 6) — stock apt is 4.3.1, too old for Quadlets.
- [ ] **`apt install uidmap`** — `newuidmap`/`newgidmap` are missing; rootless cannot map UIDs without them.
- [ ] **Add `/etc/subuid` + `/etc/subgid` ranges for `crm`** — currently absent (system account, not auto-provisioned). e.g. `usermod --add-subuids 100000-165535 --add-subgids 100000-165535 crm`.
- [ ] `loginctl enable-linger crm` — currently off; needed so the user's containers start at boot / survive logout (and so `systemctl --user` works for this *system* account).
- [ ] cgroup-v2 **delegation** for `crm` (per-container `MemoryLimit`/`CPUQuota` — current units set 512M / 150%). cgroup is v2 ✅; confirm delegation for the user slice.
- [ ] Quadlets land in `~crm/.config/containers/systemd/`; orchestrated via `systemctl --user` (run via `machinectl shell crm@` or `sudo -u crm XDG_RUNTIME_DIR=/run/user/995 systemctl --user …` since `crm` is a system account).
- [ ] **Log-access ergonomics change:** logs move to the user journal → `journalctl --user -u personalcrm-backend` (as `crm`) or `journalctl _UID=995` from root. Update the ops runbook + the operator memory note (which currently documents the system-journal command).

## Implementation mechanics (plan-ready)

- **Dockerfiles.** Backend: thin packaging layer — `COPY` the CI-cross-compiled `GOARCH=arm64` `crm-api` + `crm-admin` into `distroless/static-debian12:nonroot` (no in-Dockerfile build, no emulation). Frontend: `COPY` the CI-built `linux/arm64` Next standalone into `node:22-bookworm-slim`; **confirm the standalone bundles only `linux-arm64` prebuilds** (watch `sharp` — needed only if server-side image optimization is used) by building the standalone in an arm64-targeted CI context (arm64 runner / buildx-QEMU).
- **Quadlet units.** `.container` for backend + frontend + Postgres; `.volume` for the Postgres named volume; `.network` shared by **backend + Postgres only** (the frontend needs no link to either container — see below). Rootless (`systemctl --user`, run as `crm`); `EnvironmentFile=/srv/personalcrm/.env` wiring preserved; `personalcrm.target` re-expressed as a user target; journald (user) logging confirmed; carry over the existing units' `MemoryLimit`/`CPUQuota`/health-check semantics.
  - **Networking (resolved by recon):** the frontend makes **no** server-side calls to the backend (all API traffic is browser-side `/api/*` via Caddy), so the frontend container only needs to publish `127.0.0.1:3001`. The only container↔container link is **backend → Postgres** over the shared `.network` (`postgres:5432` via aardvark DNS). The backend still publishes `127.0.0.1:8080` for Caddy.
- **Postgres volume migration.** `pg_dump -Fc` from the live Docker container → `pg_restore` into the fresh Podman named volume (extensions pre-created by `init-db.sql`). **Repoint `scripts/backup-db.sh`** from `/var/lib/docker/volumes/infra_postgres_data/_data` to the Podman volume path under `crm`'s storage; verify the physical-copy backup/restore round-trips post-migration. Original Docker volume retained (untouched) as the rollback anchor until cutover is confirmed.
- **GHCR setup.** Public repo package; CI pushes SHA-tagged `arm64` images (A's build job; A0 defines the Dockerfiles + the initial manual push for rehearsal). Host pulls need no auth. Retention deferred.
- **Caddy.** No change (Decision 3). Document that the published container ports MUST stay 8080/3001 so the existing Caddyfile keeps resolving.
- **`crm-admin` in the image.** Bake `crm-admin` into the backend image (A needs `crm-admin --migrate`). Update build sites that currently only produce `crm-api-arm64` to also produce/package `crm-admin-arm64` (CI already cross-compiles `crm-admin-arm64`; ensure the Dockerfile `COPY`s it).
- **Cutover runbook + rollback rehearsal.** The flow above, expanded into an exact step list; deliberately exercise the "health-gate fails → restart old systemd units + old Docker Postgres" rollback in Stage-1 before trusting the cutover; record a downtime estimate from the rehearsal.

## Existing primitives to build on

- `infra/` systemd units (`personalcrm.target`, `personalcrm-{backend,frontend,database}.service`) — the shape the Quadlet units mirror.
- `infra/docker-compose.yml` + `infra/init-db.sql` — the Postgres config + extension bootstrap to carry into the Podman Postgres `.container`/`.volume`.
- `scripts/backup-db.sh` — physical Postgres volume copy; repoint to the Podman volume path.
- The already-containerized Docker Postgres — the rollback anchor (kept pristine through cutover).
- CI cross-compile of `crm-api-arm64` **and** `crm-admin-arm64` (`.github/workflows/ci.yml`) + the CI frontend-standalone build — extend to image build + push.

## Dependencies

- **Precursor to A** (A automates the containerized deploy A0 establishes).
- **Feeds C** (staging reuses A0's images + Quadlet units, with a staging env file).
- **Independent of B and D.**
