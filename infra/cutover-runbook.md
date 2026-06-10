# A0 Containerization Cutover — Runbook

One-time, manual cutover of prod from the systemd + Docker-Postgres hybrid to rootless Podman Quadlets. Design + rationale: `.ai/spec/2026-06-07-containerization-cutover-design.md`. Every command below was validated in a lima Debian-12 arm64 VM against a `crm` system account mirroring the Pi.

Throughout: `$PI_HOST` is the Pi's SSH host; `crm` is the existing service user. Helper for rootless user-systemd / podman as `crm`:

```bash
CRM_UID=$(ssh "$PI_HOST" id -u crm)
USERENV="XDG_RUNTIME_DIR=/run/user/$CRM_UID DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/$CRM_UID/bus"
crm_ctl()    { ssh "$PI_HOST" "sudo -n -u crm $USERENV systemctl --user $*"; }
crm_podman() { ssh "$PI_HOST" "sudo -n -u crm XDG_RUNTIME_DIR=/run/user/$CRM_UID podman $*"; }
```

## Phase 0 — Build & publish images (no prod impact)

1. Merge the A0 branch; the `Build & Push Images` workflow builds + pushes `personalcrm-{backend,frontend}` arm64 images to GHCR (or run it via `workflow_dispatch`).
2. **One-time:** flip both GHCR packages to **public** (repo → Packages → each package → Settings → Change visibility → Public). Decision 4: public package, no host pull credential.
3. Sanity check from the Pi (after Phase 1 installs podman): `crm_podman pull ghcr.io/spengrah/personalcrm-backend:latest`.

## Phase 1 — Stage-2 on-Pi host-check (low-risk; does NOT touch the running stack)

Recon already confirmed these are all absent on the Pi. Apply + verify:

```bash
# pinned static podman >=5.x (stock apt is 4.3.1, too old for Quadlets)
ssh "$PI_HOST" 'cd /tmp && curl -fsSL -o podman.tgz \
  https://github.com/mgoltzsche/podman-static/releases/download/v5.8.2/podman-linux-arm64.tar.gz \
  && tar -xzf podman.tgz && sudo cp -r podman-linux-arm64/usr podman-linux-arm64/etc /'
ssh "$PI_HOST" 'sudo apt-get update && sudo apt-get install -y uidmap'   # newuidmap/newgidmap (setuid)
ssh "$PI_HOST" 'getent group crm >/dev/null; grep -q "^crm:" /etc/subuid || \
  sudo usermod --add-subuids 200000-265535 --add-subgids 200000-265535 crm'
ssh "$PI_HOST" 'sudo loginctl enable-linger crm'
# verify
ssh "$PI_HOST" 'podman --version; grep crm /etc/subuid /etc/subgid; ls -ld /run/user/'"$CRM_UID"
crm_podman run --rm docker.io/library/alpine:3.20 echo rootless-ok   # must print rootless-ok
```

Confirm cgroup-v2 delegation by starting a throwaway Quadlet and checking `MemoryMax` applies (see spec rehearsal); the system slice delegated memory+cpu in rehearsal.

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
crm_ctl daemon-reload
crm_ctl start personalcrm-database.service
# wait healthy
ssh "$PI_HOST" "until sudo -n -u crm XDG_RUNTIME_DIR=/run/user/$CRM_UID podman exec crm-postgres pg_isready -U crm_user; do sleep 1; done"
# restore the dump into the fresh Podman volume
cat /tmp/crm-cutover.dump | crm_podman exec -i crm-postgres pg_restore -U crm_user -d personal_crm --no-owner --clean --if-exists

# stop the OLD systemd app units (writers) so only the new stack serves
ssh "$PI_HOST" 'sudo systemctl stop personalcrm-backend personalcrm-frontend'

crm_ctl start personalcrm-backend.service personalcrm-frontend.service
ssh "$PI_HOST" 'sudo systemctl reload caddy'   # pick up the X-API-Key injection
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
