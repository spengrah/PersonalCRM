# Mac Deploy Automation (self-hosted runner) — Design

**Date:** 2026-06-12
**Status:** Bucket A merged to `develop`; Bucket B operational rollout in progress. The deploy TRIGGER was redesigned 2026-06-12 after the runner-session codesign wall surfaced during the on-Mac rollout — see the "Trigger redesign (2026-06-12)" addendum, which supersedes the "runner invokes reconcile directly" descriptions above. Original status: revised 2026-06-12 after a Codex content review (4 critical / 7 major / 5 minor findings folded in; see "Review corrections applied").
**Author:** spengrah (brainstormed with Claude, content-reviewed with Codex)
**Parent:** `2026-06-07-deploy-automation-design.md` (Phase A — Pi prod deploy; landed + live)
**Relationship:** Extends the Phase A self-hosted-runner pattern to the Mac. The same `make promote` (`git push origin develop:main`) that deploys the backend to the Pi now also deploys the mac-daemon to the Mac, where relevant.

## Scope

Promoting `develop` to `main` auto-deploys the mac-daemon (`crm-mac`) to the Mac via a **self-hosted GitHub Actions runner**, gated on green CI, the same way Phase A deploys the backend to the Pi. Because a Mac is a personal laptop that is frequently asleep, powered off, or logged out — unlike the always-on Pi — a **launchd reconcile timer** is added as a long-offline safety net so the Mac eventually converges to `main` whenever it is next logged in. Promotes that do not touch `mac-daemon/` cause no Mac rebuild ("where relevant").

This is almost entirely **ops/scripts wiring**: one idempotent reconcile script, one new CI workflow, one launchd timer, one setup script, one runbook. **No Swift or Go application-code changes** — the daemon's `install --upgrade` / `doctor` / SMAppService lifecycle is reused as-is. The one non-script change is a **build-script tweak** to stamp the built git SHA into the bundle `Info.plist` (needed for drift-proof relevance detection — see below).

## Hard constraints that shape everything

- **Codesign / TCC.** Full Disk Access (the daemon's read of `~/Library/Messages/chat.db`, the Messages source) survives rebuilds **only** because the codesign designated requirement is anchored to a stable local self-signed Code Signing cert whose private key lives **only in the Mac's login Keychain**. This forces "build + sign on the Mac, as the user," and rules out a CI-built artifact (CI can't reproduce that designated requirement without exporting the signing key — rejected, see Out of scope).
- **Contacts re-prompts on every rebuild.** Per the daemon README and `deploy-mac-daemon.sh`, the **iCloud Contacts** grant does **not** survive a rebuild — TCC's Contacts subsystem re-evaluates on CDHash change regardless of the designated requirement, so the system Contacts dialog fires after **every** deploy. Messages/FDA is unaffected. This is an accepted, notified manual step, not a blocker (see "Failure & permission handling"). It is the reason an automated Mac deploy is "unattended for Messages, one-click-for-Contacts," never "fully unattended."
- **gui-domain only.** The daemon runs as a per-user SMAppService LaunchAgent in the gui domain. Everything that manages it (codesign against the login Keychain, `launchctl bootstrap gui/$(id -u)`, SMAppService register) **must run as the logged-in user, in their login session.** Consequence: the deploy runs **only while the user is logged in** (screen-locked is fine if the login Keychain stays unlocked); it does **not** run while logged out or sitting at the pre-login screen after a reboot.

## What already exists (so this design does not rebuild it)

- **`scripts/deploy-mac-daemon.sh`** — builds (`make mac-daemon`), runs `crm-mac install --upgrade`, performs the SMAppService re-register-from-installed-binary workaround, and verifies the registered program path. Refuses first install (pairing is interactive). This is the build+install+verify primitive the reconcile orchestrator delegates to — reconcile does **not** build separately (it would double-build).
- **`crm-mac` lifecycle** — `install --upgrade` (backs up the prior bundle, atomic-renames the new one in, re-registers via SMAppService), `doctor` (line-oriented health: `PASS agent_service: registered (enabled)` etc.), `status`. No daemon code change needed.
- **`__INSTALL_PREFIX__` placeholder pattern** — the daemon's embedded LaunchAgent plist ships with a placeholder substituted at install time. The reconcile timer's plist reuses this to keep absolute paths (and the username) out of git.
- **Phase A Pi deploy** — `deploy-prod.yml` (push:`main` → self-hosted `pi` runner → CI-conclusion gate → `deploy-artifact.sh`). The Mac deploy is a **separate** workflow (see Triggers), not a job added to that file.

## Decisions locked

### Architecture: two triggers, one login-session reconcile core

> **Superseded 2026-06-12 (see the "Trigger redesign" addendum).** The runner no longer invokes `reconcile-mac-daemon.sh` directly — a runner-session job cannot codesign (it runs in an isolated `SessionCreate` session that cannot reach the login Keychain). The runner is now a thin trigger that `launchctl kickstart`s the login-session timer; the timer runs reconcile in the user's login session. The diagram + flow below reflect the current design.

```
push to main ──► deploy-mac.yml ──► deploy-mac job (runs-on: [self-hosted, mac]) ──► trigger-mac-deploy.sh
                                                                                          │ launchctl kickstart
                                                                                          ▼
launchd TIMER (xyz.spengrah.crm-mac-deploy; StartCalendarInterval + RunAtLoad) ──────► reconcile-mac-daemon.sh
   (runs on login + ~6h catch-up; ALSO the kickstart target on promote)                   (the idempotent core,
                                                                                           ALWAYS in the login session)
                                                                                                   │
                  git fetch → CI gate → refresh tooling → relevance gate → upgrade → health → ntfy
```

- The **runner** handles "logged in when the push lands" (picks the job up in seconds) by kickstarting the timer; the kickstart returns as soon as launchd accepts the start, so the Actions run goes green on "trigger sent", not "deploy succeeded" (the real result is on ntfy + the timer's `reconcile-stderr.log`).
- The **timer** is now load-bearing for on-promote deploys (it is the kickstart target) AND remains the long-offline catch-up: it runs on login (`RunAtLoad`) and on a ~6h calendar catch-up, so a "was logged out / powered off / >24h offline" promote still converges to `origin/main` on the next login.
- Reconcile ALWAYS runs in the user's login session (kickstarted by the runner, fired on login, or fired on the schedule), so the two trigger paths converge on the identical execution context — the Mac analog of Phase A's "one idempotent `deploy-artifact.sh`," with the added constraint that codesign requires the login session.

### Reconcile-to-`main`, not per-SHA

Unlike the Pi (which deploys the exact `$GITHUB_SHA`), the Mac reconcile always converges to **current `origin/main` HEAD**. Consequences:

- Several mac-touching promotes that piled up while offline collapse to **one real upgrade + no-ops** (a later/superseding run sees "already at `main`"). This holds whether GitHub supersedes the older pending run (default concurrency behavior) or stacks them — either way they converge to latest.
- The runner job and the timer share one code path with no SHA threading.
- The Mac is not latency-critical, so converge-to-latest is the right semantics. `main` is always CI-green by construction (it fast-forwards only from CI-gated `develop`), and reconcile still verifies the target SHA's CI before building (fail-closed).

### The reconcile orchestrator (`scripts/reconcile-mac-daemon.sh`)

The Mac analog of `deploy-artifact.sh`. Every step is idempotent and fail-closed:

1. **Lock** — an atomic `mkdir`-based lock (macOS has **no `flock` binary**; `mkdir`/`shlock`/`lockf` are the portable options) so the runner-trigger and timer-trigger cannot race; if the lock is held, exit 0.
2. **Fetch** — `git -C "$CLONE_DIR" fetch --quiet origin main`; target SHA = `origin/main` HEAD.
3. **CI gate** — query `ci.yml`'s conclusion for the target SHA via `gh api`. Reconcile always runs in the user's login session now (the runner just kickstarts the timer — it no longer invokes reconcile), so the CI gate ALWAYS uses the user's `gh` keyring auth; there is no runner `GITHUB_TOKEN` path. There is no `gh auth status` precheck (it false-fails on a multi-account keyring where one configured account is unreadable even when the active account works); the `gh repo view` / `gh api` calls classify real auth failures themselves (empty repo / 401|403|404 → surfaced informational notice). Outcomes: `success` → proceed; `failure`/`cancelled` → fail-closed with a failure ntfy; **in-progress / not-found → soft-skip (exit 0, no failure ntfy)** — the next timer tick or promote retries. (A "CI not done yet" result is never treated as a deploy failure.)
4. **Refresh tooling** — atomically swap the installed orchestrator + delegated deploy scripts (and detect timer-plist/template changes) from the clone, effective next run. This runs **before** the relevance gate so fixes to the *deploy machinery itself* are never stranded by a daemon no-op. The relevance gate governs only whether to rebuild the **daemon**. A changed timer-plist template is surfaced via ntfy ("re-run `make setup-mac-deploy`") rather than auto-reloading launchd mid-run.
5. **Relevance gate** ("where relevant") — read the **actually-installed** SHA from the installed bundle's `Info.plist` (`plutil -extract`/`defaults read` of the `CRMBuildSHA` key the build stamps in), then `git diff --quiet <installedSHA> <targetSHA> -- mac-daemon/`. Unchanged → **no-op, exit 0.** Reading the SHA from the bundle (not an external file) means a manual restore/downgrade/ad-hoc install **self-corrects** — an older installed bundle reports its older SHA and the diff re-fires. Missing/unparseable `CRMBuildSHA` (first automated deploy, or a dirty manual build) → treat as "must deploy."
6. **Upgrade** — check out the target SHA into a **throwaway `git worktree`** (so the running orchestrator's own file is never rewritten under itself), then delegate to that worktree's `scripts/deploy-mac-daemon.sh` (single build → `install --upgrade` → SMAppService re-register-from-installed → registered-path verify). Reused so the hard-won SMAppService workaround lives in one place; reconcile does not pre-build.
7. **Health gate** — run `crm-mac doctor` and parse the `agent_service` line **by content** for `registered (enabled)`. Do **not** use doctor's exit code (it equals the count of FAIL lines, so an informational `pi_reachability` FAIL from a Tailscale blip would false-fail). `launchctl print` stays a narrow registered-path guard only, never the primary health contract.
8. **Contacts-pending check** — surface whether the Contacts grant needs re-approval (expected after every rebuild) as an **informational** outcome, not a health failure (Messages/registration is the success contract).
9. **Stamp-by-build + notify** — the build already stamped `CRMBuildSHA` into the installed bundle (step 6), so success needs no separate stamp write. ntfy per outcome (see Notifications). **No notification on no-ops.**
10. **Exit codes** — 0 on success, no-op, and soft-skip; non-zero only on a genuine deploy/health failure (so the runner job shows red and the failure ntfy fires).

### The two triggers

- **`deploy-mac.yml`** (a **separate** workflow, not a job in `deploy-prod.yml` — a Mac job there would inherit the workflow-level `concurrency: deploy-pi` and a 24h-queued Mac run could stall/supersede Pi deploys). `on: push: [main]`, `runs-on: [self-hosted, mac]`, its **own** `concurrency: deploy-mac` with `cancel-in-progress: false` (a new promote queues behind a running deploy; reconcile-to-`main` makes the queued run converge to latest), `environment: production`. **No `actions/checkout`** — it is a thin trigger that runs the installed `trigger-mac-deploy.sh`, which `launchctl kickstart`s the login-session timer (it does NOT invoke reconcile directly — the runner's isolated session cannot codesign). **No `gh` plumbing** — the CI gate now runs only inside the timer-fired reconcile, so the workflow drops `actions: read` + `GH_TOKEN`. **No `sudo`** — the entire Mac deploy is userland. The runner's green check means "the trigger was sent", NOT "the deploy succeeded" (fire-and-forget — see the Trigger redesign addendum).
- **launchd reconcile timer** — a per-user LaunchAgent (`xyz.spengrah.crm-mac-deploy`) using **`StartCalendarInterval`** (the variant Apple documents as catching up a missed fire after wake) plus **`RunAtLoad`** (fires on login, covering reboot/power-off → next login). `StartInterval` is **not** used — contrary to a common assumption it does not coalesce sleep-missed fires. Realistic cadence: "on login + a periodic (~6h) catch-up while logged in," **not** "on every wake." Runs in the user's login session. It is now **load-bearing for on-promote deploys too**: the runner kickstarts it, so it MUST be loaded (via `make setup-mac-deploy`) for the runner to have a target — a not-loaded timer makes the runner job go red. Its plist bakes in `EnvironmentVariables.PATH` (`/opt/homebrew/bin:…`) so the timer-fired reconcile can find `gh` (a LaunchAgent otherwise inherits a PATH that excludes Homebrew).

### Source & isolation

- **Dedicated canonical clone** at a fixed `$HOME`-relative path (`"${HOME}/Library/Application Support/crm-mac-deploy/repo"`), never the developer's `~/Workspaces/PersonalCRM` tree. Builds happen in a throwaway worktree at the target SHA, so the dev tree's branch/dirty state is irrelevant and untouched.
- **`deploy.env`** alongside the clone (`chmod 600`, **not committed**) — codesign identity name + ntfy topic/URL. Same handling as the Pi's `ntfy.env`.
- **No external stamp file** — the installed SHA is read back from the bundle's `Info.plist` (drift-proof, per the relevance gate).
- **Git auth** reuses the user's user-level git credential helper (the runner and timer both run as the user, who already pushes from this Mac). **`gh` auth for the CI gate no longer differs by trigger**: reconcile always runs in the user's login session now (the runner just kickstarts the timer), so the CI gate always uses the user's `gh` keyring auth. The runner's old `GITHUB_TOKEN` / `actions: read` plumbing is removed.

### Paths & PII

The project's no-PII-in-repo rule applies: **no absolute `/Users/<name>/…` paths in git.**

- Committed scripts express all paths as **`$HOME`-relative** (`"${HOME}/Library/Application Support/crm-mac-deploy/…"`), matching the already-committed `deploy-mac-daemon.sh` (`"$HOME/Library/Application Support/crm-mac/crm-mac.app"`).
- The launchd timer plist is the one place needing a literal absolute path (launchd does not expand `$HOME`/`~` in `ProgramArguments`). The **committed template** carries a `__INSTALL_PREFIX__`-style placeholder; `setup-mac-deploy.sh` substitutes the real absolute path at install time into the on-disk plist (never committed).
- Anything genuinely machine-specific goes in the uncommitted `deploy.env`.

### Failure & permission handling (alert-only)

Unlike the Pi, the Mac upgrade has **no irreversible / data-loss step** (no DB migration), and `install --upgrade` already backs up the prior bundle. So the Pi's restore-first auto-rollback machinery is not justified. On a failed health gate: fire a **max-priority ntfy** with the one-command manual-restore hint and leave the backed-up prior bundle in place. (Auto-restore can be bolted on later — the backup already exists.)

**Contacts (accepted manual step).** Every deploy resets the iCloud Contacts grant. Messages/FDA keeps working; the deploy is declared healthy on Messages/registration. Reconcile fires a **distinct, informational** ntfy — "Mac deployed; Contacts re-approval needed, click Allow when next at the Mac" — and Contacts ingestion resumes on the user's next Allow (the daemon auto-fires the system dialog on its next iCloud tick; no menu hunting). Contacts data lags until then; acceptable because iCloud-contact changes are low-frequency.

### Notifications

**ntfy**, reusing the Pi's channel convention, topic/URL from `deploy.env` (never committed). Titled pushes:
- low-priority `Mac deploy OK` (on a real upgrade only),
- informational `Mac deploy OK — Contacts re-approval needed` (the accepted manual step),
- max-priority `Mac deploy FAILED` (carries the manual-restore hint).
No push on no-ops or soft-skips.

### Runner security posture (contrast with the Pi)

The Mac runner is deliberately **simpler** than the Pi's locked-down posture, because the constraints invert:

- **Runs as the logged-in user, in the user's login session** — required so codesign reaches the login-Keychain signing key and SMAppService can manage the gui-domain agent. A dedicated system user (the Pi's `gha-runner` model) cannot do either. The **default `svc.sh` install is not sufficient** (it is not a proven `~/Library/LaunchAgents` GUI-session install); the runbook installs a **custom user LaunchAgent that invokes the runner's `runsvc.sh`** and validates, from an actual job, both `launchctl print gui/$(id -u)` and a real SMAppService operation.
- **No sudoers entry, no root.** Install path is under `~/Library`, launchd is the gui domain, the signing key is the user's. The runner's entire capability is "run build + install as you," which you can already do by hand.
- **Trust boundary:** a self-hosted runner in your user session runs whatever the workflow says, as you — acceptable for a **private, single-author**, PR-gated repo; noted as a conscious choice. (Phase A keeps the repo public-runner-safe: PRs cannot reach self-hosted runners.)
- **Codesign key ACL** must be pre-authorized for non-interactive use (`security set-key-partition-list` / the identity's key ACL allowing `codesign` without a prompt). Caveat: this does **not** unlock a *locked* Keychain — validate after reboot and after screen-lock. Builds run while logged in (login Keychain stays unlocked across a screen-lock by default).

## One-time setup

Mostly operational (Bucket B). Prereqs already true: the daemon is **already paired/installed** (the runner only ever does *upgrades*; first install stays manual via pairing), Xcode present, `jq`/`curl` present (install if missing), `gh` already authed as the user.

1. **Register the Mac as a GH self-hosted runner**, labels `self-hosted, mac`, installed as a **custom user LaunchAgent invoking `runsvc.sh`** (not the default `svc.sh` LaunchDaemon), running in the user login session. Validate gui-domain + SMAppService access from a test job.
2. **`make setup-mac-deploy`** (scriptable parts): create the dedicated clone; install the reconcile orchestrator to a stable path; scaffold `deploy.env` (codesign identity + ntfy); render + load the timer LaunchAgent from its template (placeholder substituted; `StartCalendarInterval` + `RunAtLoad`).
3. **Pre-authorize the codesign signing key** for non-interactive use; validate post-reboot and post-screen-lock.
4. **Supervised dry-run** validating against the real tools — the Phase A go-live lesson: mocked tests don't catch real-tool behavior. Specifically exercise reboot-before-login, login (`RunAtLoad` fire), and screen-locked cases.

## Repo artifacts vs ops wiring

- **Bucket A (committed; built via plan-and-ship):**
  - `scripts/reconcile-mac-daemon.sh` + `scripts/reconcile-mac-daemon.test.sh`
  - **`.github/workflows/deploy-mac.yml`** (new, separate workflow)
  - `scripts/setup-mac-deploy.sh` + `make setup-mac-deploy`
  - committed timer LaunchAgent **template** (placeholder; `StartCalendarInterval` + `RunAtLoad`)
  - `infra/mac-runner-installation-runbook.md`
  - **build-script tweak** to stamp `CRMBuildSHA` (the built git SHA) into the bundle `Info.plist` at assembly time (`mac-daemon/Scripts/assemble_bundle.sh` / the `mac-daemon` Make target) — the only non-script change; **no Swift/Go app code.**
  - minor `scripts/deploy-mac-daemon.sh` tweaks for clean reuse (if needed)
- **Bucket B (operational; coordinator performs on the Mac):** register the runner as a user LaunchAgent (runsvc.sh) and validate gui/SMAppService from a job; run `make setup-mac-deploy`; create `deploy.env`; pre-authorize the codesign key; load the timer; perform the supervised dry-run across login/lock/reboot cases; confirm the daemon is paired.

## Testing

- **`reconcile-mac-daemon.test.sh`** — mocks `git` / `gh` / `crm-mac` / `plutil` / ntfy and asserts: no-op when `mac-daemon/` is unchanged vs the bundle-reported SHA; deploys when it changed; **fail-closed** on CI `failure`; **soft-skip (exit 0, no failure ntfy)** on CI in-progress/not-found; tooling refresh happens even on a daemon no-op; max-priority ntfy on health failure; informational Contacts-pending ntfy on success; the `mkdir` lock prevents concurrent runs; no ntfy on no-ops. Mirrors `deploy-artifact.test.sh`.
- **Mandatory supervised real dry-run** in the runbook — the Phase A go-live surfaced two real bugs a fully-mocked suite passed. Validate the real path, including login/lock/reboot, before going live.
- Pre-push already gates the Swift suite when the range touches `mac-daemon/**`. The `CRMBuildSHA` stamp touches the bundle-assembly path, which the integration test gate (`BundleAssemblyParityTests`) covers — confirm it still passes.

## Out of scope / deferred

- **CI-built signed artifact (no local build).** Rejected: CI can't sign with the local self-signed cert's designated requirement, so a CI-signed bundle resets **FDA** grants every deploy and the daemon goes blind to Messages until re-granted by hand. (FDA is the differentiator here, not Contacts — Contacts already re-prompts under the accepted local-cert path.) The only ways to make CI-signing viable — export the signing key into CI secrets (a secret-exposure downgrade the user declined) or buy an Apple Developer ID — aren't worth it for a personal box. Build-and-sign-on-the-Mac stays.
- **Auto-restore on failed upgrade.** Deferred (alert-only chosen); cheap to add later since the prior bundle is already backed up.
- **Apple Developer ID to make Contacts persist.** Out of scope (paid; the user declined). It would remove the Contacts re-approval step entirely.
- **Auto-reloading the timer LaunchAgent when its template changes.** Reconcile notifies and defers to `make setup-mac-deploy` rather than bootout/bootstrap-ing launchd mid-run.
- **`crm-mac status`/`doctor` reporting the running SHA.** A nicety; reconcile reads `CRMBuildSHA` from the installed `Info.plist` directly, so the daemon needs no change.

## Review corrections applied (Codex content pass, 2026-06-12)

- **Critical:** `StartInterval`→`StartCalendarInterval` + `RunAtLoad` (no on-wake coalescing for `StartInterval`); separate `deploy-mac.yml` (avoid inheriting `deploy-prod.yml`'s workflow concurrency); read installed SHA from the bundle `Info.plist` instead of a drift-prone external stamp file; Contacts re-prompt is an accepted, notified manual step, not "fully unattended."
- **Major:** `mkdir` lock (no `flock` on macOS); pin CI-gate auth to the user's `gh` for both triggers; parse `doctor`'s `agent_service` line by content (its exit code = FAIL count); install the runner as a custom user LaunchAgent via `runsvc.sh` and validate gui/SMAppService from a job; logged-out/pre-login does not run (state + test); refresh deploy tooling before the relevance gate so machinery fixes aren't stranded.
- **Minor:** drop reconcile's redundant pre-build (delegate the single build to `deploy-mac-daemon.sh`); `set-key-partition-list` doesn't unlock a locked Keychain (validate post-reboot/lock); FDA (not Contacts) is the CI-signing differentiator; keep `launchctl print` as a narrow path-guard, not the health contract; CI-not-yet-complete is a soft-skip, not a failure.

## Trigger redesign (2026-06-12)

During the on-Mac operational rollout the shipped `deploy-mac.yml` — which made the `[self-hosted, mac]` runner job invoke `reconcile-mac-daemon.sh` directly — FAILED at `codesign` with `errSecInternalComponent`. Root cause: a GitHub Actions self-hosted-runner job runs inside an isolated security session (the runner's LaunchAgent carries `SessionCreate=true`), which cannot reach the user's **login Keychain** — so `codesign` against the local self-signed signing identity (whose private key lives only in the login Keychain) fails. This is an architecture wall: a runner-session job can never codesign. The launchd **timer** LaunchAgent (`xyz.spengrah.crm-mac-deploy`, `ProcessType=Background`, no `SessionCreate`) runs in the user's real login session and CAN codesign (proven end-to-end during the rollout).

**The fix: the runner stops building and becomes a thin trigger that `launchctl kickstart`s the login-session timer.** The design's central bet is that launchd then runs the timer's job in the TIMER's own context (login session, no `SessionCreate`, with the timer plist's `PATH`) — not as a child of the runner's isolated session — so codesign is EXPECTED to work. This runner-session→login-session crossing is the highest-risk item; it is validated by the Bucket-B supervised dry-run, not proven by this PR's committed unit tests. The timer's existing reconcile does all the real work, exactly as it already does on its scheduled/`RunAtLoad` fires; the two trigger paths then converge on the identical login-session execution context. Specifics:

- **Kickstart trigger** — `scripts/trigger-mac-deploy.sh` (invoked by `deploy-mac.yml`): probe `launchctl print gui/$(id -u)/<label>` first (not-loaded → red with a "run `make setup-mac-deploy`" hint), then `launchctl kickstart` (plain, **no `-k`** — `-k` would kill a mid-codesign build). Kickstart exit 0 → green; non-zero → red with the captured stderr (the benign already-running overlap empirically returns 0, so a non-zero genuinely means launchd refused the start).
- **Fire-and-forget reporting** — kickstart returns as soon as launchd accepts the start, so the runner's green check means "trigger sent", NOT "deploy succeeded". The real deploy result is on **ntfy** + the timer's `reconcile-stdout.log` / `reconcile-stderr.log`. Watch those, not the Actions run status.
- **Logged-out / no-GUI edge → red (loud), with catch-up** — no `gui/$(uid)` domain → the probe fails red. The deploy is not lost: the timer's `RunAtLoad` fires on the next login and converges to `origin/main`. The red surfaces a likely runner mis-install (it should run as the logged-in user); the catch-up still happens.
- **Removed runner `gh` plumbing** — the CI gate now runs only inside the timer-fired reconcile (user gh keyring auth), so `deploy-mac.yml` drops `actions: read` + `GH_TOKEN`.
- **Baked-in `EnvironmentVariables.PATH`** — the timer plist template now carries `PATH=/opt/homebrew/bin:/opt/homebrew/sbin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin` (a literal, untouched by setup's `__INSTALL_PREFIX__` sed) so the timer-fired reconcile's CI gate can find `gh` (a LaunchAgent otherwise inherits a PATH that excludes Homebrew).
- **Removed the `gh auth status` precheck** from `ci_gate()` — it false-failed on a multi-account keyring (a configured account the login session can't read) even when the active account worked. The `gh repo view` / `gh api` calls already classify real auth failures (empty repo / 401|403|404 → ghfailure).
- **`setup-mac-deploy.sh` source-of-truth fix** — a bare `git fetch` advances only remote-tracking refs, so an existing clone at an old SHA re-installed OLD scripts + the OLD template. Setup now checks out `CRM_MAC_SETUP_REF` (default `origin/main` — the branch reconcile deploys, NOT the remote default `develop`) after fetch, so install + render + hash all read it.
- **Required rollout SEQUENCE — promote-first.** This eliminates the old-main-reconcile hazards at the root: setup never runs while `main` is stale. (1) Merge to `develop` → `make promote` (the bootstrap `main` push fires `deploy-mac.yml`; its `[ -x "$TRIGGER" ]` guard fails RED once because the trigger script isn't installed yet — an expected, benign, actionable one-time artifact). (2) `make setup-mac-deploy` on the Mac against the now-new `main`: it installs `trigger-mac-deploy.sh` + the new reconcile, re-renders the PATH template + records its hash, and reloads the timer — whose `RunAtLoad` fire lands the deploy against new `main`. (3) No re-promote needed; future mac-touching promotes use the normal runner-kickstart path. Full procedure: `infra/mac-runner-installation-runbook.md` (Phase 5).

## Risks / notes

- **Runner-session→login-session kickstart crossing** is now the highest-uncertainty item (the Trigger redesign's central bet): `launchctl kickstart gui/$(uid)/<label>` from the runner job must make launchd run the timer's job in the TIMER's login-session context, not the runner's isolated session — proven only by the Bucket-B supervised dry-run. The runner job no longer does SMAppService ops (it only kickstarts), so the old "validate SMAppService from the runner job" check is reframed as a runner-health check, not the deploy path. The `runsvc.sh`-as-user-LaunchAgent runner install must still be validated (gui-domain), not assumed from the default `svc.sh` path.
- **Login Keychain availability** — codesign needs it unlocked; it stays unlocked while logged in (a screen-lock doesn't relock it by default). Pre-authorizing the key ACL removes the per-binary prompt but does not unlock a locked Keychain.
- **Orchestrator self-update** — installed to a stable path, atomically swapped from the worktree each run (effective next run), built in a throwaway worktree so the running file is never rewritten mid-run.
- **First automated deploy bootstraps the stamp** — the currently-installed (manually built) bundle has no `CRMBuildSHA`, so the first reconcile treats it as "must deploy" and stamps it; subsequent runs diff cleanly.
