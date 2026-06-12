#!/usr/bin/env bash
# PersonalCRM Mac deploy setup (one-time, idempotent).
#
# Sets up the SCRIPTABLE parts of the Mac deploy path (Bucket A). The
# operational wiring — registering the [self-hosted, mac] runner, filling in
# deploy.env, pre-authorizing the codesign key — is the runbook's job (Bucket B)
# and is performed separately.
#
# Runs LOCALLY on the Mac, as the logged-in user (NO sudo; the whole Mac deploy
# is userland). Re-running is safe (idempotent): it never re-clones an existing
# clone, never overwrites an existing deploy.env, and bootout-then-bootstraps the
# timer.
#
# This script:
#   - creates the deploy-root skeleton ($DEPLOY_ROOT + bin/);
#   - clones (or fetches) a DEDICATED repo clone the reconcile orchestrator
#     fetches/builds from (kept separate from your dev tree);
#   - installs reconcile-mac-daemon.sh + deploy-mac-daemon.sh +
#     trigger-mac-deploy.sh to the stable bin path the workflow + timer invoke
#     (reconcile self-refreshes them in place);
#   - scaffolds deploy.env (chmod 600, uncommitted) if absent;
#   - renders the timer LaunchAgent from the committed template (substituting
#     __INSTALL_PREFIX__) and records the template's content hash for reconcile's
#     drift detection;
#   - loads the timer ONLY once deploy.env is fully configured (the RunAtLoad
#     timer fires immediately on bootstrap, so loading it before a real codesign
#     identity + ntfy config is set would run a deploy that can't sign and can't
#     notify — see the deferred-load guard below).
#
# After running this (with deploy.env still a scaffold) you must:
#   1. Fill in deploy.env (CRM_MAC_CODESIGN_IDENTITY, NTFY_URL, NTFY_TOPIC).
#   2. Pre-authorize the codesign signing key for non-interactive codesign.
#   3. Re-run `make setup-mac-deploy` to LOAD the timer.
# See infra/mac-runner-installation-runbook.md for the full Bucket-B procedure.

set -euo pipefail

# ---------------------------------------------------------------------------
# Configuration. Defaults MUST match scripts/reconcile-mac-daemon.sh's defaults
# (DEPLOY_ROOT, the stable bin path, deploy.env, the template-hash file) so the
# two never drift. Env-overridable so the mocked test can sandbox them.
# ---------------------------------------------------------------------------
DEPLOY_ROOT="${CRM_MAC_DEPLOY_ROOT:-$HOME/Library/Application Support/crm-mac-deploy}"
CLONE_DIR="${CRM_MAC_CLONE_DIR:-$DEPLOY_ROOT/repo}"
INSTALL_BIN_DIR="${CRM_MAC_INSTALL_BIN_DIR:-$DEPLOY_ROOT/bin}"
DEPLOY_ENV_FILE="${CRM_MAC_DEPLOY_ENV_FILE:-$DEPLOY_ROOT/deploy.env}"
INSTALLED_TEMPLATE_HASH_FILE="${CRM_MAC_INSTALLED_TEMPLATE_HASH_FILE:-$DEPLOY_ROOT/.installed-template-hash}"
# The committed timer template, relative to origin/main (matches reconcile's
# TIMER_TEMPLATE_PATH default so the drift compare lines up).
TIMER_TEMPLATE_PATH="${CRM_MAC_TIMER_TEMPLATE_PATH:-infra/mac-deploy/xyz.spengrah.crm-mac-deploy.plist.template}"
# The rendered (on-disk) LaunchAgent plist. NEVER committed.
LAUNCH_AGENT_LABEL="${CRM_MAC_LAUNCH_AGENT_LABEL:-xyz.spengrah.crm-mac-deploy}"
LAUNCH_AGENT_DIR="${CRM_MAC_LAUNCH_AGENT_DIR:-$HOME/Library/LaunchAgents}"
RENDERED_PLIST="${CRM_MAC_RENDERED_PLIST:-$LAUNCH_AGENT_DIR/$LAUNCH_AGENT_LABEL.plist}"
# Origin URL for the dedicated clone. Defaults to this dev tree's origin so a
# bare `make setup-mac-deploy` Just Works; overridable for tests / re-homing.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
ORIGIN_URL="${CRM_MAC_ORIGIN_URL:-$(git -C "$PROJECT_DIR" remote get-url origin 2>/dev/null || true)}"
# The ref whose content this setup installs + renders. Defaults to origin/main —
# the branch reconcile actually deploys (NOT the remote default branch, which is
# develop; NOT the stale local origin/HEAD). After a successful fetch the script
# checks this ref OUT so steps 2/4 install + render + hash THAT ref's content,
# matching reconcile's `git show origin/main:...` self-refresh. A bare fetch only
# advances remote-tracking refs, so without this an existing clone at an old SHA
# would re-install OLD scripts + render the OLD template.
CRM_MAC_SETUP_REF="${CRM_MAC_SETUP_REF:-origin/main}"

log() { echo "[setup-mac-deploy] $*"; }

# ---------------------------------------------------------------------------
# Step 0: create the directory skeleton FIRST. A clean first run has no
# $DEPLOY_ROOT; without this, the clone + install below fail "no such file or
# directory". `mkdir -p` is idempotent.
# ---------------------------------------------------------------------------
mkdir -p "$DEPLOY_ROOT" "$INSTALL_BIN_DIR"

# ---------------------------------------------------------------------------
# Step 1: create (or fetch) the dedicated clone.
# ---------------------------------------------------------------------------
fetch_ok=false
if [ -d "$CLONE_DIR/.git" ]; then
    log "clone exists at $CLONE_DIR; fetching origin"
    # Soft-skip a failed fetch: the remaining steps (install, render, hash, load)
    # all operate on the already-present local clone, so an offline re-run (e.g.
    # "re-run to load the timer after filling deploy.env") must still complete
    # them rather than aborting under `set -e` on a transient network blip.
    if git -C "$CLONE_DIR" fetch --quiet origin; then
        fetch_ok=true
    else
        log "warning: fetch failed (offline?); continuing with the existing clone"
    fi
else
    if [ -z "$ORIGIN_URL" ]; then
        log "ERROR: could not determine the origin URL to clone from."
        log "Set CRM_MAC_ORIGIN_URL or run from a checkout with an 'origin' remote."
        exit 1
    fi
    log "cloning $ORIGIN_URL -> $CLONE_DIR"
    git clone --quiet "$ORIGIN_URL" "$CLONE_DIR"
    fetch_ok=true
fi

# Advance the clone's WORKING TREE to CRM_MAC_SETUP_REF (default origin/main) so
# steps 2/4 install + render that ref's content — a bare `fetch` only moves
# remote-tracking refs, leaving an existing clone's checkout at an old SHA. A
# fresh clone checks out the remote DEFAULT branch (develop), so it too must be
# advanced to origin/main. Only do this when the remote-tracking refs were just
# refreshed (fetch_ok); on an offline re-run, keep the existing working tree.
if [ "$fetch_ok" = true ]; then
    git -C "$CLONE_DIR" checkout -q --detach "$CRM_MAC_SETUP_REF"
    log "checked out $CRM_MAC_SETUP_REF (install + render use this ref's content)"
else
    log "skipping checkout (offline); using the existing working tree"
fi

# ---------------------------------------------------------------------------
# Step 2: install the reconcile orchestrator + delegated deploy-mac-daemon.sh to
# the stable bin path. The workflow + timer invoke this stable path; reconcile's
# tooling-refresh updates them in place on subsequent runs. install(1) sets the
# mode + does the copy in one step.
# ---------------------------------------------------------------------------
for script in reconcile-mac-daemon.sh deploy-mac-daemon.sh trigger-mac-deploy.sh; do
    install -m 0755 "$CLONE_DIR/scripts/$script" "$INSTALL_BIN_DIR/$script"
    log "installed $script -> $INSTALL_BIN_DIR/$script"
done

# ---------------------------------------------------------------------------
# Step 3: scaffold deploy.env (chmod 600) if absent. NEVER overwrite an existing
# one (it carries the real codesign identity + ntfy config the user filled in).
# Lives OUTSIDE the repo tree, so it needs no .gitignore entry.
# ---------------------------------------------------------------------------
if [ -f "$DEPLOY_ENV_FILE" ]; then
    log "deploy.env exists at $DEPLOY_ENV_FILE; leaving it untouched"
else
    cat > "$DEPLOY_ENV_FILE" <<'EOF'
# crm-mac-deploy configuration (chmod 600; NEVER commit this file).
# Fill in all three before re-running `make setup-mac-deploy` to load the timer.

# Local self-signed Code Signing certificate name (a stable identity keeps the
# designated requirement constant across rebuilds so FDA grants survive). Set to
# "-" to force ad-hoc signing.
CRM_MAC_CODESIGN_IDENTITY=

# ntfy base URL + topic for deploy notifications (the deploy's only failure
# signal). Both must be set for notifications to fire.
NTFY_URL=
NTFY_TOPIC=
EOF
    chmod 600 "$DEPLOY_ENV_FILE"
    log "scaffolded deploy.env at $DEPLOY_ENV_FILE (chmod 600) — fill it in"
fi

# ---------------------------------------------------------------------------
# Step 4: render the timer LaunchAgent from the committed template, substituting
# __INSTALL_PREFIX__ -> the absolute $DEPLOY_ROOT. Record the committed
# template's content hash so reconcile's drift detection can compare. The
# rendered plist is NEVER committed (it carries the absolute, username-bearing
# path).
# ---------------------------------------------------------------------------
TEMPLATE_FILE="$CLONE_DIR/$TIMER_TEMPLATE_PATH"
if [ ! -f "$TEMPLATE_FILE" ]; then
    log "ERROR: timer template not found at $TEMPLATE_FILE"
    exit 1
fi

mkdir -p "$LAUNCH_AGENT_DIR"
# Substitute __INSTALL_PREFIX__ -> $DEPLOY_ROOT (only ever inside <string>
# values). Use a control char (0x01) as the sed delimiter: a filesystem path can
# legitimately contain '/' or '|', but never an embedded control char, so this
# can never collide with the replacement text.
DELIM="$(printf '\001')"
sed "s${DELIM}__INSTALL_PREFIX__${DELIM}${DEPLOY_ROOT}${DELIM}g" \
    "$TEMPLATE_FILE" > "$RENDERED_PLIST"
log "rendered timer plist -> $RENDERED_PLIST"

# Validate the rendered plist (macOS only; plutil is absent on other hosts).
if command -v plutil >/dev/null 2>&1; then
    plutil -lint "$RENDERED_PLIST" >/dev/null
    log "plutil -lint OK"
fi

# Record the committed template's content hash, hashed over EXACTLY the bytes
# the render read. We render from the checked-out working tree (now at
# CRM_MAC_SETUP_REF), so hash the SAME ref's template bytes — NOT a hardcoded
# origin/main — so the render-source and the hash-source can never diverge when
# a caller overrides CRM_MAC_SETUP_REF. (With the default origin/main this is
# exactly the bytes reconcile reads via `git show origin/main:<template>`.)
template_bytes="$(git -C "$CLONE_DIR" show "$CRM_MAC_SETUP_REF:$TIMER_TEMPLATE_PATH" 2>/dev/null || true)"
if [ -n "$template_bytes" ]; then
    printf '%s' "$template_bytes" | shasum -a 256 | awk '{print $1}' > "$INSTALLED_TEMPLATE_HASH_FILE"
    log "recorded template hash -> $INSTALLED_TEMPLATE_HASH_FILE"
else
    # The template is not at the ref yet (e.g. a clone not yet fetched to the SHA
    # that has it). Skip the hash record; reconcile's drift check treats a missing
    # hash as "ambiguous -> silent".
    log "WARNING: timer template not found at $CRM_MAC_SETUP_REF; skipping hash record"
fi

# ---------------------------------------------------------------------------
# Step 5: load the timer ONLY IF deploy.env is FULLY configured. The RunAtLoad
# timer fires reconcile IMMEDIATELY on bootstrap; loading it before deploy.env
# carries a real codesign identity AND ntfy config would (a) run a deploy with an
# empty identity (a failed/ad-hoc build that resets FDA), and/or (b) run blind
# with no notification — the deploy's only failure signal. Require ALL THREE.
# ---------------------------------------------------------------------------
deploy_env_value() {
    # Read a KEY's value from deploy.env without sourcing it into this shell.
    # Strips surrounding quotes; returns the last assignment if duplicated.
    local key="$1" line
    line="$(grep -E "^${key}=" "$DEPLOY_ENV_FILE" 2>/dev/null | tail -n1 || true)"
    line="${line#"${key}"=}"
    line="${line%\"}"
    line="${line#\"}"
    printf '%s' "$line"
}

identity="$(deploy_env_value CRM_MAC_CODESIGN_IDENTITY)"
ntfy_url="$(deploy_env_value NTFY_URL)"
ntfy_topic="$(deploy_env_value NTFY_TOPIC)"

timer_loaded=false
if [ -n "$identity" ] && [ -n "$ntfy_url" ] && [ -n "$ntfy_topic" ]; then
    if ! command -v launchctl >/dev/null 2>&1; then
        log "WARNING: launchctl not found; cannot load the timer (not on macOS?)."
    else
        local_uid="$(id -u)"
        domain="gui/$local_uid"
        # A non-login (no gui domain) context cannot bootstrap a LaunchAgent.
        # Detect + warn rather than silently no-op.
        if ! launchctl print "$domain" >/dev/null 2>&1; then
            log "WARNING: no gui domain for $domain — run this in your login session to load the timer."
        else
            # bootout-then-bootstrap = idempotent reload.
            launchctl bootout "$domain" "$RENDERED_PLIST" 2>/dev/null || true
            launchctl bootstrap "$domain" "$RENDERED_PLIST"
            timer_loaded=true
            log "timer loaded (launchctl bootstrap $domain)"
        fi
    fi
else
    log "timer rendered but NOT loaded — fill in deploy.env (identity + ntfy)"
    log "then re-run \`make setup-mac-deploy\`."
fi

# ---------------------------------------------------------------------------
# Step 6: validate + report.
# ---------------------------------------------------------------------------
echo ""
log "=== setup summary ==="
log "clone:          $CLONE_DIR"
log "reconcile bin:  $INSTALL_BIN_DIR/reconcile-mac-daemon.sh"
log "deploy.env:     $DEPLOY_ENV_FILE"
log "timer plist:    $RENDERED_PLIST"
if [ "$timer_loaded" = true ]; then
    log "timer:          LOADED"
else
    log "timer:          rendered, NOT loaded (deploy.env incomplete or non-login context)"
fi

# The CI gate resolves the repo + reads Actions runs via the user's active `gh`
# account. Probe THAT — the same `gh repo view` reconcile's ci_gate uses — rather
# than `gh auth status`, which false-fails on a multi-account keyring where one
# configured account is unreadable even though the active account works.
if command -v gh >/dev/null 2>&1; then
    if gh repo view "$ORIGIN_URL" --json nameWithOwner >/dev/null 2>&1; then
        log "gh repo access: OK (active account resolves the repo)"
    else
        log "gh repo access: FAILED — run \`gh auth login\` (the CI gate needs repo + Actions read)."
    fi
else
    log "gh repo access: gh not installed — install it (the CI gate needs it)."
fi

echo ""
log "Next (Bucket B — see infra/mac-runner-installation-runbook.md):"
log "  - register the [self-hosted, mac] runner;"
log "  - fill in deploy.env (identity + ntfy) if still a scaffold;"
log "  - pre-authorize the codesign signing key;"
log "  - re-run \`make setup-mac-deploy\` to load the timer."
