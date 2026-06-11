#!/bin/bash
# PersonalCRM artifact deploy (rootless Podman Quadlet edition).
#
# Promotes a single, already-built GHCR image (tagged :<sha>) to prod. Runs ON
# THE PI AS ROOT (via one sudoers entry), but EVERY podman/systemctl op against
# the crm runtime runs as the crm user (rootless store), never root's. The only
# genuine-root ops live inside backup-db.sh / restore-db.sh (the cold volume cp).
#
# Flow:
#   validate SHA -> read rollback anchor from the live units -> pull :<sha> ->
#   migrate-check via the NEW image:
#     exit 0 (up-to-date): swap Image=, restart app, health-gate (no DB touched).
#     exit 2 (pending):    stop app -> snapshot -> start DB -> migrate -> swap ->
#                          start app -> health-gate. ANY failure after migrate
#                          succeeds routes to ROLLBACK-WITH-RESTORE.
#     exit 1/other:        abort before touching anything.
#
# Usage: deploy-artifact.sh <40-hex-sha>
#
# Notifications: sourced from /etc/personalcrm/ntfy.env if present (degrade-open
# -- absent file => skip ntfy and continue, so the script stays env-agnostic for
# staging). Bodies carry only the SHA + a fixed reason; NEVER PII or secrets.

set -euo pipefail

# ---------------------------------------------------------------------------
# Configuration (overridable by env for tests / staging; defaults are prod).
# ---------------------------------------------------------------------------
CRM_USER="${CRM_USER:-crm}"
CRM_HOME="${CRM_HOME:-/var/lib/personalcrm}"
QUADLET_DIR="${QUADLET_DIR:-$CRM_HOME/.config/containers/systemd}"
BACKEND_UNIT="${BACKEND_UNIT:-$QUADLET_DIR/personalcrm-backend.container}"
FRONTEND_UNIT="${FRONTEND_UNIT:-$QUADLET_DIR/personalcrm-frontend.container}"
BACKEND_REPO="${BACKEND_REPO:-ghcr.io/spengrah/personalcrm-backend}"
FRONTEND_REPO="${FRONTEND_REPO:-ghcr.io/spengrah/personalcrm-frontend}"
ENV_FILE="${DEPLOY_ENV_FILE:-/srv/personalcrm/.env}"
PODMAN_NETWORK="${PODMAN_NETWORK:-crm}"
MIGRATIONS_PATH="${DEPLOY_MIGRATIONS_PATH:-/migrations}"
BACKUP_SCRIPT="${BACKUP_SCRIPT:-/usr/local/sbin/backup-db.sh}"
RESTORE_SCRIPT="${RESTORE_SCRIPT:-/usr/local/sbin/restore-db.sh}"
NTFY_ENV_FILE="${NTFY_ENV_FILE:-/etc/personalcrm/ntfy.env}"
HEALTH_RETRIES="${HEALTH_RETRIES:-40}"

# ---------------------------------------------------------------------------
# crm-user helpers: ALL podman/systemctl run rootless as the crm user.
# CRM_UID/XDG are resolved in main() AFTER SHA validation (so a bad arg is
# rejected with the documented exit 2 even on a host without the crm user).
# ---------------------------------------------------------------------------
CRM_UID=""
XDG=""

# crm_podman / crm_ctl: rootless ops against the crm-user store. cd /tmp because
# an interactive `sudo -u crm` inherits root's CWD, which crm can't chdir into.
crm_podman() { ( cd /tmp && sudo -u "$CRM_USER" HOME="$CRM_HOME" XDG_RUNTIME_DIR="$XDG" podman "$@" ); }
crm_ctl()    { sudo -u "$CRM_USER" HOME="$CRM_HOME" XDG_RUNTIME_DIR="$XDG" DBUS_SESSION_BUS_ADDRESS="unix:path=$XDG/bus" systemctl --user "$@"; }

# ---------------------------------------------------------------------------
# ntfy: source the env file if present (degrade-open). Two vars: NTFY_URL +
# NTFY_TOPIC. Absent file => ntfy disabled, deploy continues.
# ---------------------------------------------------------------------------
NTFY_ENABLED=false
if [ -f "$NTFY_ENV_FILE" ]; then
    # shellcheck source=/dev/null
    source "$NTFY_ENV_FILE"
    if [ -n "${NTFY_URL:-}" ] && [ -n "${NTFY_TOPIC:-}" ]; then
        NTFY_ENABLED=true
    fi
fi

# ntfy <title> <priority> <tags> <body>. Never logs the topic/URL. A failing POST
# is logged but never changes the deploy outcome.
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

log() { echo "[deploy] $*" >&2; }

# ---------------------------------------------------------------------------
# Pure-ish helpers (unit-tested via PATH stubs).
# ---------------------------------------------------------------------------

# valid_sha: 0 iff arg is exactly 40 lowercase hex chars.
valid_sha() { [[ "$1" =~ ^[0-9a-f]{40}$ ]]; }

# read_image_tag <unit>: echoes the value of the Image= line (e.g.
# "ghcr.io/.../personalcrm-backend:latest"). Empty if not found.
read_image_tag() {
    sed -n 's/^Image=//p' "$1" | head -1
}

# rollback_ref_for <repo> <container> <unit>: resolve the immutable rollback ref.
# If the unit pins :<40-hex-sha> -> repo:<that-sha> (deterministic).
# If it pins :latest (or anything else mutable) -> resolve the RUNNING image
# digest of <container> -> repo@sha256:<digest> (immutable).
rollback_ref_for() {
    local repo="$1" container="$2" unit="$3" image tag digest
    image="$(read_image_tag "$unit")"
    tag="${image##*:}"
    if [[ "$tag" =~ ^[0-9a-f]{40}$ ]]; then
        echo "$repo:$tag"
        return 0
    fi
    # Mutable tag (:latest etc.) -> pin the currently-running digest.
    digest="$(crm_podman inspect "$container" --format '{{index .RepoDigests 0}}')"
    if [ -z "$digest" ]; then
        echo "error: could not resolve running digest for $container" >&2
        return 1
    fi
    # digest is "<repo>@sha256:<hex>"; use it verbatim (already an immutable ref).
    echo "$digest"
}

# pin_image <unit> <ref>: rewrite the Image= line (as the crm user, to preserve
# ownership) then RE-READ and assert it took. Aborts non-zero if the rewrite
# did not land (a silent no-op sed must never ship the wrong image).
pin_image() {
    local unit="$1" ref="$2" got
    sudo -u "$CRM_USER" sed -i "s|^Image=.*|Image=$ref|" "$unit"
    got="$(read_image_tag "$unit")"
    if [ "$got" != "$ref" ]; then
        echo "error: Image= rewrite of $unit did not take (got '$got', want '$ref')" >&2
        return 1
    fi
}

# run_migrate_admin runs crm-admin from the NEW image. The image ENTRYPOINT is
# crm-api, so --entrypoint is REQUIRED to run crm-admin instead. The mandatory
# -e MIGRATIONS_PATH overrides any stale host .env path (same guard as the unit).
run_migrate_admin() {
    local subcmd="$1"  # --migrate-check | --migrate
    crm_podman run --rm \
        --network "$PODMAN_NETWORK" \
        --env-file "$ENV_FILE" \
        -e "MIGRATIONS_PATH=$MIGRATIONS_PATH" \
        --entrypoint /usr/local/bin/crm-admin \
        "$BACKEND_REPO:$SHA" "$subcmd"
}

# ---------------------------------------------------------------------------
# Health gate: reads-only. All three must pass within the retry budget.
# ---------------------------------------------------------------------------
health_gate() {
    local i ok=false body
    # 1. backend /health reports overall healthy (bounded retry ~HEALTH_RETRIES s).
    #    The handler returns HTTP 503 (so `curl -sf` non-zeroes) AND top-level
    #    "status":"degraded" when the DB is unhealthy; 200 + "status":"healthy"
    #    only when the DB component is healthy. Gate on the top-level status (the
    #    DB status is nested under components.database, not a flat key).
    for ((i = 1; i <= HEALTH_RETRIES; i++)); do
        body="$(curl -sf http://127.0.0.1:8080/health 2>/dev/null)" \
            && printf '%s' "$body" | grep -qE '"status":[[:space:]]*"healthy"' \
            && { ok=true; break; }
        sleep 1
    done
    if [ "$ok" != true ]; then
        log "health-gate: backend /health did not report overall status healthy"
        return 1
    fi
    # 2. frontend answers.
    if ! curl -sf http://127.0.0.1:3001 >/dev/null 2>&1; then
        log "health-gate: frontend did not answer on 3001"
        return 1
    fi
    # 3. one authenticated read through Caddy. Plain curl: Caddy injects X-API-Key.
    #    MUST NOT send X-Mac-Host-ID (that bypasses injection -> 401).
    local code
    code="$(curl -s -o /dev/null -w '%{http_code}' http://localhost:80/api/v1/contacts 2>/dev/null || true)"
    if [ "$code" != "200" ]; then
        log "health-gate: authenticated read through Caddy returned $code (want 200)"
        return 1
    fi
    return 0
}

# post_migrate_swap: steps e-g of the pending path. Returns non-zero on the FIRST
# failing step (image swap/assert, daemon-reload, app start, or health gate) so
# the caller routes to ROLLBACK-WITH-RESTORE. Explicit && short-circuiting (not a
# `set -e` subshell) so a failure is never swallowed by the if/! context.
post_migrate_swap() {
    # e. SWAP image, daemon-reload, assert (pin_image asserts the rewrite took).
    pin_image "$BACKEND_UNIT" "$BACKEND_REPO:$SHA" \
        && pin_image "$FRONTEND_UNIT" "$FRONTEND_REPO:$SHA" \
        && crm_ctl daemon-reload \
        && crm_ctl start personalcrm-backend.service personalcrm-frontend.service \
        && health_gate
}

# ---------------------------------------------------------------------------
# Rollback. ROLLBACK-WITH-RESTORE restores the DB UNCONDITIONALLY (app stopped)
# BEFORE re-pinning the OLD image. A restore OR re-pin failure is max-priority
# "ROLLBACK FAILED" with the app left STOPPED and the snapshot retained.
# ---------------------------------------------------------------------------
rollback_with_restore() {
    local reason="$1"
    # Re-entry guard: this function always exits non-zero, which re-fires the EXIT
    # trap. Mark in-progress so the trap doesn't recursively re-invoke us.
    ROLLBACK_IN_PROGRESS=1
    log "ROLLBACK-WITH-RESTORE: $reason"

    # 1. Ensure the app is down. With it stopped, restoring the volume can never
    #    boot the wrong code against the restored DB.
    crm_ctl stop personalcrm-backend.service personalcrm-frontend.service || true

    # 2. RESTORE FIRST (unconditional). --no-app-start: DB restored + up, app NOT
    #    started. This undoes the forward migration regardless of image state.
    if ! "$RESTORE_SCRIPT" --local --no-app-start "$SNAPSHOT"; then
        ntfy "ROLLBACK FAILED — prod degraded" "urgent" "rotating_light" \
            "$SHA deploy failed AND restore/rollback failed; manual intervention required"
        log "ROLLBACK FAILED: restore-db.sh failed; app left STOPPED, snapshot retained"
        exit 1
    fi

    # 3. RE-PIN the OLD image on both units, then assert.
    if ! pin_image "$BACKEND_UNIT" "$BACKEND_ROLLBACK_REF" \
       || ! pin_image "$FRONTEND_UNIT" "$FRONTEND_ROLLBACK_REF" \
       || ! crm_ctl daemon-reload; then
        ntfy "ROLLBACK FAILED — prod degraded" "urgent" "rotating_light" \
            "$SHA DB restored, image re-pin failed, app left STOPPED; manual intervention required"
        log "ROLLBACK FAILED: DB restored but image re-pin failed; app left STOPPED"
        exit 1
    fi

    # 4. Start the app on the OLD image + restored OLD DB.
    crm_ctl start personalcrm-backend.service personalcrm-frontend.service || true
    health_gate || log "rolled-back stack health-gate did not pass (best-effort)"

    ROLLED_BACK=true
    ntfy "Rolled back" "high" "warning" \
        "$SHA $reason; rolled back to $(short_ref "$BACKEND_ROLLBACK_REF")"
    exit 1
}

# prune_prior_snapshot <keep>: after a successful deploy, delete every other
# *.bak-* alongside the volume EXCEPT <keep> (the new recovery point). Never runs
# on a failure path -- the snapshot is only pruned once a newer one is retained.
# Compares BASENAMES, not full paths: <keep> comes from backup-db.sh while the
# glob is re-derived from a separate `volume inspect`, so any path-formatting
# difference (trailing slash, symlink resolution) must not delete the keep dir.
prune_prior_snapshot() {
    local keep="$1" keep_base volume_path d
    keep_base="$(basename "$keep")"
    volume_path="$(crm_podman volume inspect personalcrm-db --format '{{.Mountpoint}}' 2>/dev/null || true)"
    if [ -z "$volume_path" ]; then
        return 0
    fi
    while IFS= read -r d; do
        [ -n "$d" ] || continue
        if [ "$(basename "$d")" != "$keep_base" ]; then
            sudo rm -rf "$d" || log "warning: could not prune old snapshot $d"
        fi
    done < <(sudo bash -c "ls -d ${volume_path}.bak-* 2>/dev/null" || true)
}

# short_ref: truncate a digest ref's hex to the first 12 chars for ntfy
# readability. A plain :<sha> ref is returned unchanged.
short_ref() {
    local ref="$1"
    if [[ "$ref" == *@sha256:* ]]; then
        local prefix="${ref%@sha256:*}" hex="${ref##*@sha256:}"
        echo "$prefix@sha256:${hex:0:12}"
    else
        echo "$ref"
    fi
}

# ---------------------------------------------------------------------------
# EXIT trap: the post-migrate backstop. If we migrated but never reached a clean
# success, run ROLLBACK-WITH-RESTORE for an UNANTICIPATED failure in that region.
# Otherwise (services merely stopped mid-flight) best-effort restart.
# ---------------------------------------------------------------------------
MIGRATED=0
SNAPSHOT=""
DONE=0
ROLLED_BACK=false
APP_STOPPED=0
ROLLBACK_IN_PROGRESS=0

# Invoked indirectly via `trap on_exit EXIT`.
# shellcheck disable=SC2329
on_exit() {
    local rc=$?
    if [ "$rc" -eq 0 ] || [ "$DONE" -eq 1 ]; then
        return
    fi
    if [ "$ROLLBACK_IN_PROGRESS" -eq 1 ]; then
        # A rollback already ran (it exits non-zero, re-firing this trap). Don't
        # recurse -- its own ntfy/exit already reported the outcome.
        return
    fi
    if [ "$MIGRATED" -eq 1 ] && [ "$ROLLED_BACK" != true ]; then
        # Unanticipated failure after a successful migrate -> restore.
        rollback_with_restore "unexpected failure after migrate"
    elif [ "$APP_STOPPED" -eq 1 ]; then
        log "error (exit $rc) with app stopped pre-migrate; attempting restart"
        crm_ctl start personalcrm-database.service personalcrm-backend.service personalcrm-frontend.service || true
    fi
}
trap on_exit EXIT

# ---------------------------------------------------------------------------
# Main.
# ---------------------------------------------------------------------------
SHA="${1:-}"

# Validate the SHA arg FIRST (before resolving the crm uid, so a bad arg is
# rejected with exit 2 even on a host that lacks the crm user).
if ! valid_sha "$SHA"; then
    ntfy "Deploy aborted" "high" "warning" "bad SHA argument; no changes applied"
    echo "error: expected a 40-hex SHA, got '${SHA}'" >&2
    DONE=1
    exit 2
fi

# Resolve the crm uid now that the arg is valid.
CRM_UID="$(id -u "$CRM_USER")"
XDG="/run/user/$CRM_UID"

# ROLLBACK ANCHOR: read BEFORE any rewrite, so a rollback can re-pin the exact
# image prod was running (a :<sha> tag verbatim, or a resolved digest for :latest).
log "resolving rollback anchors from live units"
BACKEND_ROLLBACK_REF="$(rollback_ref_for "$BACKEND_REPO" crm-backend "$BACKEND_UNIT")"
FRONTEND_ROLLBACK_REF="$(rollback_ref_for "$FRONTEND_REPO" crm-frontend "$FRONTEND_UNIT")"
log "backend rollback ref: $(short_ref "$BACKEND_ROLLBACK_REF")"
log "frontend rollback ref: $(short_ref "$FRONTEND_ROLLBACK_REF")"

# Pull both :<sha> images (fail fast, no DB touched).
log "pulling $BACKEND_REPO:$SHA and $FRONTEND_REPO:$SHA"
if ! crm_podman pull "$BACKEND_REPO:$SHA" || ! crm_podman pull "$FRONTEND_REPO:$SHA"; then
    ntfy "Deploy aborted" "high" "warning" "$SHA image missing/pull failed; no changes applied"
    log "image pull failed; aborting before touching the DB"
    DONE=1
    exit 1
fi

# MIGRATE-CHECK via the NEW image. Branch on its exit code.
log "running migrate-check via the new image"
set +e
run_migrate_admin --migrate-check
CHECK_RC=$?
set -e
log "migrate-check exit code: $CHECK_RC"

case "$CHECK_RC" in
    0)
        # UP-TO-DATE: no DB work, zero DB downtime.
        log "up-to-date: swapping image and restarting app (DB untouched)"
        pin_image "$BACKEND_UNIT" "$BACKEND_REPO:$SHA"
        pin_image "$FRONTEND_UNIT" "$FRONTEND_REPO:$SHA"
        crm_ctl daemon-reload
        crm_ctl restart personalcrm-backend.service personalcrm-frontend.service
        if health_gate; then
            ntfy "Deploy OK" "default" "white_check_mark" "Deployed $SHA (migrated=no)"
            log "deploy OK (migrated=no)"
            DONE=1
            exit 0
        fi
        # Health failed: re-pin rollback, restart. No DB was touched -> no restore.
        log "health-gate failed on up-to-date path; rolling back image (no DB restore needed)"
        pin_image "$BACKEND_UNIT" "$BACKEND_ROLLBACK_REF"
        pin_image "$FRONTEND_UNIT" "$FRONTEND_ROLLBACK_REF"
        crm_ctl daemon-reload
        crm_ctl restart personalcrm-backend.service personalcrm-frontend.service
        # Health-gate the rolled-back stack (best-effort) so we don't report a
        # clean rollback while prod is still unhealthy on the OLD image too.
        health_gate || log "rolled-back stack health-gate did not pass (best-effort)"
        ntfy "Rolled back" "high" "warning" \
            "$SHA health-gate failed; rolled back to $(short_ref "$BACKEND_ROLLBACK_REF")"
        DONE=1
        exit 1
        ;;
    2)
        # PENDING: stop -> snapshot -> start DB -> migrate -> swap -> start -> gate.
        log "pending migrations: stop app, snapshot, migrate"
        # Set APP_STOPPED BEFORE the stop: if the stop itself errors under set -e,
        # the EXIT trap must still know the app may be down and attempt a restart.
        APP_STOPPED=1
        crm_ctl stop personalcrm-backend.service personalcrm-frontend.service

        # b. Snapshot (stops DB, cp -a volume, leaves DB stopped). Capture the
        #    path. Run with set -e OFF so a non-zero backup exit routes to the
        #    explicit abort branch (with ntfy) instead of the bare EXIT trap.
        log "taking pre-migrate snapshot via backup-db.sh"
        set +e
        SNAPSHOT_OUT="$("$BACKUP_SCRIPT" --local --no-restart)"
        BACKUP_RC=$?
        set -e
        SNAPSHOT="$(printf '%s\n' "$SNAPSHOT_OUT" | sed -n 's/^BACKUP_PATH=//p')"
        if [ "$BACKUP_RC" -ne 0 ] || [ -z "$SNAPSHOT" ]; then
            ntfy "Deploy aborted" "high" "warning" "$SHA snapshot failed; no migration applied"
            log "snapshot failed (rc=$BACKUP_RC); aborting (DB not migrated)"
            # Do NOT set DONE: the app is stopped and not migrated, so let the EXIT
            # trap's APP_STOPPED branch restart the stack on the OLD image.
            exit 1
        fi
        log "snapshot: $SNAPSHOT"

        # c. Start postgres only; the migrate container connects over the network.
        log "starting postgres for the migration"
        crm_ctl start personalcrm-database.service
        PG_READY=false
        for ((i = 1; i <= 30; i++)); do
            if crm_podman exec crm-postgres pg_isready -U crm_user >/dev/null 2>&1; then
                PG_READY=true
                break
            fi
            sleep 1
        done
        # If postgres never came up, this is a "DB didn't start" abort, NOT a
        # destructive restore: the DB has not been migrated and the snapshot
        # exists, so just abort before --migrate. The EXIT trap restarts the app.
        if [ "$PG_READY" != true ]; then
            ntfy "Deploy aborted" "high" "warning" "$SHA postgres did not start; no migration applied"
            log "postgres did not become ready; aborting before migrate (snapshot: $SNAPSHOT)"
            # Do NOT set DONE: the app+DB are stopped and not migrated, so let the
            # EXIT trap's APP_STOPPED branch restart the stack on the OLD image.
            exit 1
        fi

        # d. MIGRATE via the NEW image.
        log "applying migrations via the new image"
        if ! run_migrate_admin --migrate; then
            rollback_with_restore "migrate failed — restored"
        fi
        MIGRATED=1

        # e-g: post-migrate region. ANY failure -> ROLLBACK-WITH-RESTORE.
        #      post_migrate_swap returns non-zero on the first failing step. We
        #      can't use a `set -e` subshell here: bash disables `set -e` inside a
        #      subshell that is the operand of `!`/`if`, so a failing step would
        #      be silently swallowed. Explicit `&&` short-circuiting is immune.
        if ! post_migrate_swap; then
            rollback_with_restore "rolled back"
        fi

        # Success: retain this snapshot as the recovery point; prune the PRIOR one.
        log "deploy OK (migrated=yes); retaining snapshot, pruning prior"
        prune_prior_snapshot "$SNAPSHOT"
        ntfy "Deploy OK" "default" "white_check_mark" "Deployed $SHA (migrated=yes)"
        DONE=1
        exit 0
        ;;
    1)
        ntfy "Deploy aborted" "high" "warning" "$SHA migrate-check error; no changes applied"
        log "migrate-check operational error (exit 1); aborting before touching anything"
        DONE=1
        exit 1
        ;;
    *)
        ntfy "Deploy aborted" "high" "warning" "$SHA migrate-check returned $CHECK_RC; no changes applied"
        log "migrate-check returned unexpected $CHECK_RC; aborting before touching anything"
        DONE=1
        exit 1
        ;;
esac
