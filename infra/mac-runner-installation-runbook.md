# Mac Runner Installation — Runbook

One-time, manual install of the self-hosted GitHub Actions **Mac** runner and the userland deploy wiring that turns a `push` to `main` into a `crm-mac` daemon rebuild + reinstall on the Mac host. Design + rationale: `.ai/spec/2026-06-12-mac-deploy-automation-design.md` (parent: `.ai/spec/2026-06-07-deploy-automation-design.md`, the Pi half). This runbook documents the exact, reproducible steps the coordinator performs on the Mac to wire up the automation shipped by `scripts/reconcile-mac-daemon.sh` + `scripts/deploy-mac-daemon.sh` + `scripts/setup-mac-deploy.sh`, the `infra/mac-deploy/xyz.spengrah.crm-mac-deploy.plist.template` timer, and `.github/workflows/deploy-mac.yml`.

Throughout: `$HOME` is the logged-in user's home (NO literal `/Users/<name>/` paths anywhere — the rendered artifacts carry the absolute, username-bearing path but are never committed); `<codesign-identity>` is the name of the local self-signed Code Signing certificate (the value of `CRM_MAC_CODESIGN_IDENTITY`); `<ntfy-url>` and `<ntfy-topic>` are the ntfy base URL + topic (the topic is a capability token — never commit it). Substitute the real values on the Mac.

> **Status:** the coordinator executes the steps below in the user's login session on the Mac (this is "Bucket B" operational wiring). This document is the authoritative, reproducible record of what was done, so a future re-install (new Mac, runner re-registration, disaster recovery) follows the same steps exactly. Bucket A (the committed scripts, the workflow, the timer template, the `CRMBuildSHA` stamp) is already merged to `develop`; this runbook DOCUMENTS Bucket B and performs none of it in code.

## What's committed vs what this runbook does

The committed artifacts (Bucket A, already on `develop`) are inert until the operational wiring below is in place:

| Committed artifact | What it does | What this runbook does |
|---|---|---|
| `mac-daemon/Scripts/assemble_bundle.sh` + `Makefile` `mac-daemon` target | Stamps `CRMBuildSHA=$(git rev-parse HEAD)` into the built bundle's `Contents/Info.plist` under the codesign seal | Nothing — fires automatically inside the reconcile build. Phase 5 verifies the stamp reached the installed bundle. |
| `scripts/reconcile-mac-daemon.sh` | The reconcile-to-`origin/main` orchestrator (lock → fetch → CI gate → tooling refresh → relevance gate → build+install → health gate → notify) | Installed to the stable bin path by `make setup-mac-deploy` (Phase 2); invoked by both the runner workflow and the timer. |
| `scripts/deploy-mac-daemon.sh` | The build+install primitive (`make mac-daemon` → `crm-mac install --upgrade` → SMAppService re-register → registered-path verify) | Installed alongside reconcile; reconcile delegates the single build to it. |
| `scripts/setup-mac-deploy.sh` + `make setup-mac-deploy` | One-time idempotent setup: clone, install the bin scripts, scaffold `deploy.env`, render the timer, deferred-load the timer once `deploy.env` is filled | The runbook RUNS this (Phase 2 + Phase 5). |
| `infra/mac-deploy/xyz.spengrah.crm-mac-deploy.plist.template` | The committed LaunchAgent **timer** template (`__INSTALL_PREFIX__` placeholder; `StartCalendarInterval` + `RunAtLoad`, no `KeepAlive`) | Rendered to `$HOME/Library/LaunchAgents/xyz.spengrah.crm-mac-deploy.plist` by `setup-mac-deploy.sh`; never committed rendered. |
| `.github/workflows/deploy-mac.yml` | The `push: [main]`-triggered workflow that invokes the installed reconcile script on the `[self-hosted, mac]` runner | The runbook REGISTERS the runner this targets (Phase 1). |

Bucket B (this runbook) is: register the Mac as a `[self-hosted, mac]` runner; run `make setup-mac-deploy`; fill `deploy.env`; pre-authorize the codesign key; load the timer; supervised dry-run.

## 0. Overview & security model

The promotion model is identical to the Pi's, pointed at a second self-hosted runner: `make promote` fast-forwards `main` to a reviewed `develop` SHA → `deploy-mac.yml` fires on the `main` push → the self-hosted **Mac** runner invokes `$HOME/Library/Application Support/crm-mac-deploy/bin/reconcile-mac-daemon.sh`, which does its own fetch + CI-conclusion gate + relevance gate, and rebuilds + reinstalls the `crm-mac` daemon ONLY when `mac-daemon/` actually changed since the installed bundle's `CRMBuildSHA`.

**Security posture — INVERTED relative to the Pi.** The Pi runbook goes to great lengths to keep the runner identity (`gha-runner`) distinct from the workload identity (`crm`) and root, gated by a single sudoers line. The Mac does the OPPOSITE, by necessity:

- **The Mac runner runs AS THE LOGGED-IN USER, in the login (gui) session.** This is not a convenience — it is REQUIRED:
  - `codesign` must sign against the user's **login Keychain** (the local self-signed Code Signing certificate lives there). A daemon/system context cannot reach the login Keychain.
  - `SMAppService` registration is a **gui-domain** operation (`launchctl … gui/$(id -u)`). The daemon is a per-user LaunchAgent; registering/booting it out requires the user's gui domain, which only exists inside a login session.
- **NO dedicated system user, NO sudoers, NO root.** There is no `gha-runner`-equivalent account, no `/etc/sudoers.d/` drop-in, no privileged script. The entire Mac deploy capability is "run `make mac-daemon` + `crm-mac install --upgrade` as you." `deploy-mac.yml` has no `sudo` and no checkout; the workflow's whole body is the one `run:` line that invokes the installed reconcile script.
- **Trust boundary.** The CI surface (the runner) IS the user. A compromise of the runner is a compromise of the logged-in account. This is a conscious, documented trade (spec § Runner security posture): acceptable for a **private, single-author, PR-gated** repository where every promoted SHA is already reviewed + CI-green on `develop`. The runner only ever runs reviewed code (`deploy-mac.yml` is `push: [main]`-only — never `develop`, never `pull_request` — so fork/PR code never reaches the Mac runner), and the `production` GitHub Environment enforces the `main`-only rule.
- **The CI gate uses the USER's `gh` auth, not the workflow `GITHUB_TOKEN`.** `reconcile-mac-daemon.sh` queries `ci.yml`'s conclusion for the target SHA via the logged-in user's `gh` CLI. So `deploy-mac.yml` needs no `actions: read` permission, and `gh auth status` is a go-live precondition (Phase 4).

## Phase 1 — Register the Mac as a `[self-hosted, mac]` runner (user LaunchAgent, NOT the default LaunchDaemon)

`deploy-mac.yml` targets `runs-on: [self-hosted, mac]`, so the runner MUST carry the `mac` label. Unlike the Pi (where `svc.sh install` registers a system LaunchDaemon running as a dedicated user), the Mac runner must run **as the logged-in user inside the login session** — so we do NOT use the default `svc.sh` (which installs a root LaunchDaemon). Instead we wire the runner's own `runsvc.sh` entrypoint into a **user LaunchAgent** in `$HOME/Library/LaunchAgents/`, which launchd starts inside the user's gui session.

**Download + configure** — obtain the macOS arm64 runner, register against the repo with the `mac` label:

```bash
# 1. Download + extract the runner into a working dir under $HOME (as the
#    logged-in user — NOT root). Pin to the current macOS arm64 runner release.
mkdir -p "$HOME/actions-runner" && cd "$HOME/actions-runner"
curl -fsSL -o actions-runner.tar.gz \
  https://github.com/actions/runner/releases/download/v<RUNNER_VERSION>/actions-runner-osx-arm64-<RUNNER_VERSION>.tar.gz
tar xzf actions-runner.tar.gz && rm actions-runner.tar.gz

# 2. Register against the repo. Obtain <REGISTRATION_TOKEN> from
#    repo Settings → Actions → Runners → New self-hosted runner (short-lived; do
#    NOT store it anywhere). The `mac` label is the deploy workflow's target.
./config.sh \
  --url https://github.com/spengrah/PersonalCRM \
  --token <REGISTRATION_TOKEN> \
  --labels mac --name mac-runner --unattended
```

**Run as a user LaunchAgent (not `svc.sh`)** — the standard `./svc.sh install` writes a `/Library/LaunchDaemons/` plist that runs at boot, OUTSIDE any login session, with no gui domain and no login Keychain. That breaks both `codesign` and `SMAppService`. Wire `runsvc.sh` (the runner's service entrypoint, which `./run.sh` also calls) into a **user LaunchAgent** instead, so launchd starts the runner inside the gui session:

```bash
# Write a user LaunchAgent that runs the runner's own runsvc.sh from the runner
# dir, inside the login session. RunAtLoad + KeepAlive keeps the agent up.
cat > "$HOME/Library/LaunchAgents/actions.runner.crm-mac.plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>actions.runner.crm-mac</string>
    <key>ProgramArguments</key>
    <array>
        <string>$HOME/actions-runner/runsvc.sh</string>
    </array>
    <key>RunAtLoad</key><true/>
    <key>KeepAlive</key><true/>
    <key>WorkingDirectory</key>
    <string>$HOME/actions-runner</string>
    <key>StandardOutPath</key>
    <string>$HOME/actions-runner/runner-stdout.log</string>
    <key>StandardErrorPath</key>
    <string>$HOME/actions-runner/runner-stderr.log</string>
</dict>
</plist>
EOF

# Load it into the USER's gui domain (this is what makes the runner login-session-scoped).
launchctl bootstrap "gui/$(id -u)" "$HOME/Library/LaunchAgents/actions.runner.crm-mac.plist"
```

Verify the runner is online and login-session-scoped:

```bash
launchctl print "gui/$(id -u)/actions.runner.crm-mac" | grep -E 'state|program'
# Confirm in repo Settings → Actions → Runners that "mac-runner" shows Idle with the `mac` label.
```

**Validate gui-domain + a real SMAppService op from a test job** — the whole point of running as a user LaunchAgent is that the runner job inherits the gui domain. Prove it BEFORE relying on it for a deploy. Run a throwaway workflow (or a `workflow_dispatch` job) on the `[self-hosted, mac]` runner whose only steps are:

```bash
# (a) the gui domain exists for this job (NOT just for an interactive shell):
launchctl print "gui/$(id -u)" >/dev/null && echo "GUI_DOMAIN_OK"

# (b) a real, read-only SMAppService op resolves the installed daemon's state
#     (this exercises the gui-domain + login-Keychain path WITHOUT a deploy):
"$HOME/Library/Application Support/crm-mac/crm-mac.app/Contents/MacOS/crm-mac" status
# expect: installed=true / registered=true / registration_status=enabled
```

If `launchctl print gui/$(id -u)` fails inside the job, the runner is NOT in the gui session (it was installed as a LaunchDaemon, or bootstrapped into the wrong domain) — fix the LaunchAgent wiring before proceeding. A `crm-mac status` that errors with no gui domain is the same signal.

Rationale:

- `--labels mac` is load-bearing — `deploy-mac.yml` targets `runs-on: [self-hosted, mac]`. A runner without `mac` is never picked for a Mac deploy.
- `--unattended` + a registration token from the repo UI keeps the token out of any committed artifact. **Never embed a registration token in this doc** — it is short-lived and fetched fresh from repo Settings each (re-)registration.
- The user-LaunchAgent (not `svc.sh` LaunchDaemon) is the single most important deviation from the Pi: it is what gives every runner job the gui domain + login Keychain that `codesign` and `SMAppService` require.

## Phase 2 — `make setup-mac-deploy` (first pass: scaffolds, does NOT load the timer)

Run `make setup-mac-deploy` in the user's login session, from a checkout of the repo. This is `scripts/setup-mac-deploy.sh`; it runs LOCALLY as the logged-in user (NO sudo) and is idempotent.

```bash
make setup-mac-deploy
```

On a clean first run it:

- creates the deploy-root skeleton `$HOME/Library/Application Support/crm-mac-deploy/` + `bin/`;
- clones the repo into `$HOME/Library/Application Support/crm-mac-deploy/repo` (the dedicated clone reconcile fetches/builds from — kept separate from your dev tree);
- installs `reconcile-mac-daemon.sh` + `deploy-mac-daemon.sh` (mode `0755`) to the **stable bin path** `$HOME/Library/Application Support/crm-mac-deploy/bin/` — **this is the exact path `deploy-mac.yml` invokes** (`$HOME/Library/Application Support/crm-mac-deploy/bin/reconcile-mac-daemon.sh`), so it must not drift;
- scaffolds `deploy.env` (`chmod 600`) at `$HOME/Library/Application Support/crm-mac-deploy/deploy.env` if absent — **never overwriting an existing one** (it carries the codesign identity + ntfy topic);
- renders the timer LaunchAgent from `infra/mac-deploy/xyz.spengrah.crm-mac-deploy.plist.template` to `$HOME/Library/LaunchAgents/xyz.spengrah.crm-mac-deploy.plist` (substituting `__INSTALL_PREFIX__` → the absolute deploy root) and records the committed template's content hash to `$HOME/Library/Application Support/crm-mac-deploy/.installed-template-hash` for reconcile's drift detection;
- **DEFERS loading the timer** because `deploy.env` is a fresh, empty scaffold.

The deferred-load guard is the critical safety property: the timer is `RunAtLoad`, so bootstrapping it would fire reconcile IMMEDIATELY — and with an empty `CRM_MAC_CODESIGN_IDENTITY` that would attempt a deploy that cannot sign (a failed/ad-hoc build that resets FDA) and cannot notify. So the script prints:

```text
[setup-mac-deploy] timer rendered but NOT loaded — fill in deploy.env (identity + ntfy)
[setup-mac-deploy] then re-run `make setup-mac-deploy`.
```

It also reports the `gh auth` status (the CI gate depends on it — see Phase 4). Re-running this step is safe: it does not re-clone, does not overwrite `deploy.env`, re-renders the plist, and only loads the timer once `deploy.env` is fully filled.

## Phase 3 — Fill in `deploy.env` (chmod 600, never committed) + mandatory validation gate

`deploy.env` lives at `$HOME/Library/Application Support/crm-mac-deploy/deploy.env`, OUTSIDE the repo tree, `chmod 600`, and is **NEVER committed** (the ntfy topic is a capability token; the codesign identity name is host-specific). The scaffold defines exactly three variables — fill in ALL THREE with real values:

```text
# crm-mac-deploy configuration (chmod 600; NEVER commit this file).
CRM_MAC_CODESIGN_IDENTITY=<codesign-identity>
NTFY_URL=<ntfy-url>
NTFY_TOPIC=<ntfy-topic>
```

- `CRM_MAC_CODESIGN_IDENTITY` — the name of the local self-signed Code Signing certificate in the login Keychain (the same identity used to sign every `crm-mac` build, so the designated requirement stays constant and FDA grants survive). Reconcile passes it to the delegated `deploy-mac-daemon.sh` build. Set to `-` to force ad-hoc signing (NOT recommended for prod — ad-hoc resets FDA every rebuild).
- `NTFY_URL` / `NTFY_TOPIC` — the deploy's only failure signal. Reconcile degrades-OPEN if absent (no ntfy), so the "notifications happen" guarantee comes from THIS gate, not the script.

**MANDATORY validation gate (mirrors the Pi runbook's PROD-MANDATORY ntfy check)** — confirm the file exists and all three vars are non-empty BEFORE loading the timer (Phase 5). Abort go-live if it fails:

```bash
DEPLOY_ENV="$HOME/Library/Application Support/crm-mac-deploy/deploy.env"
test -f "$DEPLOY_ENV" || { echo "FAIL: deploy.env missing"; exit 1; }
test "$(stat -f '%Lp' "$DEPLOY_ENV")" = "600" || { echo "FAIL: deploy.env is not chmod 600"; exit 1; }
for k in CRM_MAC_CODESIGN_IDENTITY NTFY_URL NTFY_TOPIC; do
  v="$(grep -E "^${k}=" "$DEPLOY_ENV" | tail -n1)"; v="${v#*=}"
  [ -n "$v" ] || { echo "FAIL: $k not set in deploy.env"; exit 1; }
done
echo "deploy.env OK (all three set, 0600)"
```

This MUST precede loading the timer (Phase 5): the deferred-load guard in `setup-mac-deploy.sh` enforces the same all-three rule, but running this check first makes the failure explicit and keeps go-live ordered.

## Phase 4 — Pre-authorize the codesign signing key + `gh auth` precondition

`reconcile-mac-daemon.sh` → `deploy-mac-daemon.sh` → `make mac-daemon` runs `codesign` **non-interactively** (no human at the keyboard during a timer-fired deploy). By default macOS pops a "allow `codesign` to use the signing key" prompt the first time a new process touches a private key in the Keychain. Pre-authorize the key's ACL so non-interactive `codesign` is allowed:

```bash
# Allow codesign (and the Apple toolchain) to use the signing key without a
# per-invocation prompt. Run in the login session; you will be prompted for the
# login-Keychain password ONCE to set the partition list.
security set-key-partition-list \
  -S apple-tool:,apple: \
  -s "$HOME/Library/Keychains/login.keychain-db"
```

**CAVEAT (spec) — this does NOT unlock a LOCKED Keychain.** `set-key-partition-list` removes the per-key authorization prompt, but it does NOT keep the login Keychain unlocked. If the login Keychain is locked when a deploy fires, `codesign` still fails. The login Keychain is unlocked at login and STAYS unlocked while the screen is merely locked, but it can lock on some sleep/lock configurations. So the partition-list grant is necessary but not sufficient — **validate empirically** that a deploy signs successfully:

- after a **reboot + login** (the `RunAtLoad` fire, Phase 5);
- with the **screen locked** (lock the screen, then trigger a reconcile run and confirm the build signs) — the login Keychain should stay unlocked across a screen lock, but confirm it on this machine.

If signing fails post-lock, the login Keychain is auto-locking; resolve it in Keychain Access (the login Keychain's "Lock after / on sleep" settings) rather than weakening the partition-list grant.

**`gh auth status` precondition.** Reconcile's CI gate queries `ci.yml`'s conclusion via the logged-in user's `gh` CLI. If `gh` is not authed (or lacks Actions read access), the CI gate cannot run — reconcile surfaces this as a low-priority informational ntfy (`Mac deploy: CI gate could not be queried`) and skips, rather than deploying blind. Confirm `gh` is authed before go-live:

```bash
gh auth status   # must show "Logged in to github.com" with repo + Actions read access
```

`make setup-mac-deploy` (Phase 2) also reports this; treat a `NOT authed` line as a go-live blocker and run `gh auth login`.

## Phase 5 — Re-run `make setup-mac-deploy` to LOAD the timer, then supervised dry-run

Now that `deploy.env` carries a real identity + ntfy config (validated in Phase 3), re-run setup to LOAD the timer:

```bash
make setup-mac-deploy
```

This second run satisfies the deferred-load guard (all three `deploy.env` vars set), so `setup-mac-deploy.sh` now `launchctl bootout`s any stale timer then `launchctl bootstrap`s `xyz.spengrah.crm-mac-deploy` into `gui/$(id -u)`. It prints `timer: LOADED`. Confirm:

```bash
launchctl print "gui/$(id -u)/xyz.spengrah.crm-mac-deploy" | grep -E 'state|program'
# program → $HOME/Library/Application Support/crm-mac-deploy/bin/reconcile-mac-daemon.sh
```

Because the timer is `RunAtLoad`, bootstrapping it fires reconcile once immediately — this is the first real reconcile run. Subscribe to the ntfy topic first so the notifications are observable.

**Supervised dry-run (the real-tool acceptance test).** Mocked unit tests cannot exercise the real `codesign` / `SMAppService` / `plutil` / `gh` path — and the Phase A Pi go-live lesson is that mocked-green tests let two real bugs through. So the dry-run IS the acceptance test for Bucket B. Exercise these cases:

1. **Reboot-before-login (no run while logged out).** Reboot the Mac and leave it at the login window without logging in. Confirm NO reconcile run fires (a user LaunchAgent only loads inside a login session — there is no gui domain at the login window). No ntfy, no build.
2. **Login (`RunAtLoad` fire).** Log in. The runner LaunchAgent (Phase 1) and the reconcile timer both load; the timer's `RunAtLoad` fires reconcile once. Tail `$HOME/Library/Application Support/crm-mac-deploy/reconcile-stdout.log` and `reconcile-stderr.log` (the rendered timer's `StandardOutPath`/`StandardErrorPath`). On an already-current install it should log a no-op (`no mac-daemon changes since <sha>; no-op`) and exit 0 with no ntfy.
3. **Screen-locked build.** Lock the screen, then trigger a reconcile that actually rebuilds (e.g. promote a SHA that touches `mac-daemon/`, or invoke the installed reconcile script directly). Confirm the login Keychain stays unlocked → `codesign` succeeds → the build + install completes. This validates the Phase 4 partition-list grant under a locked screen.
4. **A real promote-triggered runner job.** Run `make promote` for a SHA that touches `mac-daemon/`. Confirm:
   - the `Deploy Mac Daemon` workflow runs on the `[self-hosted, mac]` runner, with no checkout / no `sudo`, invoking the installed `reconcile-mac-daemon.sh`;
   - reconcile passes the CI-conclusion gate (CI green for the SHA), the relevance gate fires (`mac-daemon/` changed), and `deploy-mac-daemon.sh` rebuilds + reinstalls;
   - the health gate parses `crm-mac doctor`'s `agent_service` line by content (`registered (enabled)`), not the exit code.

**Post-deploy stamp confirmation (the build-stamp real-tool acceptance check).** After a successful real upgrade, prove the `CRMBuildSHA` stamp reached the INSTALLED bundle (the one link no unit test covers — `Bundle.main.infoDictionary` resolving from the build-dir bundle through `install --upgrade`):

```bash
INSTALL_BUNDLE="$HOME/Library/Application Support/crm-mac/crm-mac.app"
plutil -extract CRMBuildSHA raw "$INSTALL_BUNDLE/Contents/Info.plist"
# must print the deployed SHA (== the promoted main SHA, == origin/main)
```

A printed SHA that matches the promoted SHA is empirical proof the build-stamp chain works end-to-end; the relevance gate on the NEXT reconcile then reads this exact value. (A missing/empty result fails SAFE — reconcile treats "no stamp" as "must deploy," an extra rebuild, never a skipped one — but it should NOT be empty after a real deploy.)

**ntfy + Contacts-reprompt verification.** Confirm the success ntfy arrives and the Contacts re-approval prompt behaves as expected:

- On a successful real upgrade, expect ONE combined informational push: title **`Mac deploy OK -- Contacts re-approval needed`**, body `deployed <sha>; click Allow for Contacts when next at the Mac`. (This is a deliberate, documented single-push shape — Contacts re-prompts after EVERY rebuild, a TCC quirk the script cannot observe surviving, so the plain `Mac deploy OK` would only be correct in a state the script cannot detect.)
- On a CI-fail / build-fail / health-fail, expect a max-priority **`Mac deploy FAILED`** push carrying the manual-restore hint.
- After a real rebuild, macOS re-prompts for **iCloud Contacts** (the TCC re-evaluation on CDHash change — see `mac-daemon/README.md`). **Full Disk Access PERSISTS** across rebuilds (the designated requirement is cert-leaf-anchored), so it should NOT re-prompt. Click **Allow** on the Contacts dialog once, at the Mac, to restore the iCloud Contacts source. Messages ingestion + agent registration (the success contract) are unaffected.

## Phase 6 — Recovery / manual intervention

The Mac deploy is **alert-only** — there is no auto-restore on a failed upgrade (spec). The `Mac deploy FAILED` ntfy carries a manual-restore hint; this section expands it.

**The prior bundle is backed up by `crm-mac install --upgrade`.** Per `mac-daemon/README.md` (§ Upgrade), `--upgrade` "backs up the existing bundle, assembles the new one at a tmp path, atomic-renames it into place." So on a failed upgrade the previously-installed, known-good bundle was backed up before the swap; the installed bundle at `$HOME/Library/Application Support/crm-mac/crm-mac.app` is whatever the failed run left. Recovery is to reinstall a known-good build over it — there is no automatic rollback to undo.

**Manual re-run of the build+install from the worktree.** Reconcile builds in a throwaway worktree at `$HOME/Library/Application Support/crm-mac-deploy/worktree` (checked out at the target SHA) and delegates to `deploy-mac-daemon.sh`. To re-run the upgrade manually (e.g. after fixing a transient signing/Keychain issue), do it in the login session with the codesign identity set:

```bash
WORKTREE="$HOME/Library/Application Support/crm-mac-deploy/worktree"
# Source deploy.env to get the same identity reconcile uses (chmod-600 file; do
# not echo its contents). If the worktree was cleaned up by reconcile's trap,
# re-create it at a known-good SHA from the dedicated clone:
CLONE="$HOME/Library/Application Support/crm-mac-deploy/repo"
git -C "$CLONE" worktree add --detach "$WORKTREE" <known-good-sha>

# Run the build+install primitive directly (it runs `make mac-daemon` +
# `crm-mac install --upgrade` + the SMAppService re-register + verify):
set -a; . "$HOME/Library/Application Support/crm-mac-deploy/deploy.env"; set +a
"$WORKTREE/scripts/deploy-mac-daemon.sh"
```

Then re-check health and the stamp:

```bash
INSTALL_BIN="$HOME/Library/Application Support/crm-mac/crm-mac.app/Contents/MacOS/crm-mac"
"$INSTALL_BIN" doctor              # expect PASS agent_service: registered (enabled)
"$INSTALL_BIN" status             # installed=true / registered=true / registration_status=enabled
plutil -extract CRMBuildSHA raw "$HOME/Library/Application Support/crm-mac/crm-mac.app/Contents/Info.plist"
```

`crm-mac doctor`'s exit code equals the number of `FAIL` rows, so an unrelated `pi_reachability` (Tailscale) blip makes the exit code non-zero even when `agent_service` is healthy — judge health by the `agent_service: registered (enabled)` line's CONTENT, exactly as the reconcile health gate does. Clean up the worktree when done:

```bash
git -C "$HOME/Library/Application Support/crm-mac-deploy/repo" worktree remove --force "$WORKTREE"
```

**Re-grant Contacts.** After any rebuild, the iCloud Contacts grant resets (TCC quirk). Let the daemon's automatic prompt drive the regrant (it fires on the next iCloud tick — no menu hunting) and click **Allow** once. If Contacts shows as toggled-on but a dialog still fires, that is the same quirk; allow the prompt. FDA does not need re-granting under cert-backed signing.

**Stale lock.** Reconcile uses an `mkdir`-based lock at `$HOME/Library/Application Support/crm-mac-deploy/reconcile.lock` with automatic stale recovery (dead-PID or TTL-based, default 1h). A crash mid-run is reclaimed on the next fire — no manual `rm` is normally needed. If you must clear it manually (e.g. to force an immediate re-run), confirm no reconcile is actually running first, then `rm -rf "$HOME/Library/Application Support/crm-mac-deploy/reconcile.lock"`.
