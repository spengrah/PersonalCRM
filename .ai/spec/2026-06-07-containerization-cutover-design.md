# A0 — Containerization Cutover — Design

**Date:** 2026-06-07
**Status:** Design — direction locked this session. Implementation-detail threads remain.
**Author:** spengrah (brainstormed with Claude)
**Parent:** `2026-06-07-deploy-and-staging-overview-design.md`

## Scope

A **one-time cutover** of prod (and the shared `infra/`) from today's **systemd + Docker-Postgres hybrid** to a **fully containerized** stack under **Podman Quadlets**, with the **container image as the deploy artifact** and **Caddy kept native**. This is the **foundational precursor to A**: A automates a deploy that assumes containers + image artifacts, so the runtime must be containerized first. A0 also produces the image definitions + Quadlet units that **C reuses verbatim for staging**.

Split out as its own sub-project (rather than folded into A) because it is the single prod-touching, slightly-riskier milestone, and its artifact-format decision is foundational to everything in the A track.

## Why now (sequencing)

A's build job *is* "build and push the image"; A's deploy job *is* "pull the image and swap the Quadlet." If A were built against the current tarball/systemd model and the stack containerized later, the deploy path would be built twice. So containerization is cheapest done **before A** — this is the cheapest possible moment (pre-implementation of the whole initiative).

## Decisions locked (from brainstorm)

- **Image-as-artifact.** The deploy artifact is an `arm64` container image. The *same image bytes* run on the Pi (prod) and the VPS (staging) — the truest prod/staging parity, which is the whole reason staging exists. Promote-by-tag; code rollback = "run the previous tag."
- **Podman Quadlets, not Compose.** Containers are declared as systemd units, so the existing model carries over (one supervisor, journald — `journalctl -u personalcrm-backend` still works, `personalcrm.target` still orchestrates) and rootless/no-daemon fits the lockdown ethos. Compose was rejected as a second supervisor + root daemon on an already-systemd box.
- **Caddy stays native** (binds `:80`; avoids the rootless privileged-port wrinkle). Only **backend + frontend + Postgres** are containerized.
- **Build with Docker buildx in CI, run with Podman on the hosts** (OCI-standard; fine — A's build job, A0 defines the Dockerfiles).
- **Prod cutover is one-time and low-risk.** Today's prod is systemd + **Docker-Postgres** — the only stateful component is *already* containerized, so it doesn't move (its volume + `backup-db.sh` stay). Cutover = wrap the two stateless app processes in images, point the app containers at the existing Postgres volume, health-check; rollback = restart the old systemd units.
- **Postgres migrates Docker → Podman** as part of the cutover (timed with the backup/restore rewrite A needs anyway).

## Cutover flow (one-time, manual, rehearsed on staging-shaped box or locally first)

```
1. build arm64 images (backend incl. crm-admin, frontend) → push to GHCR
2. author Quadlet units (.container/.volume/.network) mirroring personalcrm.target
3. migrate Postgres Docker volume → Podman storage (or adopt existing volume); repoint backup-db.sh
4. stop systemd app units (backend, frontend); leave DB reachable
5. start Quadlet app containers pointed at the (existing) Postgres volume
6. health-gate (/health + frontend + smoke)
      FAIL → stop containers → restart old systemd units → investigate
      PASS → disable old systemd app units; commit the new infra/
```

## Open threads / TODO (fill out here)

- [ ] **Dockerfiles.** Backend: COPY the `GOARCH=arm64` cross-compiled `crm-api` + `crm-admin` into a distroless arm64 base (no emulation). Frontend: wrap the Next standalone in an arm64 node base — **verify `node_modules` has no x64-native addons** (else build on a public arm64 runner / buildx-QEMU).
- [ ] **Quadlet units.** `.container`/`.volume`/`.network` for backend + frontend + Postgres; rootless (run as `crm`); `EnvironmentFile` wiring; boot-enable + `personalcrm.target` membership preserved; journald logging confirmed.
- [ ] **Postgres volume migration.** Docker volume → Podman storage mechanics; whether to adopt-in-place vs dump/restore; **repoint `scripts/backup-db.sh`** (physical volume copy) to the Podman volume path; verify backup/restore round-trips post-migration.
- [ ] **GHCR setup.** Repo package, auth for push (CI) + pull (hosts), tag scheme (commit SHA), retention.
- [ ] **Caddy native config.** Bring the Caddyfile into `infra/` (shared with A's edge key-injection work), parameterized upstreams now pointing at the containerized backend/frontend ports.
- [ ] **Cutover runbook + rollback rehearsal.** Exact step list; deliberately exercise the "health-gate fails → restart old systemd units" rollback before trusting the cutover; downtime estimate.
- [ ] **`crm-admin` in the image.** Bake `crm-admin` into the backend image (A needs `crm-admin --migrate`); update build sites that currently only produce `crm-api-arm64`.

## Existing primitives to build on

- `infra/` systemd units (`personalcrm.target`, `personalcrm-{backend,frontend,database}.service`) — the shape the Quadlet units mirror.
- `scripts/backup-db.sh` — physical Postgres volume copy; repoint to Podman storage.
- The already-containerized Docker Postgres (the only stateful component; it stays put).
- CI cross-compile of `crm-api-arm64` (`.github/workflows/ci.yml`) — extend to images + `crm-admin`.

## Dependencies

- **Precursor to A** (A automates the containerized deploy A0 establishes).
- **Feeds C** (staging reuses A0's images + Quadlet units, with a staging env file).
- **Independent of B and D.**
