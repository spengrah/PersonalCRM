# Self-Hosted Runner Installation — Runbook

One-time, manual install of the self-hosted GitHub Actions runner and the root-owned deploy scripts that turn a `push` to `main` into a prod deploy. Design + rationale: `.ai/spec/2026-06-07-deploy-automation-design.md`. This runbook documents the exact, reproducible steps the coordinator executes on the Pi to wire up the automation shipped by `scripts/deploy-artifact.sh` + `scripts/backup-db.sh` + `scripts/restore-db.sh` and `.github/workflows/deploy-prod.yml`.

Throughout: `$PI_HOST` is the Pi's SSH host (`raspberet`); `crm` is the existing service user (uid 995, system account, home `/var/lib/personalcrm`, linger on) that owns the rootless Podman runtime. The deploy automation introduces a SECOND, separate identity — `gha-runner` — which runs the GitHub Actions agent and whose only privilege is to invoke one immutable, reviewed script as root. The two identities are deliberately distinct: the runner (untrusted CI surface) is never `crm` and never root; it can only call `deploy-artifact.sh`, which internally hops to `crm` for every rootless op.

Rootless crm-user helper (used in the manual recovery section; the deploy scripts define their own identical helpers internally):

```bash
CRM_UID=$(ssh "$PI_HOST" id -u crm)
CRM_HOME=/var/lib/personalcrm
USERENV="HOME=$CRM_HOME XDG_RUNTIME_DIR=/run/user/$CRM_UID DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/$CRM_UID/bus"
crm_ctl()    { ssh "$PI_HOST" "cd /tmp && sudo -n -u crm $USERENV systemctl --user $*"; }
crm_podman() { ssh "$PI_HOST" "cd /tmp && sudo -n -u crm HOME=$CRM_HOME XDG_RUNTIME_DIR=/run/user/$CRM_UID podman $*"; }
```

> **Status:** the coordinator has ALREADY executed the runner registration + script install on the Pi (this is "bucket B" operational wiring). This document is the authoritative, reproducible record of what was done, so a future re-install (new Pi, runner re-registration, disaster recovery) follows the same steps exactly.

## 0. Overview & security model

The promotion model is: `make promote` fast-forwards `main` to a reviewed `develop` SHA → `deploy-prod.yml` fires on the `main` push → the self-hosted Pi runner gates on the recorded CI + image-build success for that exact SHA, then runs `sudo /usr/local/sbin/deploy-artifact.sh "$GITHUB_SHA"`. Everything below makes that one `sudo` call possible and safe.

Security posture:

- **Runner identity ≠ workload identity.** The runner runs as `gha-runner` (no login shell, no `crm`, no root). The workload (Podman containers, the DB volume) runs as `crm`. A compromise of the CI surface cannot directly touch the crm-user store or the DB volume — it can only invoke the one allowed script.
- **The runner's entire privileged capability is one line of sudoers:** `gha-runner ALL=(root) NOPASSWD: /usr/local/sbin/deploy-artifact.sh`. Nothing else. No arg-matching (the script validates its own 40-hex SHA arg), no shell, no other binary.
- **The deploy scripts are root-owned and immutable to the runner** (`/usr/local/sbin/{deploy-artifact,backup-db,restore-db}.sh`, `root:root`, `0755`). The `gha-runner` user cannot edit them, so it cannot escalate by rewriting the one script it is allowed to run.
- **`deploy-artifact.sh` runs as root but does NOT use root's Podman.** Every `podman`/`systemctl` op inside it hops to the `crm` user (rootless store) via `sudo -u crm HOME=… XDG_RUNTIME_DIR=…`. The only genuine-root ops are the cold DB-volume copy (inside `backup-db.sh`/`restore-db.sh`) and snapshot pruning.

## Phase 1 — Create the `gha-runner` system user

A dedicated system account for the runner agent. It owns the runner install directory (`/opt/actions-runner`) but has no login shell.

```bash
ssh "$PI_HOST" 'sudo useradd --system --shell /usr/sbin/nologin --home-dir /opt/actions-runner --create-home gha-runner'
ssh "$PI_HOST" 'getent passwd gha-runner'   # sanity: home=/opt/actions-runner, shell=/usr/sbin/nologin
```

Rationale: `--system` (no aging, low uid), `--shell /usr/sbin/nologin` (no interactive login — the agent runs as a systemd service, not a login session), `--home-dir /opt/actions-runner --create-home` (the runner agent unpacks and runs from this directory; the agent writes work/diagnostic state under its home).

## Phase 2 — Install the GitHub Actions runner agent

The runner agent registers against the repo with the `pi` label (matching `runs-on: [self-hosted, pi]` in `deploy-prod.yml`) and runs as a **system** systemd service under `gha-runner`.

**Prerequisites** — the deploy workflow's CI/image gates use `curl` + `jq` (NOT `gh`), so both must be present on the runner host. Verify (install via the distro package manager if missing):

```bash
ssh "$PI_HOST" 'command -v curl jq'   # both must resolve; install with: sudo apt-get install -y curl jq
```

**Install + register** — download the arm64 runner, install its OS dependencies, register, then install + start the service:

```bash
# 1. Download + extract the runner into /opt/actions-runner (as gha-runner). Pin
#    the version to the current GitHub Actions runner release for arm64.
ssh "$PI_HOST" 'cd /opt/actions-runner && sudo -u gha-runner curl -fsSL -o actions-runner.tar.gz \
  https://github.com/actions/runner/releases/download/v<RUNNER_VERSION>/actions-runner-linux-arm64-<RUNNER_VERSION>.tar.gz && \
  sudo -u gha-runner tar xzf actions-runner.tar.gz && sudo -u gha-runner rm actions-runner.tar.gz'

# 2. Install OS-level dependencies the runner needs (run as root — it apt-installs).
ssh "$PI_HOST" 'cd /opt/actions-runner && sudo ./bin/installdependencies.sh'

# 3. Register against the repo. Obtain <REGISTRATION_TOKEN> from
#    repo Settings → Actions → Runners → New self-hosted runner (short-lived; do
#    NOT store it anywhere). Labels include `pi` (the deploy workflow's target).
ssh "$PI_HOST" 'cd /opt/actions-runner && sudo -u gha-runner ./config.sh \
  --url https://github.com/spengrah/PersonalCRM \
  --token <REGISTRATION_TOKEN> \
  --labels pi --name pi-runner --unattended'

# 4. Install + start as a SYSTEM service running as gha-runner.
ssh "$PI_HOST" 'cd /opt/actions-runner && sudo ./svc.sh install gha-runner && sudo ./svc.sh start'
```

Verify the service is active (the unit name is derived from the repo + runner name):

```bash
ssh "$PI_HOST" 'systemctl status actions.runner.spengrah-PersonalCRM.pi-runner.service --no-pager'
```

Rationale:

- `--labels pi` is load-bearing — `deploy-prod.yml` targets `runs-on: [self-hosted, pi]`. A runner without the `pi` label is never picked for a deploy.
- `--unattended` + a registration token obtained from the repo UI keeps the token out of any committed artifact. **Never embed a registration token in this doc** — it is short-lived and is fetched fresh from the repo Settings each time a runner is (re-)registered.
- `svc.sh install gha-runner` installs the agent as a system service that runs as `gha-runner` (not root, not `crm`).

## Phase 3 — Co-install the three deploy scripts (root-owned)

`deploy-artifact.sh` calls `backup-db.sh` and `restore-db.sh` by their **absolute installed paths** with `--local` (and `restore-db.sh --no-app-start` in the rollback path), so all three must be co-installed at `/usr/local/sbin/`, root-owned and read-only to the runner.

```bash
# Copy the three scripts from a checkout of the deployed SHA.
scp scripts/deploy-artifact.sh scripts/backup-db.sh scripts/restore-db.sh "$PI_HOST":/tmp/
ssh "$PI_HOST" 'sudo install -o root -g root -m 0755 /tmp/deploy-artifact.sh /usr/local/sbin/deploy-artifact.sh'
ssh "$PI_HOST" 'sudo install -o root -g root -m 0755 /tmp/backup-db.sh      /usr/local/sbin/backup-db.sh'
ssh "$PI_HOST" 'sudo install -o root -g root -m 0755 /tmp/restore-db.sh     /usr/local/sbin/restore-db.sh'
ssh "$PI_HOST" 'rm -f /tmp/deploy-artifact.sh /tmp/backup-db.sh /tmp/restore-db.sh'
ssh "$PI_HOST" 'ls -l /usr/local/sbin/{deploy-artifact,backup-db,restore-db}.sh'   # all root:root, -rwxr-xr-x
```

Rationale:

- **`root:root`, `0755`** — world-readable + executable, but only root can WRITE. The `gha-runner` user can run `deploy-artifact.sh` (via sudoers) but cannot edit it; this is what makes the single sudoers entry safe.
- `deploy-artifact.sh` references the other two by the absolute constants `/usr/local/sbin/backup-db.sh` and `/usr/local/sbin/restore-db.sh` (overridable by `BACKUP_SCRIPT` / `RESTORE_SCRIPT` env vars for tests only). It invokes them as `backup-db.sh --local --no-restart` (pre-migrate snapshot) and `restore-db.sh --local --no-app-start <snapshot>` (rollback). It never calls them repo-relative — it runs from an arbitrary CWD under the runner.
- **`backup-db.sh`/`restore-db.sh` default (ssh) mode is unchanged** — only the on-Pi `--local` mode is exercised here. The same scripts remain usable from a dev machine over ssh for manual backups (the default mode, no `--local`).

## Phase 4 — The single sudoers entry

The runner's ONLY root capability. Install it via a `/etc/sudoers.d/` drop-in, validated with `visudo -cf` BEFORE it is moved into place (a malformed sudoers file can lock out sudo for the whole host).

```bash
# Write the line to a temp file, validate it, then install root-owned 0440.
ssh "$PI_HOST" 'cat <<EOF | sudo tee /tmp/gha-runner-deploy >/dev/null
gha-runner ALL=(root) NOPASSWD: /usr/local/sbin/deploy-artifact.sh
EOF'
ssh "$PI_HOST" 'sudo visudo -cf /tmp/gha-runner-deploy'   # MUST print "parsed OK" before proceeding
ssh "$PI_HOST" 'sudo install -o root -g root -m 0440 /tmp/gha-runner-deploy /etc/sudoers.d/gha-runner-deploy'
ssh "$PI_HOST" 'sudo rm -f /tmp/gha-runner-deploy'
ssh "$PI_HOST" 'sudo visudo -cf /etc/sudoers.d/gha-runner-deploy'   # re-validate the installed file
```

The file contains EXACTLY this one line:

```text
gha-runner ALL=(root) NOPASSWD: /usr/local/sbin/deploy-artifact.sh
```

Rationale:

- **No arg-matching.** The sudoers line deliberately does NOT constrain the script's arguments (e.g. it does not pin the SHA). Arg-matching in sudoers is brittle and easy to bypass; instead, `deploy-artifact.sh` validates its own argument (`valid_sha`: exactly 40 lowercase hex, else exit 2 with no DB touched). The trust boundary is "this exact root-owned script," not "these exact args."
- **`0440`, `root:root`** — sudoers requires drop-ins to be non-writable by group/other; `visudo` enforces this. `visudo -cf` BEFORE install prevents a syntax error from breaking sudo host-wide.
- This is the runner's entire root surface. `deploy-artifact.sh` itself performs the `sudo -u crm` rootless hops and the genuine-root ops (volume copy, snapshot prune); the runner never gets a general root shell.

## Phase 5 — The Pi-local ntfy env file (PROD-MANDATORY)

`deploy-artifact.sh` posts deploy/rollback notifications to ntfy, reading its config from a Pi-local env file. The file is root-owned `0600` and **NEVER committed** (the topic is a capability token — anyone with it can publish/subscribe).

```bash
# Create /etc/personalcrm/ (root-owned) and the env file (0600). Use the REAL
# values on the Pi — the placeholders below are for documentation only.
ssh "$PI_HOST" 'sudo install -d -o root -g root -m 0755 /etc/personalcrm'
ssh "$PI_HOST" 'cat <<EOF | sudo tee /etc/personalcrm/ntfy.env >/dev/null
NTFY_URL=<https://ntfy.sh or a self-hosted base URL>
NTFY_TOPIC=<opaque-topic>
EOF'
ssh "$PI_HOST" 'sudo chmod 0600 /etc/personalcrm/ntfy.env && sudo chown root:root /etc/personalcrm/ntfy.env'
```

The file defines EXACTLY two variables (shown with placeholders — never commit real values):

```text
NTFY_URL=<https://ntfy.sh or a self-hosted base URL>
NTFY_TOPIC=<opaque-topic>
```

**PROD-MANDATORY post-install validation** — `deploy-artifact.sh` degrades-OPEN when the file is absent (it skips ntfy and continues, so the script stays environment-agnostic for staging). On PROD this is NOT acceptable: an unattended prod deploy must never run blind. The "notifications required" guarantee therefore comes from THIS runbook gate, not the script. Run it before bringing the runner into prod service, and ABORT prod bring-up if it fails:

```bash
ssh "$PI_HOST" '
  set -e
  test -f /etc/personalcrm/ntfy.env || { echo "FAIL: /etc/personalcrm/ntfy.env missing"; exit 1; }
  sudo grep -q "^NTFY_URL=" /etc/personalcrm/ntfy.env   || { echo "FAIL: NTFY_URL not set"; exit 1; }
  sudo grep -q "^NTFY_TOPIC=" /etc/personalcrm/ntfy.env || { echo "FAIL: NTFY_TOPIC not set"; exit 1; }
  echo "ntfy.env OK"
'
```

Rationale: the env file is the seam between the env-agnostic script (tolerant of a missing file) and prod's hard requirement that every deploy notifies. Keeping the file `0600`/`root:root` means even the `crm` user cannot read the topic token; `deploy-artifact.sh` `source`s it while running as root.

## Phase 6 — Quadlet unit paths (what `deploy-artifact.sh` rewrites)

`deploy-artifact.sh` swaps the running images by editing the `Image=` line of the live Quadlet unit files, then `daemon-reload` + restart. These files are owned by `crm`, so the script edits them AS THE CRM USER (a root edit would leave a root-owned file the crm-user systemd may refuse/mis-permission). The exact paths:

```text
/var/lib/personalcrm/.config/containers/systemd/personalcrm-backend.container
/var/lib/personalcrm/.config/containers/systemd/personalcrm-frontend.container
```

No install action is needed here — the cutover (`infra/cutover-runbook.md`, Phase 3) already placed these units. This section documents the contract so an operator knows which files the deploy mutates and how:

- The script reads each unit's `Image=` line **before** any rewrite to capture the rollback anchor:
  - If the unit pins a real `:<40-hex-sha>` → the rollback ref is `<repo>:<that-sha>` (deterministic).
  - If the unit pins `:latest` (the A0 first-deploy state, which is MUTABLE) → the script resolves the CURRENTLY-RUNNING image digest (`podman inspect crm-backend --format '{{index .RepoDigests 0}}'`) and pins the rollback ref to the immutable `<repo>@sha256:<digest>`. Every subsequent deploy reads a real `:<sha>` and is naturally deterministic.
- On a forward deploy the script rewrites `Image=` to `<repo>:<sha>` (both units), runs `daemon-reload` as the crm user, and **asserts the rewrite took** (re-reads the `Image=` line and aborts if it does not equal the intended ref) before restarting — a silent no-op `sed` must never ship the old image.
- The crm-user env prefix the script uses for every podman/systemctl op (reproduced here for manual ops):

```bash
sudo -u crm HOME=/var/lib/personalcrm XDG_RUNTIME_DIR=/run/user/$(id -u crm) podman <args>
sudo -u crm HOME=/var/lib/personalcrm XDG_RUNTIME_DIR=/run/user/$(id -u crm) \
  DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/$(id -u crm)/bus systemctl --user <args>
```

The migration step runs crm-admin from the NEW image (the image ENTRYPOINT is `crm-api`, so `--entrypoint` is required to run crm-admin instead):

```bash
sudo -u crm HOME=/var/lib/personalcrm XDG_RUNTIME_DIR=/run/user/$(id -u crm) podman run --rm \
  --network crm --env-file /srv/personalcrm/.env -e MIGRATIONS_PATH=/migrations \
  --entrypoint /usr/local/bin/crm-admin ghcr.io/spengrah/personalcrm-backend:<sha> --migrate-check
```

`--migrate-check` exits `0` (up-to-date), `2` (pending), or `1` (operational error — abort). `--migrate` applies pending migrations. The migrate container connects to `crm-postgres` over the `crm` Podman network using the secrets in `/srv/personalcrm/.env` (whose `DATABASE_URL` host is `crm-postgres`). `MIGRATIONS_PATH=/migrations` overrides any stale host `.env` path with the image-baked path.

## Phase 7 — First-deploy + rollback drill

A one-time smoke test the coordinator runs at go-live to validate the deploy automation against real prod. The deploy orchestration cannot be exercised in CI (root-on-Pi), so this drill is its acceptance test. Subscribe to the ntfy topic first so the notifications are observable.

**(a) Benign deploy (happy path).** Promote a SHA with no schema change:

```bash
make promote   # fast-forwards main to develop's HEAD; deploy-prod.yml fires on the main push
```

Verify:
- The `Deploy to Prod` workflow runs on the `[self-hosted, pi]` runner and passes both gates (green CI for the SHA + built images for the SHA).
- An ntfy push arrives: title **`Deploy OK`**, body `Deployed <sha> (migrated=no)` (or `migrated=yes` if this SHA had pending migrations).
- The backend `Image=` line moves from `:latest` (first deploy) to `:<sha>`:

```bash
ssh "$PI_HOST" 'grep ^Image= /var/lib/personalcrm/.config/containers/systemd/personalcrm-backend.container'
```

**(b) First-deploy `:latest`→digest anchor.** The FIRST deploy specifically exercises the mutable-`:latest` rollback-anchor path: the unit starts pinned at `:latest`, so the script resolves the running digest as the rollback ref before re-pinning to `:<sha>`. After (a), confirm the unit no longer pins `:latest` (it now pins `:<sha>`); a forced rollback from this point would re-pin the captured `@sha256:<digest>`, not the mutable `:latest`. No separate command — this is the property to confirm from (a)'s `Image=` check.

**(c) Intentionally-broken deploy (rollback path).** Promote a SHA whose deploy fails the health-gate (e.g. a build that boots but reports unhealthy). Verify:
- An ntfy push arrives: title **`Rolled back`** (priority high), body `<sha> health-gate failed; rolled back to <rollback_ref>`.
- The OLD image is re-pinned BEFORE the app restarts (the rollback re-pins `Image=` to the rollback ref, asserts it took, then starts the app — never the reverse). For a pending-migration deploy, confirm the DB was restored from the snapshot FIRST (unconditionally, with the app stopped), then the OLD image re-pinned, then the app started.
- After the rollback, prod is healthy on the OLD image:

```bash
ssh "$PI_HOST" 'curl -sf http://127.0.0.1:8080/health' | grep -q '"status":[[:space:]]*"healthy"' && echo BACKEND_OK
ssh "$PI_HOST" 'curl -sf http://127.0.0.1:3001 >/dev/null' && echo FRONTEND_OK
ssh "$PI_HOST" 'curl -s -o /dev/null -w "%{http_code}\n" http://localhost:80/api/v1/contacts'   # expect 200
```

After the drill, deploy a known-good SHA to leave prod on a healthy current image.

## Phase 8 — Recovery / manual intervention

The deploy script's rollback is automatic. This section is the LAST-RESORT manual recovery for the one outcome that requires a human: the urgent **`ROLLBACK FAILED — prod degraded`** ntfy (priority urgent, tag `rotating_light`). That notification means the automatic rollback could NOT complete — either the DB restore failed, or the DB was restored but the OLD-image re-pin failed and the app was left STOPPED on purpose (to avoid booting NEW code against the restored OLD schema). The snapshot is ALWAYS retained on a failure path.

**Where snapshots live.** `backup-db.sh` copies the Podman volume `_data` to a sibling `_data.bak-<YYYYMMDD-HHMMSS>` directory:

```bash
ssh "$PI_HOST" 'sudo ls -d /var/lib/personalcrm/.local/share/containers/storage/volumes/personalcrm-db/_data.bak-* 2>/dev/null'
# Resolve the volume path robustly (do not hard-code it):
crm_podman volume inspect personalcrm-db --format '{{.Mountpoint}}'   # → .../volumes/personalcrm-db/_data
```

**Retention policy.** Retain-newest-successful: `deploy-artifact.sh` retains the snapshot taken for a SUCCESSFUL pending-migration deploy as the recovery point, and prunes the PRIOR retained `_data.bak-*` only AFTER the new one is in place. Snapshots are NEVER pruned on a failure path — so after a `ROLLBACK FAILED` the snapshot you need is still on disk.

**Manual recovery — ORDER MATTERS.** The app must be started on the RIGHT image against the restored DB. Always re-pin `Image=` FIRST, then restore + start:

```bash
# 0. Ensure the app is down (so nothing boots the wrong code against the restored DB).
crm_ctl stop personalcrm-backend.service personalcrm-frontend.service

# 1. Re-pin the OLD image on BOTH units (as the crm user, to preserve ownership).
#    Use the rollback ref the ntfy reported (a :<prior-sha> or an @sha256:<digest>).
ssh "$PI_HOST" "sudo -u crm sed -i 's|^Image=.*|Image=ghcr.io/spengrah/personalcrm-backend:<prior-sha>|' \
  /var/lib/personalcrm/.config/containers/systemd/personalcrm-backend.container"
ssh "$PI_HOST" "sudo -u crm sed -i 's|^Image=.*|Image=ghcr.io/spengrah/personalcrm-frontend:<prior-sha>|' \
  /var/lib/personalcrm/.config/containers/systemd/personalcrm-frontend.container"
crm_ctl daemon-reload
# Confirm the rewrite took:
ssh "$PI_HOST" 'grep ^Image= /var/lib/personalcrm/.config/containers/systemd/personalcrm-{backend,frontend}.container'

# 2. Restore the DB from the retained snapshot, leaving the app stopped, then start it.
#    --no-app-start brings Postgres up to pg_isready WITHOUT starting backend/frontend,
#    so you control the image+start. Pass the exact snapshot path (or omit it to use the
#    newest *.bak-* alongside the volume).
ssh "$PI_HOST" 'sudo /usr/local/sbin/restore-db.sh --local --no-app-start \
  /var/lib/personalcrm/.local/share/containers/storage/volumes/personalcrm-db/_data.bak-<ts>'

# 3. Start the app on the now-correct image against the restored DB.
crm_ctl start personalcrm-backend.service personalcrm-frontend.service

# 4. Health-check.
ssh "$PI_HOST" 'curl -sf http://127.0.0.1:8080/health' | grep -q '"status":[[:space:]]*"healthy"' && echo BACKEND_OK
ssh "$PI_HOST" 'curl -s -o /dev/null -w "%{http_code}\n" http://localhost:80/api/v1/contacts'   # expect 200
```

> A standalone `restore-db.sh --local <snapshot>` WITHOUT `--no-app-start` will restore the DB and start backend/frontend on whatever image the units CURRENTLY pin. In a recovery you almost always want `--no-app-start` so you can re-pin the OLD image first (step 1), then start (step 3). Use the bare form only when the units already pin the image you want.

**Down-migrations are manual-only, never automatic.** `deploy-artifact.sh` only ever applies migrations forward (`--migrate`); it never rolls a schema DOWN. The rollback path restores the pre-migrate DB SNAPSHOT (undoing the forward migration physically), not a down-migration. If a true down-migration is ever required, it is a deliberate, human-run operation outside this automation — never wired into the deploy script.
