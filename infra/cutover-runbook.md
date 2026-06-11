# A0 Containerization Cutover — Runbook

One-time, manual cutover of prod from the systemd + Docker-Postgres hybrid to rootless Podman Quadlets. Design + rationale: `.ai/spec/2026-06-07-containerization-cutover-design.md`. Every command below was validated in a lima Debian-12 arm64 VM against a `crm` system account mirroring the Pi.

Throughout: `$PI_HOST` is the Pi's SSH host; `crm` is the existing service user (uid 995, system account, home `/var/lib/personalcrm`). Helper for rootless user-systemd / podman as `crm` — note the `cd /tmp` + explicit `HOME`: interactive `sudo -u crm` inherits the SSH user's CWD/HOME which crm can't access, so rootless podman fails to chdir without this (the Quadlet *services* run under systemd and are unaffected):

```bash
CRM_UID=$(ssh "$PI_HOST" id -u crm)
CRM_HOME=/var/lib/personalcrm
USERENV="HOME=$CRM_HOME XDG_RUNTIME_DIR=/run/user/$CRM_UID DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/$CRM_UID/bus"
crm_ctl()    { ssh "$PI_HOST" "cd /tmp && sudo -n -u crm $USERENV systemctl --user $*"; }
crm_podman() { ssh "$PI_HOST" "cd /tmp && sudo -n -u crm HOME=$CRM_HOME XDG_RUNTIME_DIR=/run/user/$CRM_UID podman $*"; }
```

## Phase 0 — Build & publish images (no prod impact)

1. Merge the A0 branch; the `Build & Push Images` workflow builds + pushes `personalcrm-{backend,frontend}` arm64 images to GHCR (or run it via `workflow_dispatch`).
2. **One-time:** flip both GHCR packages to **public** (repo → Packages → each package → Settings → Change visibility → Public). Decision 4: public package, no host pull credential.
3. Sanity check from the Pi (after Phase 1 installs podman): `crm_podman pull ghcr.io/spengrah/personalcrm-backend:latest`.

## Phase 1 — Stage-2 on-Pi host-check (low-risk; does NOT touch the running stack)

**DONE 2026-06-10** (zero-downtime portion). Applied + verified on the Pi:

- ✅ podman-static **v5.8.2** installed (stock apt is 4.3.1, too old for Quadlets): downloaded the release, `sudo cp -r usr etc /`.
- ✅ `apt install uidmap` (newuidmap/newgidmap).
- ✅ `crm` subuid/subgid added: `crm:200000:65536` in `/etc/subuid`+`/etc/subgid` (no overlap with `spencer:100000:65536`).
- ✅ `loginctl enable-linger crm` → `/run/user/995` present.
- ✅ `/var/lib/personalcrm` created (crm-owned, 0700) — crm's HOME / rootless storage root.
- ✅ rootless podman verified: `podman run` works, **overlay** driver, storage at `/var/lib/personalcrm/.local/share/containers/storage`, `Rootless: true`.

**Deferred to the window (Phase 4):** `usermod -d /var/lib/personalcrm crm` refuses while crm's services are live, so the permanent passwd-home change + user-manager bounce happen after stopping the old units. cgroup-v2 delegation was proven in the VM rehearsal (Pi is cgroup v2); confirm in-window with a throwaway Quadlet `MemoryMax`.

> **Log access changes:** podman-static's conmon lacks journald, so container logs are `crm_podman logs crm-backend`, NOT `journalctl -u personalcrm-backend`. Update the operator memory note. (Service start/stop/health still journal.)

## Phase 2 — Pre-cutover backup (writers stopped)

Physical safety copy of the **old Docker volume** (the post-cutover `scripts/backup-db.sh` targets the Podman volume, so back up directly here):

```bash
ssh "$PI_HOST" 'sudo systemctl stop personalcrm-backend personalcrm-frontend && cd /srv/personalcrm/infra && docker compose stop postgres'
ssh "$PI_HOST" 'sudo cp -a /var/lib/docker/volumes/infra_postgres_data/_data /var/lib/docker/volumes/infra_postgres_data/_data.bak-cutover'
ssh "$PI_HOST" 'cd /srv/personalcrm/infra && docker compose start postgres'   # bring DB back up for the dump
```

Take a fresh `pg_dump` from the **live Docker** Postgres — this is the migration payload (the Docker volume is left pristine as the rollback anchor):

```bash
ssh "$PI_HOST" 'docker exec crm-postgres pg_dump -U crm_user -Fc -d personal_crm' > /tmp/crm-cutover.dump
ls -la /tmp/crm-cutover.dump   # sanity: non-trivial size
```

## Phase 3 — Install units + .env changes

```bash
ssh "$PI_HOST" 'sudo install -d -o crm -g crm /home/crm/.config/containers/systemd'
scp infra/quadlet/* "$PI_HOST":/tmp/quadlet/    # then move into place owned by crm
ssh "$PI_HOST" 'sudo cp /tmp/quadlet/* /home/crm/.config/containers/systemd/ && sudo chown -R crm:crm /home/crm/.config'
ssh "$PI_HOST" 'sudo cp '"$(pwd)"'/infra/init-db.sql /srv/personalcrm/infra/init-db.sql'  # or scp it
```

Edit `/srv/personalcrm/.env`: set **`DATABASE_URL` host to `crm-postgres`** (was localhost) — the backend reaches Postgres over the `crm` Quadlet network. Confirm `POSTGRES_PASSWORD`, `POSTGRES_USER=crm_user`, `API_KEY` present.

Wire the Caddy edge key injection (revised Decision 3):

```bash
ssh "$PI_HOST" 'sudo cp '"$(pwd)"'/infra/caddy/Caddyfile /etc/caddy/Caddyfile'   # adds header_up X-API-Key
# CRM_API_KEY for Caddy's env (root-owned 0600), then point caddy.service at it:
ssh "$PI_HOST" 'echo "CRM_API_KEY=<the prod API key>" | sudo tee /etc/caddy/crm.env >/dev/null && sudo chmod 600 /etc/caddy/crm.env'
ssh "$PI_HOST" 'sudo systemctl edit caddy'   # add [Service]\nEnvironmentFile=/etc/caddy/crm.env
```

## Phase 4 — The flip (brief downtime)

```bash
# 1. stop the OLD crm services (writers) so the user + DB are quiesced
ssh "$PI_HOST" 'sudo systemctl stop personalcrm-backend personalcrm-frontend'
ssh "$PI_HOST" 'cd /srv/personalcrm/infra && docker compose stop postgres'   # old DB stays on its pristine volume = rollback anchor

# 2. permanent crm home change (deferred from Phase 1; crm now has no live procs).
#    Bounce linger so the user manager (user@995) restarts with HOME=/var/lib/personalcrm.
ssh "$PI_HOST" 'sudo loginctl disable-linger crm'
ssh "$PI_HOST" 'sudo usermod -d /var/lib/personalcrm crm && getent passwd crm'
ssh "$PI_HOST" 'sudo loginctl enable-linger crm'; sleep 2

# 3. bring up the rootless stack + restore the dump
crm_ctl daemon-reload
crm_ctl start personalcrm-database.service
ssh "$PI_HOST" "cd /tmp && until sudo -n -u crm HOME=/var/lib/personalcrm XDG_RUNTIME_DIR=/run/user/$CRM_UID podman exec crm-postgres pg_isready -U crm_user; do sleep 1; done"
cat /tmp/crm-cutover.dump | crm_podman exec -i crm-postgres pg_restore -U crm_user -d personal_crm --no-owner --clean --if-exists

crm_ctl start personalcrm-backend.service personalcrm-frontend.service
ssh "$PI_HOST" 'sudo systemctl reload caddy'   # pick up the X-API-Key injection

# 4. confirm cgroup-v2 delegation actually bit (proven in VM; confirm on Pi)
crm_ctl show personalcrm-backend.service -p MemoryMax --value   # expect 536870912
```

## Phase 5 — Health gate

```bash
ssh "$PI_HOST" 'curl -sf http://127.0.0.1:8080/health' | grep -q '"database":{"status":"healthy"' && echo BACKEND_OK
ssh "$PI_HOST" 'curl -sf http://127.0.0.1:3001 >/dev/null' && echo FRONTEND_OK
ssh "$PI_HOST" 'curl -s -o /dev/null -w "%{http_code}\n" http://localhost:80/api/v1/contacts'   # expect 200 (edge injects key)
```

- **PASS →** Phase 6.
- **FAIL →** rollback (below).

## Rollback (rehearsed)

The old Docker Postgres volume is untouched, so rollback is lossless:

```bash
crm_ctl stop personalcrm-frontend.service personalcrm-backend.service personalcrm-database.service
ssh "$PI_HOST" 'cd /srv/personalcrm/infra && docker compose start postgres'
ssh "$PI_HOST" 'sudo cp '"$(pwd)"'/infra/caddy/Caddyfile.orig /etc/caddy/Caddyfile && sudo systemctl reload caddy'  # restore pre-injection Caddyfile
ssh "$PI_HOST" 'sudo systemctl start personalcrm-backend personalcrm-frontend'
```

(Keep a copy of the original `/etc/caddy/Caddyfile` as `Caddyfile.orig` before Phase 3.)

## Phase 6 — Commit the cutover

```bash
ssh "$PI_HOST" 'sudo systemctl disable --now personalcrm-backend personalcrm-frontend personalcrm-database'  # old units
crm_ctl enable personalcrm-database.service personalcrm-backend.service personalcrm-frontend.service
```

- Verify boot-survival: reboot the Pi, confirm the Quadlet stack comes up (linger).
- Update the operator memory note: logs via `podman logs`, services are `systemctl --user` as `crm`.
- Retain the Docker volume + the `_data.bak-*` copy for a cooling-off period, then reclaim.
