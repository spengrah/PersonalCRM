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
| `scripts/trigger-mac-deploy.sh` | The runner-side thin trigger: probes the login-session timer is loaded, then `launchctl kickstart`s it (the timer's reconcile does the real build/codesign/install in the login session) | Installed alongside reconcile by `make setup-mac-deploy`; invoked by the workflow. |
| `.github/workflows/deploy-mac.yml` | The `push: [main]`-triggered workflow that runs the installed `trigger-mac-deploy.sh` on the `[self-hosted, mac]` runner to kickstart the login-session timer (it does NOT invoke reconcile directly — the runner's isolated session cannot codesign) | The runbook REGISTERS the runner this targets (Phase 1). |

Bucket B (this runbook) is: register the Mac as a `[self-hosted, mac]` runner; run `make setup-mac-deploy`; fill `deploy.env`; pre-authorize the codesign key; load the timer; supervised dry-run.

## 0. Overview & security model

The promotion model is the Pi's, pointed at a second self-hosted runner — but with a TRIGGER indirection the Pi does not need: `make promote` fast-forwards `main` to a reviewed `develop` SHA → `deploy-mac.yml` fires on the `main` push → the self-hosted **Mac** runner runs `$HOME/Library/Application Support/crm-mac-deploy/bin/trigger-mac-deploy.sh`, which `launchctl kickstart`s the login-session **timer**. The timer's `reconcile-mac-daemon.sh` (in the user's login session) does its own fetch + CI-conclusion gate + relevance gate, and rebuilds + reinstalls the `crm-mac` daemon ONLY when `mac-daemon/` actually changed since the installed bundle's `CRMBuildSHA`.

**Why the trigger indirection (the codesign wall).** A GitHub Actions self-hosted-runner job runs inside an isolated security session — the runner's LaunchAgent carries `SessionCreate=true` — which cannot reach the user's **login Keychain**. So `codesign` against the local self-signed signing identity (whose private key lives only in the login Keychain) fails with `errSecInternalComponent` from a runner-session job. This is an architecture wall: a runner-session job can never codesign. The login-session **timer** (`xyz.spengrah.crm-mac-deploy`, `ProcessType=Background`, no `SessionCreate`) runs in the user's real login session and CAN codesign. So the runner does NOT build — it kickstarts the timer, and launchd runs the timer's reconcile in the TIMER's login-session context (not as a child of the runner's isolated session), where codesign works. Two contexts, one job each: **runner = isolated session = trigger only; timer = login session = does the real work.**

**Fire-and-forget.** `launchctl kickstart` returns as soon as launchd accepts the start — it does NOT wait for reconcile. So the runner's green check means "the trigger was SENT", NOT "the deploy succeeded". The real deploy result is on **ntfy** + the timer's `reconcile-stdout.log` / `reconcile-stderr.log`. The timer LaunchAgent MUST be loaded (`make setup-mac-deploy`) for kickstart to have a target — it is now **load-bearing for on-promote deploys**, not just the offline catch-up. A not-loaded timer (or no gui session) makes the runner job go red; the deploy is not lost (the timer's `RunAtLoad` converges on next login).

**Security posture — INVERTED relative to the Pi.** The Pi runbook goes to great lengths to keep the runner identity (`gha-runner`) distinct from the workload identity (`crm`) and root, gated by a single sudoers line. The Mac does the OPPOSITE, by necessity:

- **The Mac runner runs AS THE LOGGED-IN USER, in the login (gui) session.** This is not a convenience — it is REQUIRED:
  - `codesign` must sign against the user's **login Keychain** (the local self-signed Code Signing certificate lives there). A daemon/system context cannot reach the login Keychain.
  - `SMAppService` registration is a **gui-domain** operation (`launchctl … gui/$(id -u)`). The daemon is a per-user LaunchAgent; registering/booting it out requires the user's gui domain, which only exists inside a login session.
- **NO dedicated system user, NO sudoers, NO root.** There is no `gha-runner`-equivalent account, no `/etc/sudoers.d/` drop-in, no privileged script. The entire Mac deploy capability is "run `make mac-daemon` + `crm-mac install --upgrade` as you." `deploy-mac.yml` has no `sudo` and no checkout; the workflow's whole body is the one `run:` step that runs the installed `trigger-mac-deploy.sh` to kickstart the login-session timer. The real build+sign+install runs in the timer's login session — the runner job (isolated session) only fires the trigger.
- **Trust boundary.** The CI surface (the runner) IS the user. A compromise of the runner is a compromise of the logged-in account. This is a conscious, documented trade (spec § Runner security posture): acceptable for a **private, single-author, PR-gated** repository where every promoted SHA is already reviewed + CI-green on `develop`. The runner only ever runs reviewed code (`deploy-mac.yml` is `push: [main]`-only — never `develop`, never `pull_request` — so fork/PR code never reaches the Mac runner), and the `production` GitHub Environment enforces the `main`-only rule.
- **The CI gate has ONE `gh`-auth context now: the timer (user keyring).** Reconcile runs only in the user's login session — the runner just kickstarts the timer, it makes NO `gh` calls — so the CI gate always uses the logged-in user's `gh` keyring auth. The runner's old `GITHUB_TOKEN` / `actions: read` plumbing is removed. The go-live precondition is that the active `gh` account resolves the repo + reads Actions, verified with the same `gh repo view` / `gh api` probe the script uses (Phase 4), NOT `gh auth status` (which false-fails on a multi-account keyring).

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

**Validate gui-domain from a test job (runner-health check, NOT the deploy path).** After the trigger redesign the runner job only `launchctl kickstart`s the login-session timer — it does NOT codesign or run SMAppService ops itself (those run in the timer's reconcile, in the login session). So these checks confirm the runner is **online + login-session-scoped**, not that "the runner can deploy" (the deploy path is the kickstart→timer→reconcile crossing, validated in Phase 5). Run a throwaway workflow (or a `workflow_dispatch` job) on the `[self-hosted, mac]` runner whose only steps are:

```bash
# (a) the gui domain exists for this job (NOT just for an interactive shell):
launchctl print "gui/$(id -u)" >/dev/null && echo "GUI_DOMAIN_OK"

# (b) (optional, runner-health only) a read-only SMAppService op resolves the
#     installed daemon's state from the job. NOTE: the runner job no longer needs
#     this for the deploy (the deploy's SMAppService ops run in the timer's
#     reconcile), but it's a useful confirmation the job sees the gui domain:
"$HOME/Library/Application Support/crm-mac/crm-mac.app/Contents/MacOS/crm-mac" status
# expect: installed=true / registered=true / registration_status=enabled
```

If `launchctl print gui/$(id -u)` fails inside the job, the runner is NOT in the gui session (it was installed as a LaunchDaemon, or bootstrapped into the wrong domain) — fix the LaunchAgent wiring before proceeding. This is the same `gui/$(id -u)` domain the kickstart targets: if the runner job can't see it, `trigger-mac-deploy.sh`'s `launchctl print` probe will go red (D4/R2).

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
- installs `reconcile-mac-daemon.sh` + `deploy-mac-daemon.sh` + `trigger-mac-deploy.sh` (mode `0755`) to the **stable bin path** `$HOME/Library/Application Support/crm-mac-deploy/bin/` — **`trigger-mac-deploy.sh` here is the exact path `deploy-mac.yml` invokes** (`$HOME/Library/Application Support/crm-mac-deploy/bin/trigger-mac-deploy.sh`), so it must not drift;
- scaffolds `deploy.env` (`chmod 600`) at `$HOME/Library/Application Support/crm-mac-deploy/deploy.env` if absent — **never overwriting an existing one** (it carries the codesign identity + ntfy topic);
- renders the timer LaunchAgent from `infra/mac-deploy/xyz.spengrah.crm-mac-deploy.plist.template` to `$HOME/Library/LaunchAgents/xyz.spengrah.crm-mac-deploy.plist` (substituting `__INSTALL_PREFIX__` → the absolute deploy root) and records the committed template's content hash to `$HOME/Library/Application Support/crm-mac-deploy/.installed-template-hash` for reconcile's drift detection;
- **DEFERS loading the timer** because `deploy.env` is a fresh, empty scaffold.

The deferred-load guard is the critical safety property: the timer is `RunAtLoad`, so bootstrapping it would fire reconcile IMMEDIATELY — and with an empty `CRM_MAC_CODESIGN_IDENTITY` that would attempt a deploy that cannot sign (a failed/ad-hoc build that resets FDA) and cannot notify. So the script prints:

```text
[setup-mac-deploy] timer rendered but NOT loaded — fill in deploy.env (identity + ntfy)
[setup-mac-deploy] then re-run `make setup-mac-deploy`.
```

It also reports `gh repo access` (whether the active `gh` account resolves the repo — what the CI gate actually probes, see Phase 4). Re-running this step is safe: it does not re-clone, does not overwrite `deploy.env`, advances the clone's working tree to `origin/main` (the source-of-truth fix — so a re-run installs the SAME content reconcile would), re-renders the plist, and only loads the timer once `deploy.env` is fully filled.

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

**`gh` go-live precondition — the targeted probe, NOT `gh auth status`.** Reconcile always runs in the login session now and queries `ci.yml`'s conclusion via the logged-in user's `gh` CLI. If the active `gh` account cannot resolve the repo or read Actions, the CI gate cannot run — reconcile surfaces this as a low-priority informational ntfy (`Mac deploy: CI gate could not be queried`) and skips, rather than deploying blind. Do NOT gate on `gh auth status`: it false-fails on a multi-account keyring where one configured account is unreadable even when the active account works (this is exactly why the script's `gh auth status` precheck was removed). Instead verify the SAME thing `ci_gate()` does:

```bash
# (a) the active account resolves the repo:
gh repo view spengrah/PersonalCRM --json nameWithOwner
# (b) the active account can read Actions runs (the CI-conclusion query):
gh api "repos/spengrah/PersonalCRM/actions/workflows/ci.yml/runs?per_page=1" >/dev/null && echo "ACTIONS_READ_OK"
```

A non-zero on EITHER is the real go-live blocker — run `gh auth login` (and select the account that has repo + Actions read). `make setup-mac-deploy` (Phase 2) reports the `gh repo access` line for the same reason; treat a `FAILED` line as a go-live blocker.

## Phase 5 — Required rollout SEQUENCE (promote-first), then LOAD the timer + supervised dry-run

### The required rollout SEQUENCE for the trigger redesign: PROMOTE FIRST, then setup against new `main`

The kickstart redesign changes the safe rollout order. Reconcile is hardwired to converge to `origin/main`, the timer is `RunAtLoad`, and setup's timer reload fires reconcile immediately — so running setup while `main` is still OLD would fire an old-`main` reconcile that reverts the just-installed scripts. **Inverting the order — promote first, then setup against the now-new `main` — eliminates every old-`main`-reconcile hazard at the root.** Do this once, when landing this redesign:

1. **Merge to `develop`, then `make promote`.** The `main` push fires `deploy-mac.yml`; the runner's `[ -x "$TRIGGER" ]` guard **FAILS RED** because `trigger-mac-deploy.sh` isn't installed in the bin dir yet. **This red is EXPECTED and benign** — a one-time bootstrap artifact: no deploy was attempted, nothing is half-installed, and the log shows the actionable message `trigger script not installed — run make setup-mac-deploy on the Mac` (NOT a deploy failure). No reconcile ran (the guard fails before any kickstart).
   - **The red `deploy-mac` check persists as a failed-run signal but blocks nothing** — the Pi prod deploy is a separate workflow (`deploy-prod.yml`) with its own gating, `main` is not branch-protected on `deploy-mac`, and `make promote` is a plain push. A dashboard glance will show a failed `Deploy Mac Daemon` run; recognize it as this documented bootstrap artifact. After step 2, you may OPTIONALLY re-run the `deploy-mac` workflow from the Actions UI — the trigger script is now installed, so the re-run goes green (it kickstarts the already-deployed timer = a clean no-op) and clears the lingering red. The re-run is cosmetic (the deploy already landed in step 2).
2. **On the Mac, FIRST confirm no reconcile is running, THEN `make setup-mac-deploy`.** Between step 1's promote and now, the already-loaded (old) timer can fire on its `StartCalendarInterval` schedule, and setup's timer reload does `launchctl bootout` then `bootstrap` — booting out a reconcile mid-build/codesign is exactly the "kill mid-codesign" hazard the redesign avoids. So before running setup, verify the reconcile lock dir is absent:

   ```bash
   LOCK="$HOME/Library/Application Support/crm-mac-deploy/reconcile.lock"
   [ ! -d "$LOCK" ] && echo "QUIESCENT — safe to run setup" || echo "RECONCILE RUNNING — wait for it to clear"
   ```

   If the lock is present, confirm via `reconcile-stdout.log` that it's stale, or wait for it to clear (or for the lock's stale-recovery TTL). To minimize the residual TOCTOU (a scheduled fire starting AFTER the lock check but BEFORE setup reaches `bootout`), run setup IMMEDIATELY after the check and avoid the four `StartCalendarInterval` minutes (03/09/15/21:00). Given the ~6h cadence and the supervised one-time nature, this residual is acceptable. Then:

   ```bash
   make setup-mac-deploy
   ```

   With `deploy.env` already filled (Phase 3) and the default `CRM_MAC_SETUP_REF=origin/main` now carrying the new content, setup checks out `origin/main`, **installs `trigger-mac-deploy.sh` + the new `reconcile-mac-daemon.sh`**, re-renders the timer plist (picking up `EnvironmentVariables.PATH`) + records its hash (both from `main`), and reloads the timer. The reload's `RunAtLoad` fires reconcile against **NEW `main`** — it finds `gh` (the reloaded timer carries PATH), passes the CI gate, `refresh_tooling` reads the same new `origin/main` content (a no-op), the relevance gate fires if `mac-daemon/` changed, and **the deploy COMPLETES here**. Verify the trigger is installed + the timer carries PATH, and watch ntfy + `reconcile-stderr.log` for the deploy result:

   ```bash
   [ -x "$HOME/Library/Application Support/crm-mac-deploy/bin/trigger-mac-deploy.sh" ] && echo "TRIGGER INSTALLED"
   launchctl print "gui/$(id -u)/xyz.spengrah.crm-mac-deploy" | grep -E 'state|program'
   # program → $HOME/Library/Application Support/crm-mac-deploy/bin/reconcile-mac-daemon.sh
   plutil -extract EnvironmentVariables.PATH raw "$HOME/Library/LaunchAgents/xyz.spengrah.crm-mac-deploy.plist"
   # → /opt/homebrew/bin:/opt/homebrew/sbin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin
   ```
3. **No re-promote needed** — the deploy landed in step 2's `RunAtLoad` fire against new `main`. Future mac-touching promotes use the normal runner-kickstart path (trigger installed, timer carries PATH).

This inverted order is what makes the rollout SAFE: setup never runs while `main` is stale, so no reconcile ever converges to old content, there is no transient revert / self-heal / race, and no dependency on the removed `gh auth status` precheck. The one-time red on the step-1 bootstrap promote is the documented, expected cost.

> **First-ever install (no prior redesign rollout).** On a brand-new Mac (no runner, no prior timer, no installed bundle) follow Phases 1–4 then run `make setup-mac-deploy` here as the FIRST load — there is no "old timer" to revert, so the promote-first ordering above is the concern only when an OLD installed timer already exists. The deferred-load guard still applies: the timer loads only once `deploy.env` is fully filled.

### Loading the timer (what the setup re-run does)

The setup re-run above satisfies the deferred-load guard (all three `deploy.env` vars set), so `setup-mac-deploy.sh` `launchctl bootout`s any stale timer then `launchctl bootstrap`s `xyz.spengrah.crm-mac-deploy` into `gui/$(id -u)`. It prints `timer: LOADED`. Because the timer is `RunAtLoad`, bootstrapping fires reconcile once immediately. Subscribe to the ntfy topic first so the notifications are observable.

**Supervised dry-run (the real-tool acceptance test).** Mocked unit tests cannot exercise the real `codesign` / `SMAppService` / `plutil` / `gh` path — and the Phase A Pi go-live lesson is that mocked-green tests let two real bugs through. So the dry-run IS the acceptance test for Bucket B. Exercise these cases:

1. **Reboot-before-login (no run while logged out).** Reboot the Mac and leave it at the login window without logging in. Confirm NO reconcile run fires (a user LaunchAgent only loads inside a login session — there is no gui domain at the login window). No ntfy, no build.
2. **Login (`RunAtLoad` fire).** Log in. The runner LaunchAgent (Phase 1) and the reconcile timer both load; the timer's `RunAtLoad` fires reconcile once. Tail `$HOME/Library/Application Support/crm-mac-deploy/reconcile-stdout.log` and `reconcile-stderr.log` (the rendered timer's `StandardOutPath`/`StandardErrorPath`). On an already-current install it should log a no-op (`no mac-daemon changes since <sha>; no-op`) and exit 0 with no ntfy.
3. **Screen-locked build.** Lock the screen, then trigger a reconcile that actually rebuilds (e.g. promote a SHA that touches `mac-daemon/`, or invoke the installed reconcile script directly). Confirm the login Keychain stays unlocked → `codesign` succeeds → the build + install completes. This validates the Phase 4 partition-list grant under a locked screen.
4. **A real promote-triggered runner job (the kickstart crossing — R1, the redesign's central bet).** This is the acceptance test for the runner-session→login-session boundary the local unit tests CANNOT cover. Run `make promote` for a SHA that touches `mac-daemon/`. Confirm:
   - the `Deploy Mac Daemon` workflow runs on the `[self-hosted, mac]` runner, with **no checkout / no `gh` / no `sudo`** — it runs only `trigger-mac-deploy.sh`, which `launchctl print`s + `launchctl kickstart`s the timer. The runner job goes GREEN the instant the trigger is sent (fire-and-forget) — this does NOT mean the deploy succeeded;
   - **the TIMER-fired reconcile** (NOT the runner job) does the real work — observe via **ntfy + `$HOME/Library/Application Support/crm-mac-deploy/reconcile-stderr.log`**: it passes the CI-conclusion gate (CI green for the SHA), the relevance gate fires (`mac-daemon/` changed), `deploy-mac-daemon.sh` rebuilds + reinstalls (codesign SUCCEEDS — proving the kickstart ran the job in the login session, not the runner's isolated session), and the health gate parses `crm-mac doctor`'s `agent_service` line by content (`registered (enabled)`), not the exit code;
   - **if codesign still fails** (the reconcile-stderr.log shows `errSecInternalComponent`), the kickstart ran the job in the runner's session after all — R1 is invalidated and an alternative trigger is needed (e.g. the runner writes a sentinel the timer polls). This is the failure mode to watch for.

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
