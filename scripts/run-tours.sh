#!/bin/bash
# Reset-before-run wrapper for the agentic UX QA tours.
#
# Runs staging-reset.sh (ssh) for a known prod-shaped world BEFORE Playwright
# launches, resolves the deployed staging image digest (read-only ssh, mirroring
# staging-reset.sh's read_image_ref WITHOUT modifying that shared script),
# exports the run env, then launches the dedicated tours config.
#
# Config comes from ENV ONLY (never committed):
#   TOURS_BASE_URL   staging frontend base URL (required)
#   TOURS_API_KEY    staging X-API-Key         (required)
#   TOURS_API_URL    staging backend base URL  (optional; defaults to TOURS_BASE_URL)
#   TOURS_SKIP_RESET=1  skip staging-reset (dev iteration against a seeded staging)
#
# Usage: scripts/run-tours.sh [extra playwright args...]   (or: make tours)

set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"

# --- Required env (fail loud) ---
missing=()
[ -n "${TOURS_BASE_URL:-}" ] || missing+=("TOURS_BASE_URL")
[ -n "${TOURS_API_KEY:-}" ] || missing+=("TOURS_API_KEY")
if [ "${#missing[@]}" -gt 0 ]; then
    echo "run-tours: missing required env: ${missing[*]}" >&2
    echo "run-tours: set TOURS_BASE_URL and TOURS_API_KEY (and optionally TOURS_API_URL) from your env; never commit them." >&2
    exit 1
fi

# --- Reset staging to the prod-shaped world (unless skipped) ---
if [ "${TOURS_SKIP_RESET:-0}" = "1" ]; then
    echo "run-tours: TOURS_SKIP_RESET=1 — skipping staging-reset (using existing seeded staging)" >&2
else
    echo "run-tours: resetting + reseeding staging (prod-shaped)..." >&2
    bash "$REPO_ROOT/scripts/staging-reset.sh"
fi

# --- Resolve the deployed image digest (read-only; best-effort) ---
# Mirror staging-reset.sh's read_image_ref defaults without importing it.
STAGING_HOST="${STAGING_HOST:-stovepipes}"
CRM_USER="${CRM_USER:-staging}"
CRM_HOME="${CRM_HOME:-/var/lib/staging}"
BACKEND_UNIT="${STAGING_BACKEND_UNIT:-$CRM_HOME/.config/containers/systemd/personalcrm-backend.container}"
IMAGE_DIGEST="$(ssh "$STAGING_HOST" "sudo -n -u $CRM_USER sed -n 's/^Image=//p' '$BACKEND_UNIT' 2>/dev/null | head -1" 2>/dev/null || true)"
if [ -z "$IMAGE_DIGEST" ]; then
    echo "run-tours: WARNING — could not read deployed image digest from $BACKEND_UNIT (manifest records 'unknown')" >&2
    IMAGE_DIGEST="unknown"
fi

# --- Export run env consumed by globalSetup + the capture helper ---
export TOURS_RUN_ID="$(date -u +%Y%m%dT%H%M%SZ)"
export TOURS_GIT_SHA="$(git rev-parse HEAD)"
export TOURS_IMAGE_DIGEST="$IMAGE_DIGEST"
export TOURS_SEED_PROFILE="prod-shaped"

echo "run-tours: launching tours (runId=$TOURS_RUN_ID)..." >&2
cd "$REPO_ROOT/frontend"
exec bunx playwright test --config=playwright.tours.config.ts "$@"
