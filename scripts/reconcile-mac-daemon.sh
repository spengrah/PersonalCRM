#!/bin/bash
# PersonalCRM Mac daemon reconcile-to-origin/main orchestrator.
#
# The Mac analog of deploy-artifact.sh (the Pi deploy primitive). Runs as the
# LOGGED-IN USER (userland; NO sudo). Invoked by BOTH the runner workflow
# (deploy-mac.yml) and the launchd timer, so the two paths can never diverge.
#
# Flow (every step idempotent + fail-closed):
#   lock (mkdir + stale recovery) -> fetch origin/main -> CI gate (gh) ->
#   refresh installed tooling -> relevance gate (CRMBuildSHA vs target) ->
#   build+install via deploy-mac-daemon.sh in a throwaway worktree ->
#   health gate (crm-mac doctor, parsed by CONTENT) -> notify.
#
# Exit codes:
#   0  success, no-op, or soft-skip (transient: try again next tick).
#   1  genuine CI-fail / build-fail / health-fail (runner job shows red;
#      failure ntfy fires).
#
# Notifications: sourced from $DEPLOY_ENV_FILE if present (degrade-open --
# absent file => no ntfy, never PII or secrets in bodies).
#
# Success-ntfy shape (DOCUMENTED SPEC DEVIATION): a successful real upgrade
# emits ONE combined informational push "Mac deploy OK -- Contacts re-approval
# needed". The spec lists two distinct success pushes (a plain "Mac deploy OK"
# AND the Contacts notice), but Contacts re-prompts after EVERY rebuild (a TCC
# quirk the script cannot observe surviving), so the plain "Mac deploy OK" would
# only ever be correct in a state the script cannot detect. Emitting one combined
# push avoids redundant double-notification on every deploy.

set -euo pipefail

# ---------------------------------------------------------------------------
# Configuration (env-overridable for tests; $HOME-relative defaults).
# Mirror deploy-mac-daemon.sh's "$HOME/Library/Application Support/crm-mac/..."
# style (double-quoted, unbraced $HOME, literal space in "Application Support").
# ---------------------------------------------------------------------------
DEPLOY_ROOT="${CRM_MAC_DEPLOY_ROOT:-$HOME/Library/Application Support/crm-mac-deploy}"
CLONE_DIR="${CRM_MAC_CLONE_DIR:-$DEPLOY_ROOT/repo}"
WORKTREE_DIR="${CRM_MAC_WORKTREE_DIR:-$DEPLOY_ROOT/worktree}"          # throwaway
DEPLOY_ENV_FILE="${CRM_MAC_DEPLOY_ENV_FILE:-$DEPLOY_ROOT/deploy.env}"  # chmod 600, uncommitted
INSTALL_BUNDLE="${CRM_MAC_INSTALL_BUNDLE:-$HOME/Library/Application Support/crm-mac/crm-mac.app}"
LOCK_DIR="${CRM_MAC_LOCK_DIR:-$DEPLOY_ROOT/reconcile.lock}"            # mkdir-based lock
LOCK_STALE_SECS="${CRM_MAC_LOCK_STALE_SECS:-3600}"                     # lock TTL (env-overridable for tests)
# Where the refreshed (self-updating) orchestrator + delegate live. setup-mac-deploy.sh
# installs the stable path; reconcile self-refreshes it BEFORE the relevance gate.
INSTALL_BIN_DIR="${CRM_MAC_INSTALL_BIN_DIR:-$DEPLOY_ROOT/bin}"
# Recorded committed-template hash, written by setup-mac-deploy.sh; reconcile
# compares it to origin/main's template to detect drift (notify-only).
INSTALLED_TEMPLATE_HASH_FILE="${CRM_MAC_INSTALLED_TEMPLATE_HASH_FILE:-$DEPLOY_ROOT/.installed-template-hash}"
TIMER_TEMPLATE_PATH="${CRM_MAC_TIMER_TEMPLATE_PATH:-infra/mac-deploy/xyz.spengrah.crm-mac-deploy.plist.template}"
# Default to the INSTALLED bundle's binary, NOT a bare `crm-mac` on PATH -- the
# health gate must verify the app we just upgraded, and `crm-mac` is not
# guaranteed to be on the runner/timer PATH. Derived from INSTALL_BUNDLE so the
# two never drift; tests override it with a stub.
CRM_MAC_BIN="${CRM_MAC_BIN:-$INSTALL_BUNDLE/Contents/MacOS/crm-mac}"
# Bounded retry budget for the gh CI-conclusion query.
GH_API_RETRIES="${CRM_MAC_GH_API_RETRIES:-3}"

log() { echo "[reconcile] $*" >&2; }

# ---------------------------------------------------------------------------
# deploy.env: source if present (degrade-open). Defines CRM_MAC_CODESIGN_IDENTITY
# (passed to the delegated build), NTFY_URL, NTFY_TOPIC. set -u safety: an ABSENT
# deploy.env means these are never set, so normalize to "" right after the source
# so any later bare reference does not abort the script (unbound variable).
# ---------------------------------------------------------------------------
if [ -f "$DEPLOY_ENV_FILE" ]; then
    # shellcheck source=/dev/null
    source "$DEPLOY_ENV_FILE"
fi
CRM_MAC_CODESIGN_IDENTITY="${CRM_MAC_CODESIGN_IDENTITY:-}"
NTFY_URL="${NTFY_URL:-}"
NTFY_TOPIC="${NTFY_TOPIC:-}"

NTFY_ENABLED=false
if [ -n "$NTFY_URL" ] && [ -n "$NTFY_TOPIC" ]; then
    NTFY_ENABLED=true
fi

# ntfy <title> <priority> <tags> <body>. Never logs the topic/URL. A failing POST
# is logged but never changes the deploy outcome (degrade-open).
ntfy() {
    local title="$1" priority="$2" tags="$3" body="$4"
    if [ "$NTFY_ENABLED" != true ]; then
        return 0
    fi
    if ! curl -fsS \
        -H "Title: $title" -H "Priority: $priority" -H "Tags: $tags" \
        -d "$body" "$NTFY_URL/$NTFY_TOPIC" >/dev/null 2>&1; then
        echo "warning: ntfy POST failed (deploy outcome unchanged)" >&2
    fi
}

# ---------------------------------------------------------------------------
# Lock + worktree cleanup trap. Gated ENTIRELY on lock_created=1 so a concurrent
# LOSER (which never acquired the lock and never created the worktree) does NOT
# tear down the live winner's lock OR mid-build worktree.
# ---------------------------------------------------------------------------
lock_created=0

# Invoked indirectly via `trap`.
# shellcheck disable=SC2329
cleanup() {
    if [ "$lock_created" -eq 1 ]; then
        git -C "$CLONE_DIR" worktree remove --force "$WORKTREE_DIR" 2>/dev/null || true
        rm -rf "$LOCK_DIR" 2>/dev/null || true
    fi
}
trap cleanup EXIT INT TERM HUP

# acquire_lock: atomic mkdir; on contention, recover a stale lock via a two-factor
# (PID-liveness AND age) rule. Returns 0 if we hold the lock (sets lock_created=1),
# 1 if a live concurrent run holds it (caller should exit 0).
acquire_lock() {
    if mkdir "$LOCK_DIR" 2>/dev/null; then
        echo "$$ $(date +%s)" > "$LOCK_DIR/owner"
        lock_created=1
        return 0
    fi

    # Lock already held -- check for a STALE lock before deferring.
    local owner pid ts now age
    owner="$(cat "$LOCK_DIR/owner" 2>/dev/null || true)"
    pid="$(printf '%s\n' "$owner" | awk '{print $1}')"
    ts="$(printf '%s\n' "$owner" | awk '{print $2}')"
    now="$(date +%s)"

    if [ -n "$pid" ] && [ -n "$ts" ] && [[ "$pid" =~ ^[0-9]+$ ]] && [[ "$ts" =~ ^[0-9]+$ ]]; then
        age=$(( now - ts ))
        if kill -0 "$pid" 2>/dev/null; then
            # PID alive.
            if [ "$age" -lt "$LOCK_STALE_SECS" ]; then
                log "lock held by PID $pid (age ${age}s); exiting"
                return 1
            fi
            # PID alive but older than the TTL: no legitimate reconcile runs that
            # long, so this is almost certainly a REUSED PID. Reclaim once.
            log "lock PID $pid alive but age ${age}s >= ${LOCK_STALE_SECS}s (likely reused PID); reclaiming"
        else
            # PID dead -> stale regardless of age. Reclaim once.
            log "lock owner PID $pid is dead; reclaiming stale lock"
        fi
    else
        # Owner file missing/unparseable: fall back to the dir mtime TTL.
        local mtime
        mtime="$(get_mtime "$LOCK_DIR")"
        if [[ "$mtime" =~ ^[0-9]+$ ]]; then
            age=$(( now - mtime ))
            if [ "$age" -lt "$LOCK_STALE_SECS" ]; then
                log "lock held (no owner file, age ${age}s); exiting"
                return 1
            fi
            log "lock owner file missing and dir age ${age}s >= ${LOCK_STALE_SECS}s; reclaiming stale lock"
        else
            # mtime unparseable (no usable stat output) -> cannot compute age.
            # Safe direction is to RECLAIM (never wedge forever on a lock whose
            # age we can't read); the re-mkdir race still guards against stealing
            # a live winner's lock.
            log "lock owner file missing and dir mtime unreadable; reclaiming stale lock"
        fi
    fi

    # Reclaim ONCE. A real run racing in between -> the re-mkdir fails -> exit 0.
    rm -rf "$LOCK_DIR"
    if mkdir "$LOCK_DIR" 2>/dev/null; then
        echo "$$ $(date +%s)" > "$LOCK_DIR/owner"
        lock_created=1
        return 0
    fi
    log "lost the race to reclaim the stale lock; exiting"
    return 1
}

# get_mtime <path>: epoch mtime, portable across BSD (macOS) and GNU stat.
# Echoes a bare integer epoch on success, or NOTHING (empty) if neither form
# yields a numeric value. BSD `stat -f %m` and GNU `stat -c %Y` are mutually
# exclusive in SYNTAX but NOT in exit code: GNU stat treats `-f %m DIR` as
# `--file-system` over files `%m`+DIR and, for an existing DIR, prints a
# MULTI-LINE filesystem block (first line `  File: ...`) and exits 0 — so the
# `||` fallback never fires and the garbage would flow into arithmetic and crash
# under `set -u` (`File: unbound variable`). Guard by validating each form's
# output is a single integer before accepting it; emit nothing otherwise so the
# caller can treat it as "unparseable -> safe path".
get_mtime() {
    local out
    out="$(stat -f %m "$1" 2>/dev/null)"
    if [[ "$out" =~ ^[0-9]+$ ]]; then echo "$out"; return 0; fi
    out="$(stat -c %Y "$1" 2>/dev/null)"
    if [[ "$out" =~ ^[0-9]+$ ]]; then echo "$out"; return 0; fi
    return 0  # nothing parseable -> empty output, caller handles
}

# ---------------------------------------------------------------------------
# empty-identity abort: a missing CRM_MAC_CODESIGN_IDENTITY can never sign a
# build. ALWAYS a loud, non-silent abort -- via ntfy when ntfy is configured,
# else via a red exit + stderr log (the "notify when notify-config is in the
# same missing file" contradiction: loud via red-exit+log, not ntfy).
# ---------------------------------------------------------------------------
abort_empty_identity() {
    if [ "$NTFY_ENABLED" = true ]; then
        # deploy.env exists (NTFY configured) but the identity is empty: a
        # half-filled scaffold. Notify, then exit non-zero (loud).
        ntfy "Mac deploy: deploy.env not configured" "max" "warning" \
            "codesign identity empty -- fill CRM_MAC_CODESIGN_IDENTITY in deploy.env"
        log "CRM_MAC_CODESIGN_IDENTITY empty (deploy.env present); aborting"
    else
        # deploy.env wholly absent: ntfy is ALSO unconfigured, so NO push is
        # possible. Log loudly + exit non-zero so the runner job shows red.
        log "CRM_MAC_CODESIGN_IDENTITY empty and deploy.env/ntfy unconfigured; aborting (no ntfy possible)"
    fi
    exit 1
}

# ---------------------------------------------------------------------------
# refresh_tooling: copy the orchestrator + delegate from the clone's origin/main
# tree to their installed stable paths, effective NEXT run. Runs on EVERY
# non-skip path (including a daemon no-op) so a machinery fix is never stranded.
# Atomic same-dir rename over an OPEN file: the running shell keeps the OLD inode
# and executes to completion; new content takes effect next invocation.
# ---------------------------------------------------------------------------
refresh_tooling() {
    local script dest tmp
    mkdir -p "$INSTALL_BIN_DIR"
    for script in reconcile-mac-daemon.sh deploy-mac-daemon.sh; do
        dest="$INSTALL_BIN_DIR/$script"
        tmp="$dest.tmp.$$"
        if git -C "$CLONE_DIR" show "origin/main:scripts/$script" > "$tmp" 2>/dev/null; then
            # Preserve the executable bit: `git show > tmp` writes 0644; a bare mv
            # would brick the next invocation with "permission denied".
            chmod 0755 "$tmp"
            mv -f "$tmp" "$dest"
        else
            rm -f "$tmp" 2>/dev/null || true
            log "warning: could not refresh $script from origin/main"
        fi
    done

    check_timer_template_drift
}

# check_timer_template_drift: best-effort notify-only. Compare origin/main's timer
# template to the hash recorded at setup time. Differ -> informational ntfy + do
# NOT auto-reload launchd (out of scope). Ambiguous (no recorded hash) -> silent.
check_timer_template_drift() {
    local template_content committed stored
    # Capture the committed template SEPARATELY from hashing: piping a failed
    # `git show` straight into shasum would hash the EMPTY string (a non-empty
    # digest), defeating the "template absent" guard. The template does not exist
    # at origin/main until PR3 lands it, so this guard must hold.
    template_content="$(git -C "$CLONE_DIR" show "origin/main:$TIMER_TEMPLATE_PATH" 2>/dev/null || true)"
    if [ -z "$template_content" ]; then
        return 0  # template not present at origin/main -> nothing to compare
    fi
    if [ ! -f "$INSTALLED_TEMPLATE_HASH_FILE" ]; then
        return 0  # ambiguous (no recorded hash) -> skip silently, do not spam
    fi
    committed="$(printf '%s' "$template_content" | hash_stdin)"
    stored="$(cat "$INSTALLED_TEMPLATE_HASH_FILE" 2>/dev/null || true)"
    if [ -n "$stored" ] && [ "$committed" != "$stored" ]; then
        ntfy "Mac deploy: timer template changed" "default" "information_source" \
            "re-run \`make setup-mac-deploy\` to update the launchd timer"
        log "timer template drift detected (committed != recorded); notified, not reloading launchd"
    fi
}

# hash_stdin: stable content hash of stdin (shasum is present on macOS + Ubuntu).
hash_stdin() {
    shasum -a 256 | awk '{print $1}'
}

# ---------------------------------------------------------------------------
# Steps.
# ---------------------------------------------------------------------------

# Step 1: lock.
if ! acquire_lock; then
    exit 0
fi

# Step 2: fetch -- advance the origin/main REMOTE-TRACKING ref (not just
# FETCH_HEAD), via an EXPLICIT refspec so rev-parse origin/main is never stale.
# Fetch failure -> soft-skip (transient; try again next tick).
if ! git -C "$CLONE_DIR" fetch --quiet origin main:refs/remotes/origin/main 2>/dev/null; then
    log "git fetch failed; soft-skip (try again next tick)"
    exit 0
fi
TARGET_SHA="$(git -C "$CLONE_DIR" rev-parse origin/main 2>/dev/null || true)"
if [ -z "$TARGET_SHA" ]; then
    log "could not resolve origin/main after fetch; soft-skip"
    exit 0
fi

# Step 3: CI gate. Query ci.yml's conclusion for TARGET_SHA via the workflow
# GITHUB_TOKEN (runner path) or the user's gh keyring auth (timer path); gh reads
# whichever is in the environment. Derive REPO with gh (BSD-sed-safe; no hand-rolled regex), anchored to the
# clone's remote URL (reconcile's CWD has no checkout). The status=completed
# filter is load-bearing: an in-progress re-run on the main push must never read
# as success; for a SHA promoted from CI-green develop, the already-completed
# develop run is returned.
ci_gate() {
    # Prints the resolved CI-gate class on stdout (one of):
    #   __resolved__ <conclusion>   the gh query succeeded; <conclusion> is the
    #                               run conclusion or "missing" (zero completed runs)
    #   ghfailure                   persistent/structural (unauthed, empty REPO,
    #                               401/403/404) -> surfaced, not silent
    #   softskip                    auth OK but transport error -> try next tick
    if ! gh auth status >/dev/null 2>&1; then
        echo "ghfailure"
        return 0
    fi

    local origin_url repo
    origin_url="$(git -C "$CLONE_DIR" remote get-url origin 2>/dev/null || true)"
    repo="$(gh repo view "$origin_url" --json nameWithOwner --jq .nameWithOwner 2>/dev/null || true)"
    if [ -z "$repo" ]; then
        # Structural: cannot resolve owner/name -> surfaced, not silent.
        echo "ghfailure"
        return 0
    fi

    local query="repos/$repo/actions/workflows/ci.yml/runs?head_sha=$TARGET_SHA&status=completed&per_page=1"
    local attempt conclusion
    for (( attempt = 1; attempt <= GH_API_RETRIES; attempt++ )); do
        # The happy path: gh resolves + the jq fallback emits the conclusion (or
        # "missing"). A stub can echo the conclusion and exit 0.
        if conclusion="$(gh api "$query" \
                --jq '.workflow_runs[0].conclusion // "missing"' 2>/dev/null)"; then
            echo "__resolved__ ${conclusion:-missing}"
            return 0
        fi
        # gh api returned non-zero. Classify structural (4xx) vs transient (5xx)
        # by probing the HTTP status line via -i. 401/403/404 = structural.
        local status_code
        status_code="$(gh api -i "$query" 2>/dev/null | awk 'NR==1{print $2; exit}')"
        case "$status_code" in
            401|403|404)
                echo "ghfailure"  # token lacks scope / repo|workflow not found
                return 0 ;;
        esac
        sleep 1
    done

    # Auth OK but the query kept failing as a transport error -> transient.
    echo "softskip"
    return 0
}

ci_result="$(ci_gate)"
case "$ci_result" in
    __resolved__*)
        conclusion="${ci_result#__resolved__ }"
        case "$conclusion" in
            success)
                log "CI conclusion success for $TARGET_SHA; proceeding"
                ;;
            failure|cancelled|timed_out|action_required)
                ntfy "Mac deploy FAILED" "max" "rotating_light" \
                    "CI conclusion '$conclusion' for $TARGET_SHA; not deploying"
                log "CI conclusion '$conclusion' for $TARGET_SHA; fail-closed"
                exit 1
                ;;
            missing|*)
                # Zero completed runs (CI still in-progress or not found). Never a
                # deploy failure -- try again next tick.
                log "CI not completed for $TARGET_SHA (conclusion='$conclusion'); soft-skip"
                exit 0
                ;;
        esac
        ;;
    ghfailure)
        # Persistent/structural gh failure (unauthed, or auth OK but
        # misconfigured). SURFACED via a low-priority informational ntfy, NOT a
        # silent forever-skip. Not a deploy failure -- nothing to roll back.
        ntfy "Mac deploy: CI gate could not be queried" "low" "warning" \
            "check \`gh auth status\` and Actions read access"
        log "CI gate could not be queried (gh auth/structural failure); informational notice + exit 0"
        exit 0
        ;;
    softskip|*)
        log "CI gate transient query failure; soft-skip (try again next tick)"
        exit 0
        ;;
esac

# Step 4: refresh installed tooling BEFORE the relevance gate (so machinery fixes
# are never stranded by a daemon no-op).
refresh_tooling

# Step 5: relevance gate. Read the INSTALLED bundle's CRMBuildSHA; skip the
# upgrade only if mac-daemon/ is unchanged between it and the target.
installed_sha="$(plutil -extract CRMBuildSHA raw "$INSTALL_BUNDLE/Contents/Info.plist" 2>/dev/null || true)"
if [ -n "$installed_sha" ] \
   && git -C "$CLONE_DIR" diff --quiet "$installed_sha" "$TARGET_SHA" -- mac-daemon/ 2>/dev/null; then
    log "no mac-daemon changes since $installed_sha; no-op"
    exit 0  # no-op, no ntfy
fi
# Missing/unparseable stamp, or an unknown installed SHA (git diff errors) ->
# treat as "must deploy" (bootstrap path / self-correcting downgrade).

# Empty identity is only reachable here (we are about to build). Loud abort.
if [ -z "$CRM_MAC_CODESIGN_IDENTITY" ]; then
    abort_empty_identity
fi

# Step 6: upgrade -- checkout TARGET_SHA into the throwaway worktree, delegate the
# single build+install to deploy-mac-daemon.sh (which runs `make mac-daemon`,
# stamping CRMBuildSHA=$(git rev-parse HEAD)=TARGET_SHA from inside the worktree).
git -C "$CLONE_DIR" worktree remove --force "$WORKTREE_DIR" 2>/dev/null || true
git -C "$CLONE_DIR" worktree add --detach "$WORKTREE_DIR" "$TARGET_SHA"

if ! CRM_MAC_CODESIGN_IDENTITY="$CRM_MAC_CODESIGN_IDENTITY" \
        "$WORKTREE_DIR/scripts/deploy-mac-daemon.sh"; then
    ntfy "Mac deploy FAILED" "max" "rotating_light" \
        "build/install failed for $TARGET_SHA; prior bundle retained (restore via \`crm-mac install --upgrade\` from a known-good build)"
    log "deploy-mac-daemon.sh failed for $TARGET_SHA; fail-closed"
    exit 1
fi

# Step 7: health gate -- crm-mac doctor, parsed by CONTENT (NOT exit code: the
# exit code equals the FAIL count, so an informational pi_reachability blip would
# false-fail). Capture output FIRST (|| true) so pipefail does not abort us.
doctor_out="$("$CRM_MAC_BIN" doctor 2>/dev/null || true)"
# Extract the agent_service line into a variable, THEN match -- do NOT chain
# `grep -E ... | grep -q ...`: under pipefail, grep -q matching and exiting early
# can SIGPIPE the upstream grep, surfacing a non-zero pipeline status that would
# FALSE-FAIL a genuinely-healthy deploy (the exact fail-closed-when-healthy bug
# this content-parse gate exists to avoid).
agent_line="$(printf '%s\n' "$doctor_out" \
    | grep -E '^(PASS|FAIL)[[:space:]]+agent_service:' || true)"
# Pipeline-free content match (a bash glob, not a piped grep -q) so there is no
# second pipeline whose status pipefail could surface.
if [[ "$agent_line" == *"registered (enabled)"* ]]; then
    health_ok=true
else
    health_ok=false
fi

if [ "$health_ok" != true ]; then
    ntfy "Mac deploy FAILED" "max" "rotating_light" \
        "health gate failed for $TARGET_SHA (agent_service not registered); prior bundle retained (restore via \`crm-mac install --upgrade\` from a known-good build)"
    log "health gate failed for $TARGET_SHA; fail-closed"
    exit 1
fi

# Step 8: Contacts-pending check -- informational only (iCloud Contacts grant is
# reset every rebuild; a TCC quirk). Not a health failure.
# Step 9: notify -- one combined informational success push (see header note).
ntfy "Mac deploy OK -- Contacts re-approval needed" "default" "white_check_mark" \
    "deployed $TARGET_SHA; click Allow for Contacts when next at the Mac"
log "deploy OK for $TARGET_SHA (Contacts re-approval pending)"

exit 0
